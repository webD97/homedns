// Command homedns is a custom CoreDNS distribution for a home network.
//
// It is the upstream CoreDNS binary with every stock plugin still available,
// plus two additions: the blocklist plugin in this repository (Pi-hole style
// DNS filtering) and k8s_gateway (resolves hostnames from Kubernetes Gateway
// API resources). See README.md for the reasoning.
package main

import (
	"github.com/coredns/coredns/coremain"

	// Register every plugin that ships with CoreDNS.
	_ "github.com/coredns/coredns/core/plugin"

	// Register our additions. The directives themselves are spliced into the
	// plugin chain order in directives.go.
	_ "github.com/k8s-gateway/k8s_gateway"
	_ "github.com/webd97/homedns/plugin/blocklist"
)

func main() {
	coremain.Run()
}
