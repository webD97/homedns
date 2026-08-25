package blocklist

import (
	"slices"
	"sync"
	"testing"
)

func TestNameSetCoversSubtrees(t *testing.T) {
	s := newNameSet([]string{"ads.example.com", "tracker.net"})

	for _, name := range []string{
		"ads.example.com",       // the entry itself
		"img.ads.example.com",   // direct child
		"a.b.c.ads.example.com", // deep descendant
		"tracker.net",           //
		"cdn.tracker.net",       //
	} {
		if !s.covers(name) {
			t.Errorf("covers(%q) = false, want true", name)
		}
	}

	for _, name := range []string{
		"example.com",               // parent of an entry is not itself blocked
		"com",                       //
		"notads.example.com",        // sibling
		"ads.example.com.evil.test", // entry appearing as a prefix label
		"xads.example.com",          // label suffix, not a label boundary
		"my-tracker.net",            //
		"",                          //
	} {
		if s.covers(name) {
			t.Errorf("covers(%q) = true, want false", name)
		}
	}
}

// Every entry denotes a subtree, so a child of a listed parent is redundant.
// On the real lists this prunes 45% of StevenBlack.
func TestNewNameSetPrunesAndDedups(t *testing.T) {
	s := newNameSet([]string{
		"example.com",
		"example.com", // exact duplicate
		"ads.example.com",
		"deep.ads.example.com",
		"other.net",
	})

	want := []string{"example.com", "other.net"}
	if !slices.Equal([]string(s), want) {
		t.Errorf("got %v, want %v", s, want)
	}
	// Pruning must not change what the set answers.
	for _, n := range []string{"example.com", "ads.example.com", "deep.ads.example.com"} {
		if !s.covers(n) {
			t.Errorf("pruning lost coverage of %q", n)
		}
	}
}

func TestNewNameSetEmpty(t *testing.T) {
	if s := newNameSet(nil); s != nil {
		t.Errorf("got %v, want nil", s)
	}
}

func TestMatcherAllowBeatsBlock(t *testing.T) {
	m := &matcher{
		block: newNameSet([]string{"example.com"}),
		allow: newNameSet([]string{"good.example.com"}),
	}

	cases := map[string]bool{
		"example.com":          true,
		"bad.example.com":      true,
		"good.example.com":     false, // carved out
		"sub.good.example.com": false, // allow is a subtree too
		"nogood.example.com":   true,  // label-boundary check applies to allow
		"unrelated.net":        false,
	}
	for name, want := range cases {
		if got := m.blocked(name); got != want {
			t.Errorf("blocked(%q) = %v, want %v", name, got, want)
		}
	}
}

// The handler reads m.current before the first fetch completes.
func TestNilMatcherBlocksNothing(t *testing.T) {
	var m *matcher
	if m.blocked("ads.example.com") {
		t.Error("nil matcher must not block")
	}
	if m.size() != 0 {
		t.Error("nil matcher must report size 0")
	}
}

func TestNormalizeQuery(t *testing.T) {
	for in, want := range map[string]string{
		"Ads.Example.COM.": "ads.example.com",
		"ads.example.com":  "ads.example.com",
		".":                "",
	} {
		if got := normalizeQuery(in); got != want {
			t.Errorf("normalizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// Refreshes publish a new matcher while queries are in flight. Run with -race.
func TestMatcherConcurrentReadDuringSwap(t *testing.T) {
	b := &Blocklist{}
	b.current.Store(&matcher{block: newNameSet([]string{"a.example.com"})})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.current.Load().blocked("x.a.example.com")
				}
			}
		}()
	}

	for i := 0; i < 200; i++ {
		b.current.Store(&matcher{block: newNameSet([]string{"a.example.com", "b.example.net"})})
	}
	close(stop)
	wg.Wait()
}
