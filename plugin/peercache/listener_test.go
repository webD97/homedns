package peercache

import (
	"testing"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func probeOnce(t *testing.T, addr, qname string) *dns.Msg {
	t.Helper()

	m, err := dns.Exchange(new(dns.Msg).SetQuestion(qname, dns.TypeA), addr)
	if err != nil {
		t.Fatalf("probing %s: %v", addr, err)
	}
	return m
}

func TestProbeListenerServesFromStore(t *testing.T) {
	addr := startPeer(t, newPeer(t, true))

	m := probeOnce(t, addr, "example.com.")
	if m.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
	if got := answerIP(t, m); got != "192.0.2.99" {
		t.Errorf("answer = %s", got)
	}
}

// A miss is REFUSED rather than resolved. REFUSED classifies as an error
// response, so the asking replica's cache never keeps it.
func TestProbeListenerRefusesOnMiss(t *testing.T) {
	addr := startPeer(t, newPeer(t, true))

	if m := probeOnce(t, addr, "absent.example.com."); m.Rcode != dns.RcodeRefused {
		t.Errorf("rcode = %s, want REFUSED", dns.RcodeToString[m.Rcode])
	}
}

// The listener has no next handler at all. This is the structural reason a
// probe can never be relayed onwards, which is what keeps loop's startup HINFO
// probe from bouncing between replicas into its log.Fatalf. newPeer wires a
// handler that fails the test the moment the chain is entered.
func TestProbeListenerNeverEntersTheChain(t *testing.T) {
	addr := startPeer(t, newPeer(t, false))

	probeOnce(t, addr, "example.com.")
	probeOnce(t, addr, "anything.example.net.")

	hinfo := new(dns.Msg).SetQuestion("probe.local.", dns.TypeHINFO)
	if _, err := dns.Exchange(hinfo, addr); err != nil {
		t.Fatalf("probing with HINFO: %v", err)
	}
}

// The port is on the pod IP and in no Service, but the source check keeps the
// household's resolution history off the rest of the cluster network.
func TestProbeListenerRefusesUnknownSource(t *testing.T) {
	p := newPeer(t, true)
	none := []string{}
	p.peers.Store(&none)
	addr := startPeer(t, p)

	before := testutil.ToFloat64(probeFailures.WithLabelValues("not_a_peer"))

	if m := probeOnce(t, addr, "example.com."); m.Rcode != dns.RcodeRefused {
		t.Errorf("rcode = %s, want REFUSED for a source outside the peer set", dns.RcodeToString[m.Rcode])
	}
	if after := testutil.ToFloat64(probeFailures.WithLabelValues("not_a_peer")); after <= before {
		t.Error("rejected source was not counted")
	}
}

func TestProbeListenerCountsReceived(t *testing.T) {
	addr := startPeer(t, newPeer(t, true))

	before := testutil.ToFloat64(probesCount.WithLabelValues("received"))
	probeOnce(t, addr, "example.com.")
	if after := testutil.ToFloat64(probesCount.WithLabelValues("received")); after <= before {
		t.Error("received probe was not counted")
	}
}
