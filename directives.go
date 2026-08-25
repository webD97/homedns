package main

import (
	"fmt"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/coremain"
)

// Directive names contributed by this distribution.
const (
	blocklistDirective = "blocklist"
	gatewayDirective   = "k8s_gateway"
)

// CoreDNS keeps the plugin chain order in dnsserver.Directives, a plain
// []string that it reads back at Corefile-parse time (register.go hands caddy a
// `func() []string { return Directives }` rather than a snapshot). So splicing
// our directives in from an init here is both safe and correctly ordered: the
// imported packages' inits have already filled the slice by the time this runs,
// and nothing has read it yet.
func init() {
	// Blocking belongs before cache, so a refreshed list takes effect
	// immediately and blocked names never reach an upstream. It stays below
	// prometheus/log/errors so blocked queries are still counted and logged.
	insertBefore("cache", blocklistDirective)

	// Sits with the other Kubernetes-backed sources.
	insertBefore("kubernetes", gatewayDirective)
}

// insertBefore splices name into dnsserver.Directives directly before anchor.
//
// Panicking on a missing anchor is deliberate. The anchors are upstream CoreDNS
// directive names, so a rename or reordering during a version bump would
// otherwise leave our plugins silently misplaced in the chain — and a blocklist
// that runs after cache is exactly the kind of bug nobody notices. This turns it
// into a loud failure on the bump PR instead.
func insertBefore(anchor, name string) {
	for _, d := range dnsserver.Directives {
		if d == name {
			panic(fmt.Sprintf("homedns: directive %q registered twice", name))
		}
	}

	for i, d := range dnsserver.Directives {
		if d != anchor {
			continue
		}
		spliced := make([]string, 0, len(dnsserver.Directives)+1)
		spliced = append(spliced, dnsserver.Directives[:i]...)
		spliced = append(spliced, name)
		spliced = append(spliced, dnsserver.Directives[i:]...)
		dnsserver.Directives = spliced
		return
	}

	panic(fmt.Sprintf(
		"homedns: cannot place %q — upstream directive %q is not in CoreDNS %s's chain; "+
			"the plugin order changed upstream and directives.go needs updating",
		name, anchor, coremain.CoreVersion))
}
