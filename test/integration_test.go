// Package test runs the homedns binary against a real Corefile and queries it
// over the wire.
package test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "homedns-itest")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binary = filepath.Join(dir, "homedns")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = ".."
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building homedns: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// Slow on purpose, so the readiness gate is observable rather than a race.
const listLoadDelay = 1500 * time.Millisecond

const blocklistBody = `# test list
0.0.0.0 ads.example.com
*.tracker.example.net
`

// The hosts entries shadow every blocked name, so an NXDOMAIN here proves
// blocking rather than absence. Do not remove them.
const corefileTemplate = `.:{{DNS}} {
    blocklist {
        url {{URL}}
        allow ok.ads.example.com
        ready_timeout 60s
    }
    hosts {
        10.1.2.3 allowed.example.org
        10.1.2.4 ok.ads.example.com
        10.1.2.5 ads.example.com
        10.1.2.6 deep.sub.ads.example.com
        10.1.2.7 tracker.example.net
        10.1.2.8 cdn.tracker.example.net
        10.1.2.9 nottracker.example.net
    }
    ready {{READY}}
    prometheus {{METRICS}}
    errors
}
`

func TestEndToEnd(t *testing.T) {
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(listLoadDelay)
		w.Write([]byte(blocklistBody))
	}))
	defer list.Close()

	dnsAddr := freeAddr(t)
	readyAddr := freeAddr(t)
	metricsAddr := freeAddr(t)

	corefile := strings.NewReplacer(
		"{{DNS}}", port(dnsAddr),
		"{{URL}}", list.URL,
		"{{READY}}", readyAddr,
		"{{METRICS}}", metricsAddr,
	).Replace(corefileTemplate)

	path := filepath.Join(t.TempDir(), "Corefile")
	if err := os.WriteFile(path, []byte(corefile), 0o600); err != nil {
		t.Fatal(err)
	}

	stopServer(t, startServer(t, path))

	waitForDNS(t, dnsAddr)

	t.Run("not ready until the blocklist loads", func(t *testing.T) {
		if code := readyCode(t, readyAddr); code != http.StatusServiceUnavailable {
			t.Errorf("/ready returned %d before the list loaded, want 503", code)
		}
	})

	waitUntil(t, 30*time.Second, "never became ready", func() bool {
		return readyCode(t, readyAddr) == http.StatusOK
	})

	t.Run("queries", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			qname string
			rcode int
			addr  string
		}{
			{"listed name is blocked", "ads.example.com.", dns.RcodeNameError, ""},
			{"subdomain of a listed name is blocked", "deep.sub.ads.example.com.", dns.RcodeNameError, ""},
			{"wildcard entry is blocked", "tracker.example.net.", dns.RcodeNameError, ""},
			{"subdomain of a wildcard entry is blocked", "cdn.tracker.example.net.", dns.RcodeNameError, ""},
			{"allow wins over a blocked parent", "ok.ads.example.com.", dns.RcodeSuccess, "10.1.2.4"},
			{"unrelated name resolves", "allowed.example.org.", dns.RcodeSuccess, "10.1.2.3"},
			{"label-boundary match only", "nottracker.example.net.", dns.RcodeSuccess, "10.1.2.9"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resp := query(t, dnsAddr, tc.qname)
				if resp.Rcode != tc.rcode {
					t.Fatalf("%s: rcode %s, want %s", tc.qname,
						dns.RcodeToString[resp.Rcode], dns.RcodeToString[tc.rcode])
				}
				if tc.addr == "" {
					return
				}
				if len(resp.Answer) != 1 {
					t.Fatalf("%s: %d answers, want 1", tc.qname, len(resp.Answer))
				}
				a, ok := resp.Answer[0].(*dns.A)
				if !ok {
					t.Fatalf("%s: answer is %T, want *dns.A", tc.qname, resp.Answer[0])
				}
				if a.A.String() != tc.addr {
					t.Errorf("%s: got %s, want %s", tc.qname, a.A, tc.addr)
				}
			})
		}
	})

	t.Run("metrics", func(t *testing.T) {
		body := get(t, "http://"+metricsAddr+"/metrics")

		// 2 subtrees survive pruning.
		for _, want := range []string{
			"coredns_blocklist_domains_total 2",
			"coredns_blocklist_fail_open 0",
			"coredns_blocklist_blocked_total",
			"coredns_blocklist_allowed_total",
			"coredns_blocklist_source_domains",
			"homedns_build_info",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics is missing %q", want)
			}
		}
	})
}

