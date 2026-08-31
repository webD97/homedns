package peercache

import (
	"time"

	"github.com/coredns/coredns/plugin/pkg/cache"
	"github.com/coredns/coredns/plugin/pkg/response"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

const (
	// storeCapacity is fixed rather than configurable: the store only has to
	// answer sibling probes, and a miss costs nothing because the caller's
	// upstream leg is racing anyway.
	storeCapacity = 4096

	// minServeTTL is the floor below which an entry is dropped instead of
	// served. Without it a name near expiry bounces between replicas at an
	// ever-shrinking TTL instead of being refreshed upstream.
	minServeTTL = 5 * time.Second
)

// entry is an answer held for sibling probes. The message keeps its original
// TTLs; what is served is rebased against stored.
type entry struct {
	msg     *dns.Msg
	stored  time.Time
	origTTL uint32
}

// store holds answers this replica has resolved, so a sibling can take them
// instead of paying for its own upstream round trip.
//
// It is not a second resolver cache and must not grow into one: the stock cache
// plugin serves every client query. A wrong or stale entry here costs a missed
// peer hit, never a wrong answer to a client.
type store struct {
	c *cache.Cache[*entry]

	// now is swapped out in tests.
	now func() time.Time
}

func newStore(capacity int) *store {
	return &store{c: cache.New[*entry](capacity), now: time.Now}
}

// storable reports whether an exchange may be held for siblings.
//
// DO and CD queries are excluded on purpose. Keying and serving them correctly
// means the DNSSEC and RFC 6840 5.7-5.8 AD-bit handling that makes the stock
// cache large, and there is no need to take that on: excluded queries simply
// take the upstream leg on both sides.
func storable(state request.Request, m *dns.Msg) bool {
	if m == nil || m.Truncated || state.Do() || state.Req.CheckingDisabled {
		return false
	}

	switch t, _ := response.Typify(m, time.Now()); t {
	case response.NoError, response.NameError, response.NoData:
		return true
	default:
		return false
	}
}

func (s *store) put(state request.Request, m *dns.Msg) {
	if !storable(state, m) {
		return
	}

	ttl, ok := minTTL(m)
	if !ok || time.Duration(ttl)*time.Second < minServeTTL {
		return
	}

	s.c.Add(key(state), &entry{msg: m.Copy(), stored: s.now(), origTTL: ttl})
	storeEntries.Set(float64(s.c.Len()))
}

// get returns an answer for state with its TTLs rebased, or false.
func (s *store) get(state request.Request) (*dns.Msg, bool) {
	if state.Do() || state.Req.CheckingDisabled {
		return nil, false
	}

	k := key(state)
	e, ok := s.c.Get(k)
	if !ok {
		return nil, false
	}

	remaining := time.Duration(e.origTTL)*time.Second - s.now().Sub(e.stored)
	if remaining < minServeTTL {
		s.c.Remove(k)
		storeEntries.Set(float64(s.c.Len()))
		return nil, false
	}

	m := e.msg.Copy()
	rcode := m.Rcode
	m.SetRcode(state.Req, rcode)
	setTTL(m, uint32(remaining.Seconds()))
	return m, true
}

func (s *store) len() int { return s.c.Len() }

func key(state request.Request) uint64 {
	name := state.Name() // already lowercased
	qtype, qclass := state.QType(), state.QClass()

	b := make([]byte, 0, len(name)+4)
	b = append(b, name...)
	b = append(b, byte(qtype>>8), byte(qtype), byte(qclass>>8), byte(qclass))
	return cache.Hash(b)
}

// minTTL is the smallest TTL across the message, which is what the whole entry
// expires on. OPT is skipped: its header TTL carries the extended rcode and
// flags, not a lifetime.
func minTTL(m *dns.Msg) (uint32, bool) {
	found := false
	var min uint32

	for _, rrs := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			if ttl := rr.Header().Ttl; !found || ttl < min {
				min, found = ttl, true
			}
		}
	}
	return min, found
}

func setTTL(m *dns.Msg, ttl uint32) {
	for _, rrs := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			rr.Header().Ttl = ttl
		}
	}
}
