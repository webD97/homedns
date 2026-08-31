package peercache

import (
	"testing"
	"time"

	"github.com/coredns/coredns/plugin/test"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

func stateFor(qname string, qtype uint16, opts ...func(*dns.Msg)) request.Request {
	r := new(dns.Msg).SetQuestion(qname, qtype)
	for _, o := range opts {
		o(r)
	}
	return request.Request{W: &test.ResponseWriter{}, Req: r}
}

func withDO(m *dns.Msg) { m.SetEdns0(4096, true) }
func withCD(m *dns.Msg) { m.CheckingDisabled = true }
func answerA(qname string, ttl uint32) *dns.Msg {
	m := new(dns.Msg).SetReply(new(dns.Msg).SetQuestion(qname, dns.TypeA))
	m.Answer = []dns.RR{test.A(qname + " " + itoa(ttl) + " IN A 192.0.2.1")}
	return m
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestStoreRoundTripRebasesTTL(t *testing.T) {
	now := time.Now()
	s := newStore(16)
	s.now = func() time.Time { return now }

	state := stateFor("example.com.", dns.TypeA)
	s.put(state, answerA("example.com.", 300))

	// Half the TTL later the sibling should be offered what is actually left.
	now = now.Add(120 * time.Second)
	m, ok := s.get(state)
	if !ok {
		t.Fatal("no entry returned")
	}
	if got := m.Answer[0].Header().Ttl; got != 180 {
		t.Errorf("ttl = %d, want 180", got)
	}
	if m.Id != state.Req.Id || m.Question[0].Name != "example.com." {
		t.Error("answer was not retargeted at the asking request")
	}
}

// A name close to expiry must go upstream instead of bouncing between replicas
// at an ever-shrinking TTL.
func TestStoreDropsNearExpiry(t *testing.T) {
	now := time.Now()
	s := newStore(16)
	s.now = func() time.Time { return now }

	state := stateFor("example.com.", dns.TypeA)
	s.put(state, answerA("example.com.", 30))

	now = now.Add(26 * time.Second)
	if _, ok := s.get(state); ok {
		t.Error("served an entry with less than the minimum TTL left")
	}
	if s.len() != 0 {
		t.Error("expired entry was not removed")
	}
}

func TestStoreRejectsShortTTLOnPut(t *testing.T) {
	s := newStore(16)
	state := stateFor("example.com.", dns.TypeA)
	s.put(state, answerA("example.com.", 2))

	if s.len() != 0 {
		t.Error("stored an answer that is already below the serve floor")
	}
}

// DO and CD are excluded on both sides so the DNSSEC semantics the stock cache
// implements never have to be reproduced here.
func TestStoreIgnoresDOAndCD(t *testing.T) {
	for name, state := range map[string]request.Request{
		"DO": stateFor("example.com.", dns.TypeA, withDO),
		"CD": stateFor("example.com.", dns.TypeA, withCD),
	} {
		s := newStore(16)
		s.put(state, answerA("example.com.", 300))
		if s.len() != 0 {
			t.Errorf("%s: answer was stored", name)
		}

		// And a plain entry must not be handed to such a query either.
		plain := stateFor("example.com.", dns.TypeA)
		s.put(plain, answerA("example.com.", 300))
		if _, ok := s.get(state); ok {
			t.Errorf("%s: served from the store", name)
		}
	}
}

func TestStoreOnlyKeepsCacheableResponses(t *testing.T) {
	servfail := new(dns.Msg)
	servfail.SetRcode(new(dns.Msg).SetQuestion("example.com.", dns.TypeA), dns.RcodeServerFailure)

	truncated := answerA("example.com.", 300)
	truncated.Truncated = true

	empty := new(dns.Msg).SetReply(new(dns.Msg).SetQuestion("example.com.", dns.TypeA))

	for name, m := range map[string]*dns.Msg{
		"servfail":   servfail,
		"truncated":  truncated,
		"no records": empty,
	} {
		s := newStore(16)
		s.put(stateFor("example.com.", dns.TypeA), m)
		if s.len() != 0 {
			t.Errorf("%s: was stored", name)
		}
	}
}

func TestStoreKeysOnTypeAndName(t *testing.T) {
	s := newStore(16)
	s.put(stateFor("example.com.", dns.TypeA), answerA("example.com.", 300))

	if _, ok := s.get(stateFor("example.com.", dns.TypeAAAA)); ok {
		t.Error("an A answer was served for an AAAA query")
	}
	if _, ok := s.get(stateFor("other.example.com.", dns.TypeA)); ok {
		t.Error("served an answer for a different name")
	}
	// Names are matched case-insensitively, as request.Name lowercases.
	if _, ok := s.get(stateFor("EXAMPLE.COM.", dns.TypeA)); !ok {
		t.Error("case-different name missed")
	}
}

// OPT carries the extended rcode and flags in its header TTL, so it must be
// left out of both the expiry calculation and the rewrite.
func TestStoreIgnoresOPTTTL(t *testing.T) {
	m := answerA("example.com.", 300)
	m.SetEdns0(4096, false)

	if ttl, ok := minTTL(m); !ok || ttl != 300 {
		t.Errorf("minTTL = %d %v, want 300 true", ttl, ok)
	}

	setTTL(m, 60)
	if opt := m.IsEdns0(); opt == nil || opt.Header().Ttl != 0 {
		t.Error("setTTL overwrote the OPT header")
	}
}

func TestStoreNegativeAnswer(t *testing.T) {
	q := new(dns.Msg).SetQuestion("nope.example.com.", dns.TypeA)
	m := new(dns.Msg).SetRcode(q, dns.RcodeNameError)
	m.Ns = []dns.RR{test.SOA("example.com. 60 IN SOA ns.example.com. hostmaster.example.com. 1 60 60 60 60")}

	s := newStore(16)
	state := stateFor("nope.example.com.", dns.TypeA)
	s.put(state, m)

	got, ok := s.get(state)
	if !ok {
		t.Fatal("NXDOMAIN was not stored")
	}
	if got.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %s, want NXDOMAIN", dns.RcodeToString[got.Rcode])
	}
}
