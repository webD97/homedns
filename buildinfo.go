package main

import (
	"runtime"
	"runtime/debug"

	"github.com/coredns/coredns/coremain"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Stamped via -ldflags; see the Makefile.
var (
	version  = "dev"
	revision = ""
)

var buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "homedns_build_info",
	Help: "Build information for the running homedns binary. Always 1.",
}, []string{"version", "revision", "coredns_version", "goversion"})

func init() {
	buildInfo.WithLabelValues(version, vcsRevision(), coremain.CoreVersion, runtime.Version()).Set(1)
}

// vcsRevision prefers the ldflags value, falling back to what the toolchain
// embeds so a plain `go build` still reports something.
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
