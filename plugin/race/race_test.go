package race

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/pkg/proxy"
	"github.com/coredns/coredns/plugin/pkg/transport"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newUpstream runs a fake resolver on an ephemeral loopback port. The conn is
// closed rather than the server shut down, which is what makes ActivateAndServe
// return without racing a server that has not started serving yet.
func newUpstream(t *testing.T, h dns.HandlerFunc) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &dns.Server{PacketConn: pc, Handler: h}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { pc.Close(); <-done })

	return pc.LocalAddr().String()
}

// blackHole accepts packets and never answers.
func blackHole(t *testing.T) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc.LocalAddr().String()
}

func answering(delay time.Duration, rcode int, ip string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		if delay > 0 {
			time.Sleep(delay)
		}
		m := new(dns.Msg).SetRcode(r, rcode)
		if ip != "" {
			m.Answer = []dns.RR{test.A("example.com. 300 IN A " + ip)}
		}
		_ = w.WriteMsg(m)
	}
}

func newRacer(t *testing.T, readTimeout time.Duration, addrs ...string) *Race {
	t.Helper()

	rc := &Race{from: ".", expire: defaultExpire}
	for _, addr := range addrs {
		p := proxy.NewProxy(pluginName, addr, transport.DNS)
		p.SetReadTimeout(readTimeout)
		p.SetMaxIdleConns(maxIdleConns)
		rc.proxies = append(rc.proxies, p)
	}
	return rc
}

func query(t *testing.T, rc *Race, w dns.ResponseWriter) (int, error) {
	t.Helper()
	req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	return rc.ServeDNS(context.Background(), w, req)
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

// A slow loser is what makes the outcome deterministic; in production the gap
// between two resolvers is tens of milliseconds.
const slow = 250 * time.Millisecond

func TestFastestUpstreamWins(t *testing.T) {
	fast := newUpstream(t, answering(0, dns.RcodeSuccess, "192.0.2.1"))
	newSlow := newUpstream(t, answering(slow, dns.RcodeSuccess, "192.0.2.2"))

	rc := newRacer(t, time.Second, fast, newSlow)
	before := testutil.ToFloat64(winsCount.WithLabelValues(fast))

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, rc, rec); err != nil {
		t.Fatal(err)
	}
	if got := answerIP(t, rec.Msg); got != "192.0.2.1" {
		t.Errorf("answer = %s, want the fast upstream's 192.0.2.1", got)
	}
	if after := testutil.ToFloat64(winsCount.WithLabelValues(fast)); after <= before {
		t.Error("the win was not counted against the upstream that answered")
	}
}

// A fast failure must not end the contest: another upstream may still resolve
// the name, which is the entire reason for asking more than one.
func TestFastServfailLosesToSlowerSuccess(t *testing.T) {
	failing := newUpstream(t, answering(0, dns.RcodeServerFailure, ""))
	working := newUpstream(t, answering(slow, dns.RcodeSuccess, "192.0.2.2"))

	rc := newRacer(t, time.Second, failing, working)
	before := testutil.ToFloat64(legFailures.WithLabelValues(failing, reasonUnusable))

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, rc, rec); err != nil {
		t.Fatal(err)
	}
	if got := answerIP(t, rec.Msg); got != "192.0.2.2" {
		t.Errorf("answer = %s, want the slower NOERROR 192.0.2.2", got)
	}
	if after := testutil.ToFloat64(legFailures.WithLabelValues(failing, reasonUnusable)); after <= before {
		t.Error("the SERVFAIL leg was not counted as unusable")
	}
}

// A truncated answer is not an answer: it loses to a complete one.
func TestTruncatedAnswerLoses(t *testing.T) {
	cut := newUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg).SetReply(r)
		m.Truncated = true
		_ = w.WriteMsg(m)
	})
	whole := newUpstream(t, answering(slow, dns.RcodeSuccess, "192.0.2.2"))

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, newRacer(t, time.Second, cut, whole), rec); err != nil {
		t.Fatal(err)
	}
	if got := answerIP(t, rec.Msg); got != "192.0.2.2" {
		t.Errorf("answer = %s, want the untruncated 192.0.2.2", got)
	}
}

