// Command homedns is a custom CoreDNS distribution for a home network.
// See AGENTS.md for how the build is put together.
package main

import (
	"github.com/coredns/coredns/coremain"

	_ "github.com/coredns/coredns/core/plugin"
	_ "github.com/k8s-gateway/k8s_gateway"
	_ "github.com/webd97/homedns/plugin/blocklist"
	_ "github.com/webd97/homedns/plugin/peercache"
)

func main() {
	coremain.Run()
}
