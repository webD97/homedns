// Package peercache provides a CoreDNS plugin that races sibling replicas
// against the upstream on a local cache miss, so a name warm on one replica is
// cheap on all of them. See README.md for the Corefile syntax and AGENTS.md for
// the design decisions.
package peercache

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

const (
	pluginName = "peercache"

	sourcePeer     = "peer"
	sourceUpstream = "upstream"

	// probeTimeout bounds a probe goroutine. It is not a latency budget: the
	// upstream leg is already in flight, so a hung peer costs nothing the
	// client can see.
	probeTimeout = 500 * time.Millisecond
)

var log = clog.NewWithPlugin(pluginName)

type PeerCache struct {
	Next plugin.Handler

	selector string
	port     int

	store  *store
	client *dns.Client

	// peers holds podIP:port for every sibling, republished wholesale by the
	// informer so readers never take a lock.
	peers atomic.Pointer[[]string]

	stop context.CancelFunc
	wg   sync.WaitGroup
}

func (p *PeerCache) Name() string { return pluginName }

func (p *PeerCache) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	peers := p.peerList()

	// No siblings discovered: behave exactly as if the plugin were absent,
	// while still recording the answer for whenever one shows up.
	if len(peers) == 0 {
		rec := dnstest.NewRecorder(w)
		rcode, err := plugin.NextOrFailure(p.Name(), p.Next, ctx, rec, r)
		if rec.Msg != nil {
			p.store.put(state, rec.Msg)
		}
		return rcode, err
	}

	return p.race(ctx, w, r, state, peers)
}

type legResult struct {
	source string
	msg    *dns.Msg
	rcode  int
	err    error
}

// race asks every sibling and the upstream at once and takes the first useful
// answer. Losing legs are cancelled and their results dropped.
func (p *PeerCache) race(ctx context.Context, w dns.ResponseWriter, r *dns.Msg, state request.Request, peers []string) (int, error) {
	legs := len(peers) + 1
	results := make(chan legResult, legs)

	for _, addr := range peers {
		go func(addr string) {
			results <- p.probe(ctx, addr, r.Copy())
		}(addr)
	}

	// The upstream leg outlives this function when a peer wins, so it must not
	// hold the live ResponseWriter: the server may cancel the context and reuse
	// the connection the moment ServeDNS returns.
	dw := &detachedWriter{local: w.LocalAddr(), remote: w.RemoteAddr()}
	go func() {
		rcode, err := plugin.NextOrFailure(p.Name(), p.Next, ctx, dw, r.Copy())
		results <- legResult{source: sourceUpstream, msg: dw.msg, rcode: rcode, err: err}
	}()

	var upstream *legResult
	for range legs {
		var res legResult
		select {
		case <-ctx.Done():
			return dns.RcodeServerFailure, ctx.Err()
		case res = <-results:
		}

		if res.source == sourceUpstream {
			last := res
			upstream = &last
		}
		if !useful(res.msg) {
			continue
		}

		winsCount.WithLabelValues(res.source).Inc()
		p.store.put(state, res.msg)
		return p.reply(w, r, res.msg, res.err)
	}

	// Nothing useful anywhere. Hand back whatever the upstream produced so the
	// failure looks exactly as it would without this plugin.
	if upstream == nil {
		return dns.RcodeServerFailure, plugin.Error(pluginName, errors.New("no leg produced an answer"))
	}
	if upstream.msg == nil {
		return upstream.rcode, upstream.err
	}
	return p.reply(w, r, upstream.msg, upstream.err)
}

// reply writes m as the answer to r. The message may have come from a peer
// exchange with its own ID and question, so it is retargeted first.
func (p *PeerCache) reply(w dns.ResponseWriter, r, m *dns.Msg, err error) (int, error) {
	rcode := m.Rcode
	m.SetRcode(r, rcode)
	if werr := w.WriteMsg(m); werr != nil {
		return dns.RcodeServerFailure, werr
	}
	return dns.RcodeSuccess, err
}

// probe asks one sibling whether it already holds the answer.
func (p *PeerCache) probe(ctx context.Context, addr string, r *dns.Msg) legResult {
	probesCount.WithLabelValues("sent").Inc()

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	started := time.Now()
	m, _, err := p.client.ExchangeContext(ctx, r, addr)
	probeDuration.WithLabelValues("sent").Observe(time.Since(started).Seconds())

	switch {
	case errors.Is(err, context.Canceled):
		// Another leg won and the race was torn down. Not a peer failure.
		return legResult{source: sourcePeer}
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		probeFailures.WithLabelValues("timeout").Inc()
		return legResult{source: sourcePeer}
	case err != nil:
		probeFailures.WithLabelValues("error").Inc()
		return legResult{source: sourcePeer}
	}

	return legResult{source: sourcePeer, msg: m}
}

func (p *PeerCache) peerList() []string {
	if v := p.peers.Load(); v != nil {
		return *v
	}
	return nil
}

// useful reports whether a leg's answer can end the race. A refusal from a
// sibling that does not hold the name is the common case, not an error.
func useful(m *dns.Msg) bool {
	if m == nil || m.Truncated {
		return false
	}
	return m.Rcode == dns.RcodeSuccess || m.Rcode == dns.RcodeNameError
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// detachedWriter captures a reply without delegating anything to the live
// ResponseWriter, so a losing leg can finish after ServeDNS has returned.
// plugin/pkg/nonwriter cannot be used here: it embeds the real writer.
type detachedWriter struct {
	local, remote net.Addr
	msg           *dns.Msg
}

func (d *detachedWriter) LocalAddr() net.Addr         { return d.local }
func (d *detachedWriter) RemoteAddr() net.Addr        { return d.remote }
func (d *detachedWriter) WriteMsg(m *dns.Msg) error   { d.msg = m; return nil }
func (d *detachedWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *detachedWriter) Close() error                { return nil }
func (d *detachedWriter) TsigStatus() error           { return nil }
func (d *detachedWriter) TsigTimersOnly(bool)         {}
func (d *detachedWriter) Hijack()                     {}