// An answer to a different question is not an answer, the check forward makes
// before accepting a reply.
func TestMismatchedReplyIsRejected(t *testing.T) {
	wrong := newUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg).SetReply(r)
		m.Question[0].Name = "elsewhere.example."
		m.Answer = []dns.RR{test.A("elsewhere.example. 300 IN A 192.0.2.9")}
		_ = w.WriteMsg(m)
	})
	right := newUpstream(t, answering(slow, dns.RcodeSuccess, "192.0.2.2"))

	rc := newRacer(t, time.Second, wrong, right)
	before := testutil.ToFloat64(legFailures.WithLabelValues(wrong, reasonMismatch))

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, rc, rec); err != nil {
		t.Fatal(err)
	}
	if got := answerIP(t, rec.Msg); got != "192.0.2.2" {
		t.Errorf("answer = %s, want the upstream that answered the question asked", got)
	}
	if after := testutil.ToFloat64(legFailures.WithLabelValues(wrong, reasonMismatch)); after <= before {
		t.Error("the mismatched reply was not counted")
	}
}

// With nothing useful anywhere the client must see what an upstream actually
// said, exactly as it would under forward.
func TestAllUpstreamsFailingReturnsTheirResponse(t *testing.T) {
	a := newUpstream(t, answering(0, dns.RcodeServerFailure, ""))
	b := newUpstream(t, answering(0, dns.RcodeRefused, ""))

	before := testutil.ToFloat64(queriesCount.WithLabelValues(resultFallback))

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, newRacer(t, time.Second, a, b), rec); err != nil {
		t.Fatal(err)
	}
	if rec.Msg == nil {
		t.Fatal("no response written")
	}
	switch rec.Msg.Rcode {
	case dns.RcodeServerFailure, dns.RcodeRefused:
	default:
		t.Errorf("rcode = %s, want an upstream's own failure", dns.RcodeToString[rec.Msg.Rcode])
	}
	if after := testutil.ToFloat64(queriesCount.WithLabelValues(resultFallback)); after <= before {
		t.Error("the fallback was not counted")
	}
}

// Every upstream unreachable is the one case that has to synthesize a failure.
func TestAllUpstreamsDeadServfails(t *testing.T) {
	rc := newRacer(t, 300*time.Millisecond, blackHole(t), blackHole(t))
	before := testutil.ToFloat64(queriesCount.WithLabelValues(resultFailed))

	rcode, err := query(t, rc, dnstest.NewRecorder(&test.ResponseWriter{}))
	if err == nil {
		t.Error("want an error when no upstream answers")
	}
	if rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[rcode])
	}
	if after := testutil.ToFloat64(queriesCount.WithLabelValues(resultFailed)); after <= before {
		t.Error("the failure was not counted")
	}
}

// nextHandler stands in for the rest of the chain below race.
type nextHandler struct{ calls atomic.Int64 }

func (h *nextHandler) Name() string { return "next" }
func (h *nextHandler) ServeDNS(_ context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	h.calls.Add(1)
	m := new(dns.Msg).SetReply(r)
	m.Answer = []dns.RR{test.A("example.com. 300 IN A 192.0.2.7")}
	return dns.RcodeSuccess, w.WriteMsg(m)
}

func TestOutOfZoneFallsThrough(t *testing.T) {
	next := &nextHandler{}
	rc := newRacer(t, time.Second, blackHole(t), blackHole(t))
	rc.from, rc.Next = "example.org.", next

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, rc, rec); err != nil {
		t.Fatal(err)
	}
	if got := next.calls.Load(); got != 1 {
		t.Errorf("next called %d times, want 1", got)
	}
}

// countingWriter records every interaction, and whether it happened after the
// request was finished and the server would be free to reuse the connection.
type countingWriter struct {
	test.ResponseWriter
	done      atomic.Bool
	writes    atomic.Int64
	afterDone atomic.Int64
}

func (w *countingWriter) note() {
	if w.done.Load() {
		w.afterDone.Add(1)
	}
}

func (w *countingWriter) LocalAddr() net.Addr  { w.note(); return w.ResponseWriter.LocalAddr() }
func (w *countingWriter) RemoteAddr() net.Addr { w.note(); return w.ResponseWriter.RemoteAddr() }
func (w *countingWriter) WriteMsg(m *dns.Msg) error {
	w.note()
	w.writes.Add(1)
	return w.ResponseWriter.WriteMsg(m)
}

