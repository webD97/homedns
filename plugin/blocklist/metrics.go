package blocklist

import (
	"github.com/coredns/coredns/plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics land on prometheus.DefaultRegisterer, which is exactly what CoreDNS's
// metrics plugin serves on /metrics (it takes DefaultRegisterer as its own
// registry), so promauto at package level is all that's needed.
//
// Everything here is deliberately low cardinality: counters carry the standard
// `server` label and per-source gauges are keyed by list URL, of which there
// are a handful. No per-domain or per-client labels.
var (
	blockedCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "blocked_total",
		Help:      "Counter of DNS queries answered NXDOMAIN because of the blocklist.",
	}, []string{"server"})

	allowedCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "allowed_total",
		Help:      "Counter of DNS queries checked against the blocklist and passed through.",
	}, []string{"server"})

	domainsTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "domains_total",
		Help:      "Number of blocked domain subtrees currently loaded, after dedup.",
	})

	sourceDomains = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "source_domains",
		Help:      "Number of entries parsed from each blocklist source, before dedup.",
	}, []string{"source"})

	sourceLastSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "source_last_success_timestamp_seconds",
		Help:      "Unix timestamp of the last successful fetch of each blocklist source.",
	}, []string{"source"})

	sourceFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "source_update_failures_total",
		Help:      "Counter of failed fetches per blocklist source.",
	}, []string{"source"})

	reloadDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "reload_duration_seconds",
		Help:      "Histogram of the time taken to fetch and rebuild the full blocklist.",
		Buckets:   prometheus.ExponentialBuckets(0.5, 2, 8), // 0.5s .. ~64s
	})

	failOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "fail_open",
		Help:      "1 while serving queries unfiltered because no blocklist source loaded in time.",
	})
)
