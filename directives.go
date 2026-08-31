package main

import (
	"fmt"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/coremain"
)

const (
	blocklistDirective = "blocklist"
	gatewayDirective   = "k8s_gateway"
	peercacheDirective = "peercache"
)

func init() {
	insertBefore("cache", blocklistDirective)
	insertBefore("kubernetes", gatewayDirective)
	insertBefore("forward", peercacheDirective)
}

// insertBefore splices name into dnsserver.Directives directly before anchor.
//
// Panics when anchor is missing so that an upstream reordering fails the
// CoreDNS bump PR instead of silently misplacing the plugin in the chain.
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
