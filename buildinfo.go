package main

import (
	"runtime"
	"runtime/debug"

	"github.com/coredns/coredns/coremain"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Stamped via -ldflags at build time; see the Makefile.
var (
	version  = "dev"
	revision = ""
)

// homedns_build_info reports which homedns build is running and, importantly,
// which upstream CoreDNS it was compiled against — the whole point of the
// version-bump automation is knowing that at a glance.
var buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "homedns_build_info",
	Help: "Build information for the running homedns binary. Always 1.",
}, []string{"version", "revision", "coredns_version", "goversion"})

func init() {
	buildInfo.WithLabelValues(version, vcsRevision(), coremain.CoreVersion, runtime.Version()).Set(1)
}

// vcsRevision prefers the ldflags-stamped value and falls back to the revision
// the Go toolchain embeds, so `go build` without the Makefile still reports one.
func vcsRevision() string {
	if revision != "" {
		return revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "unknown"
}
