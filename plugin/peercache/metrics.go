package peercache

import (
	"github.com/coredns/coredns/plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// promauto registers on prometheus.DefaultRegisterer, which is the registry
// CoreDNS's metrics plugin serves. Labels are deliberately low cardinality.
var (
	winsCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "wins_total",
		Help:      "Counter of races won, by which leg answered first.",
	}, []string{"source"})

	probesCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "probes_total",
		Help:      "Counter of peer probes sent to and received from sibling replicas.",
	}, []string{"direction"})

	probeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "probe_duration_seconds",
		Help:      "Histogram of peer probe round trips (sent) and time to answer a sibling (received).",
		Buckets:   plugin.TimeBuckets,
	}, []string{"direction"})

	probeFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "probe_failures_total",
		Help:      "Counter of peer probes that did not produce a usable answer.",
	}, []string{"reason"})

	peersTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "peers",
		Help:      "Number of sibling replicas currently discovered.",
	})

	storeEntries = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "store_entries",
		Help:      "Number of answers held in the probe store.",
	})
)
