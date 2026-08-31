package main

import (
	"slices"
	"testing"

	"github.com/coredns/coredns/core/dnsserver"
)

func indexOf(t *testing.T, name string) int {
	t.Helper()
	i := slices.Index(dnsserver.Directives, name)
	if i < 0 {
		t.Fatalf("directive %q missing from the chain", name)
	}
	return i
}

// The init in directives.go has already run by the time tests execute, so this
// asserts the real chain the server will build from a Corefile.
func TestDirectiveOrder(t *testing.T) {
	blocklist := indexOf(t, blocklistDirective)
	gateway := indexOf(t, gatewayDirective)
	peercache := indexOf(t, peercacheDirective)

	// Blocking must precede cache, so a refreshed list applies immediately and
	// no blocked name is ever forwarded upstream.
	if cache := indexOf(t, "cache"); blocklist >= cache {
		t.Errorf("blocklist must come before cache, got blocklist=%d cache=%d", blocklist, cache)
	}

	// ...but stay below the observability plugins, so blocked queries are still
	// counted and logged.
	for _, above := range []string{"prometheus", "errors", "log"} {
		if i := indexOf(t, above); i >= blocklist {
			t.Errorf("blocklist must come after %s, got %s=%d blocklist=%d", above, above, i, blocklist)
		}
	}

	if kubernetes := indexOf(t, "kubernetes"); gateway >= kubernetes {
		t.Errorf("k8s_gateway must come before kubernetes, got k8s_gateway=%d kubernetes=%d", gateway, kubernetes)
	}

	// peercache races the upstream, so it must sit directly in front of it and
	// below cache: every query that reaches it is already a local miss.
	if forward := indexOf(t, "forward"); peercache >= forward {
		t.Errorf("peercache must come before forward, got peercache=%d forward=%d", peercache, forward)
	}

	// Below hosts and k8s_gateway too, so names this cluster answers itself are
	// never fanned out to siblings that hold the same data.
	for _, above := range []string{"cache", "hosts", gatewayDirective, "loop"} {
		if i := indexOf(t, above); i >= peercache {
			t.Errorf("peercache must come after %s, got %s=%d peercache=%d", above, above, i, peercache)
		}
	}
}

func TestDirectivesAppearExactlyOnce(t *testing.T) {
	for _, name := range []string{blocklistDirective, gatewayDirective, peercacheDirective} {
		var n int
		for _, d := range dnsserver.Directives {
			if d == name {
				n++
			}
		}
		if n != 1 {
			t.Errorf("directive %q appears %d times, want 1", name, n)
		}
	}
}

// A missing anchor means upstream renamed or moved a directive during a version
// bump. Panicking is what turns that into a red CI run on the bump PR rather
// than a silently misplaced plugin at runtime.
func TestInsertBeforePanicsOnUnknownAnchor(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an anchor that is not in the chain")
		}
	}()
	insertBefore("no-such-upstream-directive", "irrelevant")
}

func TestInsertBeforePanicsOnDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic when re-registering an existing directive")
		}
	}()
	insertBefore("cache", blocklistDirective)
}
