package race

import (
	"github.com/coredns/coredns/plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Label values, kept as constants so a typo cannot invent a new series.
const (
	reasonError    = "error"
	reasonMismatch = "mismatch"
	reasonUnusable = "unusable"

	resultWon      = "won"
	resultFallback = "fallback"
	resultFailed   = "failed"
)

// promauto registers on prometheus.DefaultRegisterer, which is the registry
// CoreDNS's metrics plugin serves. Labels are deliberately low cardinality: `to`
// is bounded by the Corefile, and there are no per-domain or per-client labels.
//
// Per-upstream latency needs nothing here. pkg/proxy already observes
// coredns_proxy_request_duration_seconds{proxy_name="race",to,rcode} on every
// leg, which is what the dashboard charts.
var (
	winsCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "wins_total",
		Help:      "Counter of queries answered, by the upstream that got there first.",
	}, []string{"to"})

	legFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "leg_failures_total",
		Help:      "Counter of upstream legs that did not produce a usable answer.",
	}, []string{"to", "reason"})

	staleConns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "stale_conns_total",
		Help:      "Counter of pooled connections found already closed by the upstream and retried.",
	}, []string{"to"})

	queriesCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: pluginName,
		Name:      "queries_total",
		Help:      "Counter of queries handled, by how they were resolved.",
	}, []string{"result"})
)
