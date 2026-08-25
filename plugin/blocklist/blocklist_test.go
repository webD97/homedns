package blocklist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestServeDNSBlocksAndPassesThrough(t *testing.T) {
	b := &Blocklist{Next: test.NextHandler(dns.RcodeSuccess, nil)}
	b.current.Store(&matcher{
		block: newNameSet([]string{"ads.example.com"}),
		allow: newNameSet([]string{"ok.ads.example.com"}),
	})

	for _, tc := range []struct {
		qname string
		want  int
	}{
		{"ads.example.com.", dns.RcodeNameError},
		{"img.ads.example.com.", dns.RcodeNameError}, // subtree
		{"ADS.EXAMPLE.COM.", dns.RcodeNameError},     // case-insensitive
		{"ok.ads.example.com.", dns.RcodeSuccess},    // allow wins
		{"example.com.", dns.RcodeSuccess},           // parent not blocked
		{"unrelated.net.", dns.RcodeSuccess},
	} {
		req := new(dns.Msg).SetQuestion(tc.qname, dns.TypeA)
		rec := dnstest.NewRecorder(&test.ResponseWriter{})

		rcode, err := b.ServeDNS(context.Background(), rec, req)
		if err != nil {
			t.Fatalf("%s: %v", tc.qname, err)
		}
		if rcode != tc.want {
			t.Errorf("%s: rcode = %s, want %s", tc.qname, dns.RcodeToString[rcode], dns.RcodeToString[tc.want])
		}
		if tc.want == dns.RcodeNameError {
			if rec.Msg == nil {
				t.Fatalf("%s: no response written", tc.qname)
			}
			if rec.Msg.Rcode != dns.RcodeNameError {
				t.Errorf("%s: wrote %s, want NXDOMAIN", tc.qname, dns.RcodeToString[rec.Msg.Rcode])
			}
			if !rec.Msg.Authoritative {
				t.Errorf("%s: NXDOMAIN should be authoritative", tc.qname)
			}
			if len(rec.Msg.Answer) != 0 {
				t.Errorf("%s: NXDOMAIN should carry no answers", tc.qname)
			}
		}
	}
}

// Blocking must apply to every query type, not just A.
func TestServeDNSBlocksAllQTypes(t *testing.T) {
	b := &Blocklist{Next: test.NextHandler(dns.RcodeSuccess, nil)}
	b.current.Store(&matcher{block: newNameSet([]string{"ads.example.com"})})

	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeMX, dns.TypeTXT} {
		req := new(dns.Msg).SetQuestion("ads.example.com.", qtype)
		rec := dnstest.NewRecorder(&test.ResponseWriter{})
		rcode, err := b.ServeDNS(context.Background(), rec, req)
		if err != nil {
			t.Fatal(err)
		}
		if rcode != dns.RcodeNameError {
			t.Errorf("qtype %s: rcode = %s, want NXDOMAIN",
				dns.TypeToString[qtype], dns.RcodeToString[rcode])
		}
	}
}

// Before the first fetch lands there is no matcher at all; queries must still
// flow rather than fail.
func TestServeDNSBeforeFirstLoad(t *testing.T) {
	b := &Blocklist{Next: test.NextHandler(dns.RcodeSuccess, nil)}

	req := new(dns.Msg).SetQuestion("ads.example.com.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	rcode, err := b.ServeDNS(context.Background(), rec, req)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != dns.RcodeSuccess {
		t.Errorf("rcode = %s, want NOERROR passthrough", dns.RcodeToString[rcode])
	}
}

func newTestBlocklist(t *testing.T, urls []string, readyTimeout time.Duration) *Blocklist {
	t.Helper()
	return &Blocklist{
		Next:         test.NextHandler(dns.RcodeSuccess, nil),
		urls:         urls,
		refresh:      time.Hour,
		readyTimeout: readyTimeout,
		lastGood:     map[string][]string{},
		// httptest serves on an IP literal, so nothing here is resolved; the
		// bootstrap servers just need to be valid.
		bootstrap: newBootstrapResolver(defaultBootstrapDNS),
	}
}

func TestReadyAfterFirstLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("0.0.0.0 ads.example.com\n0.0.0.0 tracker.example.net\n"))
	}))
	defer srv.Close()

	b := newTestBlocklist(t, []string{srv.URL}, time.Hour)
	if b.Ready() {
		t.Fatal("must not be ready before any list is loaded")
	}

	if err := b.start(); err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()

	waitFor(t, b.Ready, 5*time.Second, "plugin never became ready")

	if got := b.current.Load().size(); got != 2 {
		t.Errorf("loaded %d domains, want 2", got)
	}
	if v := testutil.ToFloat64(failOpen); v != 0 {
		t.Errorf("fail_open = %v, want 0 after a successful load", v)
	}
}

// A home network's only resolver must not stay out of service because a
// blocklist host is unreachable: after ready_timeout it reports ready and
// serves unfiltered, flagging the state in a metric.
func TestReadyFailsOpenAfterTimeout(t *testing.T) {
	failOpen.Set(0)

	// Reserve a port, then close it, so connections are refused immediately.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	b := newTestBlocklist(t, []string{deadURL}, 200*time.Millisecond)
	if err := b.start(); err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()

	waitFor(t, b.Ready, 5*time.Second, "plugin never failed open")

	if v := testutil.ToFloat64(failOpen); v != 1 {
		t.Errorf("fail_open = %v, want 1", v)
	}
	if b.current.Load().size() != 0 {
		t.Error("no list should be loaded")
	}
}

// One dead URL must never empty the blocklist.
func TestRefreshKeepsLastGoodOnFailure(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	b := newTestBlocklist(t, []string{srv.URL}, time.Hour)
	b.reload(context.Background())
	if got := b.current.Load().size(); got != 1 {
		t.Fatalf("first load: %d domains, want 1", got)
	}

	fail = true
	b.reload(context.Background())

	if got := b.current.Load().size(); got != 1 {
		t.Errorf("after a failed refresh: %d domains, want the previous 1 retained", got)
	}
	if !b.current.Load().blocked("ads.example.com") {
		t.Error("previously loaded domain stopped being blocked after a failed refresh")
	}
}

// A 200 that parses to nothing is an error page or a moved list, not an empty
// blocklist — treating it as success would silently disable filtering.
func TestEmptyResponseTreatedAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html>404 not found</html>\n"))
	}))
	defer srv.Close()

	if _, err := fetch(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected an error for a response containing no domains")
	}
}

func TestShutdownStopsRefreshLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	b := newTestBlocklist(t, []string{srv.URL}, time.Hour)
	if err := b.start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, b.Ready, 5*time.Second, "plugin never became ready")

	// shutdown blocks until the goroutine is gone; the reload plugin rebuilds
	// the handler on every Corefile change, so a leak here compounds.
	done := make(chan error, 1)
	go func() { done <- b.shutdown() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return: refresh goroutine leaked")
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