// The client's connection carries exactly one reply, and a leg that is still
// running when ServeDNS returns never reaches it. That is structural: legs get a
// detached.Writer holding nothing but the client's addresses.
func TestOnlyOneReplyReachesTheClient(t *testing.T) {
	loserDone := make(chan struct{})
	fast := newUpstream(t, answering(0, dns.RcodeSuccess, "192.0.2.1"))
	loser := newUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		defer close(loserDone)
		time.Sleep(slow)
		m := new(dns.Msg).SetRcode(r, dns.RcodeSuccess)
		m.Answer = []dns.RR{test.A("example.com. 300 IN A 192.0.2.2")}
		_ = w.WriteMsg(m)
	})

	w := &countingWriter{}
	if _, err := query(t, newRacer(t, time.Second, fast, loser), w); err != nil {
		t.Fatal(err)
	}

	// From here the server may hand the connection to another query.
	w.done.Store(true)

	<-loserDone
	time.Sleep(100 * time.Millisecond) // let the losing leg finish reporting

	if got := w.writes.Load(); got != 1 {
		t.Errorf("client received %d replies, want exactly 1", got)
	}
	if got := w.afterDone.Load(); got != 0 {
		t.Errorf("the losing leg touched the client's writer %d times after the request finished", got)
	}
}

func TestConcurrentServeDNS(t *testing.T) {
	fast := newUpstream(t, answering(0, dns.RcodeSuccess, "192.0.2.1"))
	failing := newUpstream(t, answering(0, dns.RcodeServerFailure, ""))
	rc := newRacer(t, time.Second, fast, failing)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				// Not query(): t.Fatal must not be called off the test goroutine.
				req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
				rec := dnstest.NewRecorder(&test.ResponseWriter{})
				_, _ = rc.ServeDNS(context.Background(), rec, req)
			}
		}()
	}
	wg.Wait()
}

func TestUseful(t *testing.T) {
	for name, tc := range map[string]struct {
		msg  *dns.Msg
		want bool
	}{
		"nil":       {nil, false},
		"noerror":   {&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}}, true},
		"nxdomain":  {&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeNameError}}, true},
		"servfail":  {&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}, false},
		"refused":   {&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeRefused}}, false},
		"truncated": {&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess, Truncated: true}}, false},
	} {
		if got := useful(tc.msg); got != tc.want {
			t.Errorf("%s: useful = %v, want %v", name, got, tc.want)
		}
	}
}

var _ plugin.Handler = (*Race)(nil)

// hangUpUpstream answers over TCP and then closes the connection, so every
// connection it hands back is dead by the time it is reused — the same thing a
// public resolver does to an idle DoT connection.
func hangUpUpstream(t *testing.T, ip string) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &dns.Server{Listener: l, Handler: dns.HandlerFunc(
		func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg).SetReply(r)
			m.Answer = []dns.RR{test.A("example.com. 300 IN A " + ip)}
			_ = w.WriteMsg(m)
			_ = w.Close()
		})}

	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { l.Close(); <-done })

	return l.Addr().String()
}

// A pool full of connections the upstream has already hung up on must not fail
// the query. This is the bug that reached production: the pool can hold more
// dead connections than a fixed retry count, and a write to one of them fails
// with EPIPE rather than the ErrCachedClosed that proxy.Connect names.
func TestDeadPooledConnectionsAreDrained(t *testing.T) {
	addr := hangUpUpstream(t, "192.0.2.1")
	rc := newRacer(t, 2*time.Second, addr)
	rc.opts.ForceTCP = true

	// Warm the pool: each of these dials fresh, gets an answer, yields the
	// connection back — and the server has already hung up on it.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
			_, _ = rc.ServeDNS(context.Background(), dnstest.NewRecorder(&test.ResponseWriter{}), req)
		}()
	}
	wg.Wait()
	time.Sleep(150 * time.Millisecond) // let the peer's FINs land

	before := testutil.ToFloat64(staleConns.WithLabelValues(addr))

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := query(t, rc, rec); err != nil {
		t.Fatalf("a pool of dead connections must not fail the query: %v", err)
	}
	if got := answerIP(t, rec.Msg); got != "192.0.2.1" {
		t.Errorf("answer = %s, want 192.0.2.1", got)
	}
	if after := testutil.ToFloat64(staleConns.WithLabelValues(addr)); after <= before {
		t.Error("draining a dead pooled connection was not counted")
	}
}

func TestStaleConn(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":              {nil, false},
		"cached closed":    {proxy.ErrCachedClosed, true},
		"eof":              {io.EOF, true},
		"broken pipe":      {&net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)}, true},
		"connection reset": {&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		// A timeout is the upstream being slow, not a dead connection: retrying
		// would spend the client's latency budget twice.
		"timeout": {&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, false},
		"other":   {errors.New("nope"), false},
	} {
		if got := staleConn(tc.err); got != tc.want {
			t.Errorf("%s: staleConn = %v, want %v", name, got, tc.want)
		}
	}
}