func startServer(t *testing.T, corefile string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary, "-conf", corefile)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func stopServer(t *testing.T, cmd *exec.Cmd) {
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
}

func query(t *testing.T, addr, qname string) *dns.Msg {
	t.Helper()
	msg := new(dns.Msg).SetQuestion(qname, dns.TypeA)
	client := &dns.Client{Timeout: 5 * time.Second}

	resp, _, err := client.Exchange(msg, addr)
	if err != nil {
		t.Fatalf("querying %s: %v", qname, err)
	}
	return resp
}

func readyCode(t *testing.T, addr string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/ready")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// freeAddr returns a 127.0.0.1:port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func waitForDNS(t *testing.T, addr string) {
	t.Helper()
	msg := new(dns.Msg).SetQuestion("allowed.example.org.", dns.TypeA)
	client := &dns.Client{Timeout: time.Second}

	waitUntil(t, 30*time.Second, "server never started listening on "+addr, func() bool {
		_, _, err := client.Exchange(msg, addr)
		return err == nil
	})
}

func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// The race plugin has no external dependency, so it can be exercised for real:
// two upstreams in this process, one deliberately slow, and the answer that
// comes back over the wire has to be the fast one's.
const raceCorefileTemplate = `.:{{DNS}} {
    race . {{FAST}} {{SLOW}}
    prometheus {{METRICS}}
    errors
}
`

const raceSlowDelay = 400 * time.Millisecond

func TestRaceEndToEnd(t *testing.T) {
	fast := fakeUpstream(t, 0, "10.9.0.1")
	slow := fakeUpstream(t, raceSlowDelay, "10.9.0.2")

	dnsAddr := freeAddr(t)
	metricsAddr := freeAddr(t)

	corefile := strings.NewReplacer(
		"{{DNS}}", port(dnsAddr),
		"{{FAST}}", fast,
		"{{SLOW}}", slow,
		"{{METRICS}}", metricsAddr,
	).Replace(raceCorefileTemplate)

	path := filepath.Join(t.TempDir(), "Corefile")
	if err := os.WriteFile(path, []byte(corefile), 0o600); err != nil {
		t.Fatal(err)
	}

	stopServer(t, startServer(t, path))
	waitForDNS(t, dnsAddr)

	resp := query(t, dnsAddr, "allowed.example.org.")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode %s, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("%d answers, want 1", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is %T, want *dns.A", resp.Answer[0])
	}
	if a.A.String() != "10.9.0.1" {
		t.Errorf("got %s, want the fast upstream's 10.9.0.1", a.A)
	}

	body := get(t, "http://"+metricsAddr+"/metrics")
	for _, want := range []string{
		`coredns_race_wins_total{to="` + fast + `"}`,
		`coredns_race_queries_total{result="won"}`,
		`coredns_proxy_request_duration_seconds_count{proxy_name="race"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

// fakeUpstream answers every query with addr after delay, on an ephemeral
// loopback port.
func fakeUpstream(t *testing.T, delay time.Duration, addr string) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(
		func(w dns.ResponseWriter, r *dns.Msg) {
			if delay > 0 {
				time.Sleep(delay)
			}
			m := new(dns.Msg).SetReply(r)
			m.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
				A:   net.ParseIP(addr),
			}}
			_ = w.WriteMsg(m)
		})}

	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { pc.Close(); <-done })

	return pc.LocalAddr().String()
}
