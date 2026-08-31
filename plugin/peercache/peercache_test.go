package peercache

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// answerHandler stands in for the rest of the chain below peercache.
type answerHandler struct {
	ip    string
	delay time.Duration
	rcode int
	err   error
	calls int
	mu    sync.Mutex
}

func (h *answerHandler) Name() string { return "answer" }

func (h *answerHandler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()

	if h.delay > 0 {
		select {
		case <-ctx.Done():
			return dns.RcodeServerFailure, ctx.Err()
		case <-time.After(h.delay):
		}
	}
	if h.err != nil || h.rcode != 0 {
		return h.rcode, h.err
	}

	m := new(dns.Msg).SetReply(r)
	m.Answer = []dns.RR{test.A("example.com. 300 IN A " + h.ip)}
	if err := w.WriteMsg(m); err != nil {
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

func (h *answerHandler) called() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// failHandler fails the test if the chain is entered at all.
type failHandler struct{ t *testing.T }

func (h *failHandler) Name() string { return "fail" }
func (h *failHandler) ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error) {
	h.t.Error("the probe listener reached the plugin chain; it must never forward")
	return dns.RcodeServerFailure, errors.New("must not be called")
}

// startPeer runs a real probe listener on an ephemeral loopback port and
// returns its address.
func startPeer(t *testing.T, p *PeerCache) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.serve(ctx, pc) }()
	t.Cleanup(func() { cancel(); <-done })

	return pc.LocalAddr().String()
}

// newPeer builds a sibling that trusts loopback probes and holds one answer.
func newPeer(t *testing.T, holds bool) *PeerCache {
	t.Helper()

	p := &PeerCache{store: newStore(16), Next: &failHandler{t: t}}
	trusted := []string{"127.0.0.1:1"}
	p.peers.Store(&trusted)

	if holds {
		m := new(dns.Msg).SetReply(new(dns.Msg).SetQuestion("example.com.", dns.TypeA))
		m.Answer = []dns.RR{test.A("example.com. 300 IN A 192.0.2.99")}
		p.store.put(stateFor("example.com.", dns.TypeA), m)
	}
	return p
}

func newRacer(next plugin.Handler, peers ...string) *PeerCache {
	p := &PeerCache{
		Next:   next,
		store:  newStore(16),
		client: &dns.Client{Net: "udp", Timeout: probeTimeout},
	}
	p.peers.Store(&peers)
	return p
}

func query(t *testing.T, p *PeerCache) *dns.Msg {
	t.Helper()

	req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := p.ServeDNS(context.Background(), rec, req); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if rec.Msg == nil {
		t.Fatal("no response written")
	}
	return rec.Msg
}

func answerIP(t *testing.T, m *dns.Msg) string {
	t.Helper()
	if len(m.Answer) != 1 {
		t.Fatalf("want one answer, got %d", len(m.Answer))
	}
	a, ok := m.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is %T, want A", m.Answer[0])
	}
	return a.A.String()
}

func TestPeerWinsRace(t *testing.T) {
	addr := startPeer(t, newPeer(t, true))

	before := testutil.ToFloat64(winsCount.WithLabelValues(sourcePeer))
	// A slow upstream is what makes the outcome deterministic; in production
	// the gap is a DoT round trip against a sub-millisecond pod hop.
	p := newRacer(&answerHandler{ip: "192.0.2.1", delay: 300 * time.Millisecond}, addr)

	m := query(t, p)
	if got := answerIP(t, m); got != "192.0.2.99" {
		t.Errorf("answer = %s, want the peer's 192.0.2.99", got)
	}
	if after := testutil.ToFloat64(winsCount.WithLabelValues(sourcePeer)); after <= before {
		t.Error("peer win was not counted")
	}

	// The winner is also kept for this replica's own siblings.
	if _, ok := p.store.get(stateFor("example.com.", dns.TypeA)); !ok {
		t.Error("peer-won answer was not recorded in the probe store")
	}
}

func TestUpstreamWinsWhenPeerRefuses(t *testing.T) {
	addr := startPeer(t, newPeer(t, false))

	before := testutil.ToFloat64(winsCount.WithLabelValues(sourceUpstream))
	p := newRacer(&answerHandler{ip: "192.0.2.1"}, addr)

	if got := answerIP(t, query(t, p)); got != "192.0.2.1" {
		t.Errorf("answer = %s, want the upstream's 192.0.2.1", got)
	}
	if after := testutil.ToFloat64(winsCount.WithLabelValues(sourceUpstream)); after <= before {
		t.Error("upstream win was not counted")
	}
}

// A sibling that never answers must not delay the client: the upstream leg is
// already in flight.
func TestBlackHoledPeerDoesNotStall(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })

	p := newRacer(&answerHandler{ip: "192.0.2.1"}, pc.LocalAddr().String())

	started := time.Now()
	if got := answerIP(t, query(t, p)); got != "192.0.2.1" {
		t.Errorf("answer = %s", got)
	}
	if elapsed := time.Since(started); elapsed > probeTimeout {
		t.Errorf("took %s, want the upstream answer well inside the %s probe timeout", elapsed, probeTimeout)
	}
}

func TestProbeTimeoutIsCounted(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })

	before := testutil.ToFloat64(probeFailures.WithLabelValues("timeout"))
	p := newRacer(&answerHandler{ip: "192.0.2.1"})

	res := p.probe(context.Background(), pc.LocalAddr().String(), new(dns.Msg).SetQuestion("example.com.", dns.TypeA))
	if res.msg != nil {
		t.Error("a black hole produced an answer")
	}
	if after := testutil.ToFloat64(probeFailures.WithLabelValues("timeout")); after <= before {
		t.Error("timeout was not counted")
	}
}

// With nothing discovered the plugin must behave as if it were not in the
// chain, while still recording the answer for whenever a sibling appears.
func TestNoPeersIsPassThrough(t *testing.T) {
	next := &answerHandler{ip: "192.0.2.1"}
	p := newRacer(next)

	if got := answerIP(t, query(t, p)); got != "192.0.2.1" {
		t.Errorf("answer = %s", got)
	}
	if next.called() != 1 {
		t.Errorf("next called %d times, want 1", next.called())
	}
	if _, ok := p.store.get(stateFor("example.com.", dns.TypeA)); !ok {
		t.Error("answer was not recorded in the probe store")
	}
}

// When no leg produces anything usable the failure must look exactly as it
// would without this plugin in the chain.
func TestAllLegsFailPropagatesUpstream(t *testing.T) {
	addr := startPeer(t, newPeer(t, false))

	wantErr := errors.New("upstream is down")
	p := newRacer(&answerHandler{rcode: dns.RcodeServerFailure, err: wantErr}, addr)

	req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	rcode, err := p.ServeDNS(context.Background(), rec, req)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[rcode])
	}
}

// The peer set is swapped wholesale while queries are in flight.
func TestConcurrentPeerSwapAndServe(t *testing.T) {
	p := newRacer(&answerHandler{ip: "192.0.2.1"})

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				// Not query(): t.Fatal must not be called off the test goroutine.
				req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
				rec := dnstest.NewRecorder(&test.ResponseWriter{})
				_, _ = p.ServeDNS(context.Background(), rec, req)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 200 {
			set := []string{}
			if i%2 == 0 {
				set = append(set, "127.0.0.1:1")
			}
			p.peers.Store(&set)
		}
	}()
	wg.Wait()
}
