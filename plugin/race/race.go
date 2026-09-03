// Package race provides a CoreDNS plugin that sends every query to all of its
// upstreams at once and answers with the first useful reply, so one slow
// resolver never decides the client's latency. See README.md for the Corefile
// syntax and AGENTS.md for the design decisions.
package race

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"syscall"
	"time"

	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/pkg/proxy"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	"github.com/webd97/homedns/internal/detached"
)

const (
	pluginName = "race"

	// hcInterval is the retry cadence of up.Probe once a check has failed.
	// Nothing here ever calls Healthcheck, so no probe traffic is generated;
	// Start is called only because it also starts the transport's connection
	// manager, without which no connection is ever pooled.
	hcInterval = 500 * time.Millisecond

	// maxIdleConns caps how many connections each upstream keeps pooled. race
	// fills a pool faster than forward does, because every query goes to every
	// upstream at once instead of to one of them, and the drain below needs a
	// pool depth it can be bounded by.
	maxIdleConns = 8

	// maxConnectAttempts bounds one leg. Every attempt that finds a dead pooled
	// connection throws exactly that one away, so draining a full pool and then
	// dialling fresh fits inside this.
	maxConnectAttempts = maxIdleConns + 2
)

var (
	log = clog.NewWithPlugin(pluginName)

	errNoAnswer = errors.New("no upstream produced an answer")
	errMismatch = errors.New("upstream answered a different question")
)

// Race asks every upstream at once. It replaces forward rather than sitting in
// front of it, and owns its own proxies.
type Race struct {
	Next plugin.Handler

	from    string
	proxies []*proxy.Proxy

	// opts is left at its zero value: no upstream protocol is forced, so a leg
	// follows the client's. A truncated reply is therefore not retried over TCP
	// but simply loses, and is only ever returned if every upstream truncated —
	// at which point the client retries over TCP itself, as it would under
	// forward.
	opts proxy.Options

	tlsConfig     *tls.Config
	tlsServerName string
	expire        time.Duration
}

func (rc *Race) Name() string { return pluginName }

func (rc *Race) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	if !plugin.Name(rc.from).Matches(state.Name()) {
		return plugin.NextOrFailure(rc.Name(), rc.Next, ctx, w, r)
	}
	return rc.dispatch(ctx, w, r)
}

type legResult struct {
	to  string
	msg *dns.Msg
	err error
}

// dispatch sends the query to every upstream and answers with the first useful
// reply. Losing legs are left to finish on their own; see the package docs on
// why they are neither cancelled nor waited for.
func (rc *Race) dispatch(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	results := make(chan legResult, len(rc.proxies))

	for _, p := range rc.proxies {
		// Both of these are built here rather than inside the goroutine: the
		// copy so no two legs read r while a third mutates its own header, and
		// the writer because it must snapshot the client's addresses while this
		// goroutine still owns them.
		leg, msg := detached.New(w), r.Copy()
		go func(p *proxy.Proxy) {
			results <- rc.exchange(ctx, p, leg, msg)
		}(p)
	}

	var (
		fallback *legResult
		lastErr  error
	)
	for range rc.proxies {
		var res legResult
		select {
		case <-ctx.Done():
			return dns.RcodeServerFailure, ctx.Err()
		case res = <-results:
		}

		if res.err != nil {
			lastErr = res.err
		}
		if res.msg != nil {
			last := res
			fallback = &last
		}
		if !useful(res.msg) {
			continue
		}

		winsCount.WithLabelValues(res.to).Inc()
		queriesCount.WithLabelValues(resultWon).Inc()
		return rc.reply(w, r, res.msg)
	}

	// Nothing useful anywhere. Hand back whatever an upstream did produce, so
	// the failure looks exactly as it would under forward.
	if fallback != nil {
		queriesCount.WithLabelValues(resultFallback).Inc()
		return rc.reply(w, r, fallback.msg)
	}

	queriesCount.WithLabelValues(resultFailed).Inc()
	if lastErr == nil {
		lastErr = errNoAnswer
	}
	return dns.RcodeServerFailure, plugin.Error(pluginName, lastErr)
}

// exchange asks one upstream. It never touches the live ResponseWriter: dw
// carries only the client's addresses, which request.Request needs in order to
// size the reply.
func (rc *Race) exchange(ctx context.Context, p *proxy.Proxy, dw *detached.Writer, m *dns.Msg) legResult {
	state := request.Request{W: dw, Req: m}
	res := legResult{to: p.Addr()}

	var (
		ret *dns.Msg
		err error
	)
	for range maxConnectAttempts {
		ret, _, _, err = p.Connect(ctx, state, rc.opts)
		if !staleConn(err) {
			break
		}
		// Dial has already discarded that connection, so the next attempt takes
		// the next one and eventually dials fresh.
		staleConns.WithLabelValues(res.to).Inc()
	}

	switch {
	case err != nil:
		legFailures.WithLabelValues(res.to, reasonError).Inc()
		res.err = err
		return res

	case !state.Match(ret):
		// The same check forward makes before accepting a reply: an answer to a
		// different question is not an answer.
		legFailures.WithLabelValues(res.to, reasonMismatch).Inc()
		res.err = errMismatch
		return res
	}

	if !useful(ret) {
		legFailures.WithLabelValues(res.to, reasonUnusable).Inc()
	}
	res.msg = ret
	return res
}

// reply writes m as the answer to r. The message came back from an upstream
// exchange carrying its own header, so it is retargeted first.
func (rc *Race) reply(w dns.ResponseWriter, r, m *dns.Msg) (int, error) {
	rcode := m.Rcode
	m.SetRcode(r, rcode)
	if err := w.WriteMsg(m); err != nil {
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

// staleConn reports whether err means the pooled connection was already dead,
// rather than anything being wrong with the upstream.
//
// proxy.Connect only names this case when the *read* returns io.EOF, and only
// then does it become ErrCachedClosed. A *write* to a connection the peer has
// closed fails with EPIPE or ECONNRESET and comes back as a raw syscall error,
// which is the form that reached clients as SERVFAIL. Both mean the same thing:
// discard it and dial a new one.
//
// Timeouts are deliberately not included. Those are the upstream being slow or
// unreachable, and retrying one spends the client's latency budget twice for an
// answer that is losing the contest anyway.
func staleConn(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, proxy.ErrCachedClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// useful reports whether an upstream's answer can end the contest. SERVFAIL and
// REFUSED are the cases another upstream may still do better on, which is the
// whole reason for asking more than one.
func useful(m *dns.Msg) bool {
	if m == nil || m.Truncated {
		return false
	}
	return m.Rcode == dns.RcodeSuccess || m.Rcode == dns.RcodeNameError
}
