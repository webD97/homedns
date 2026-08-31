package peercache

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

const (
	bindAttempts = 20
	bindDelay    = 250 * time.Millisecond
)

// listen serves sibling probes on its own port, off the plugin chain.
//
// Keeping this off :53 is what stops probes from being counted as client
// queries by prometheus, logged by log, and measured by blocklist. It is also
// the loop guarantee: this handler has no Next and cannot forward, so a probe
// can never be relayed onwards and loop's startup HINFO probe can never bounce
// between replicas into its log.Fatalf.
func (p *PeerCache) listen(ctx context.Context, podIP string) {
	defer p.wg.Done()

	addr := net.JoinHostPort(podIP, strconv.Itoa(p.port))
	pc := p.bind(ctx, addr)
	if pc == nil {
		return
	}

	log.Infof("serving peer probes on %s", addr)
	p.serve(ctx, pc)
}

// serve answers probes on pc until ctx is cancelled.
func (p *PeerCache) serve(ctx context.Context, pc net.PacketConn) {
	// Closing the conn is what unblocks ActivateAndServe, and it is safe at any
	// point in its lifecycle -- unlike Shutdown, which races a server that has
	// not started serving yet.
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	mux := dns.NewServeMux()
	mux.HandleFunc(".", p.serveProbe)
	srv := &dns.Server{PacketConn: pc, Handler: mux}

	if err := srv.ActivateAndServe(); err != nil && ctx.Err() == nil {
		log.Warningf("peer probe listener on %s stopped: %v; siblings can no longer take answers "+
			"from this replica", pc.LocalAddr(), err)
	}
}

// bind retries because the reload plugin re-runs setup in place: OnStartup can
// fire before the previous listener has released the port. Giving up is not
// fatal, it only means siblings stop getting hits from this replica.
func (p *PeerCache) bind(ctx context.Context, addr string) net.PacketConn {
	for i := range bindAttempts {
		pc, err := net.ListenPacket("udp", addr)
		if err == nil {
			return pc
		}

		if i == bindAttempts-1 {
			log.Warningf("could not bind peer probe listener on %s after %s: %v; this replica "+
				"will still take answers from siblings but cannot serve them", addr,
				time.Duration(bindAttempts)*bindDelay, err)
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(bindDelay):
		}
	}
	return nil
}

// serveProbe answers a sibling from the probe store, or refuses. There is no
// next handler here by design; see listen.
func (p *PeerCache) serveProbe(w dns.ResponseWriter, r *dns.Msg) {
	started := time.Now()
	probesCount.WithLabelValues("received").Inc()
	defer func() { probeDuration.WithLabelValues("received").Observe(time.Since(started).Seconds()) }()

	// The port is on the pod IP and in no Service, but a source check keeps the
	// house's resolution history off the rest of the cluster network.
	if !p.isPeer(w.RemoteAddr()) {
		probeFailures.WithLabelValues("not_a_peer").Inc()
		refuse(w, r)
		return
	}

	if len(r.Question) != 1 {
		refuse(w, r)
		return
	}

	m, ok := p.store.get(request.Request{W: w, Req: r})
	if !ok {
		refuse(w, r)
		return
	}
	if err := w.WriteMsg(m); err != nil {
		log.Debugf("answering peer probe from %s: %v", w.RemoteAddr(), err)
	}
}

// isPeer matches on IP alone: a sibling's probe comes from an ephemeral source
// port, not the port it listens on.
func (p *PeerCache) isPeer(remote net.Addr) bool {
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	for _, peer := range p.peerList() {
		peerHost, _, err := net.SplitHostPort(peer)
		if err != nil {
			continue
		}
		if ip.Equal(net.ParseIP(peerHost)) {
			return true
		}
	}
	return false
}

func refuse(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeRefused)
	_ = w.WriteMsg(m)
}
