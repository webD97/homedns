# race

Sends every query to all of its upstreams at once and answers with the first useful reply,
in place of `forward` picking one.

## Syntax

```
race FROM TO... {
    tls_servername NAME
    expire         DURATION
}
```

`FROM` is the zone to handle, `TO...` two or more upstream addresses. `dns://` (default)
and `tls://` are supported; each gets the default port for its transport, so
`tls://9.9.9.11` means `9.9.9.11:853`.

| Option | Default | Meaning |
| --- | --- | --- |
| `tls_servername` | — | Server name to validate `tls://` upstream certificates against. Without it they are validated against the upstream IP, which needs a certificate carrying an IP SAN. |
| `expire` | `30s` | How long an idle upstream connection is kept. Three times CoreDNS's own default. |

Fewer than two upstreams is an error: there is nothing to race, and `forward` is the plugin
for that.

## Why

`forward`'s default policy is `random` — one upstream per query — so a resolver having a bad
moment sets the client's latency roughly half the time. Measured against this deployment's
two Quad9 anycast addresses, 590 paired queries sent to both simultaneously:

| | p50 | p90 | p95 | p99 |
| --- | --- | --- | --- | --- |
| `9.9.9.11` | 20 ms | 39 ms | 126 ms | 635 ms |
| `149.112.112.11` | 16 ms | 33 ms | 65 ms | 180 ms |
| whichever answered first | 16 ms | 25 ms | 33 ms | 107 ms |

Per-query correlation between the two was **r = 0.064**: same operator, same anycast
network, and still effectively independent. Of the 17 queries where either upstream took
over 200 ms, **none** were slow on both. The tail is not a property of "the upstream", it is
a property of *one* upstream at *one* moment, and asking two at once removes it.

This costs upstream query volume: **N upstreams means N times the queries.** That is the
whole trade, and it is the opposite of what `peercache` promises. On a home network doing
under one query per second it is free; on anything busier, price it first.

A side effect worth knowing: every upstream now sees every miss instead of `1/N` of them,
so connection pools stay warmer and fewer queries pay a TLS handshake.

## Behaviour

**`NOERROR` and `NXDOMAIN` win.** `SERVFAIL`, `REFUSED`, truncation, timeouts and replies
that answer a different question do not — those are exactly the cases another upstream may
still do better on.

**If nothing useful arrives, an upstream's own response is returned** unchanged, so a
failure looks as it would under `forward`. Only when no upstream answers at all is a
`SERVFAIL` synthesized. A truncated reply keeps its `TC` bit on the way out, so if every
upstream truncated, the client retries over TCP exactly as it would under `forward`.

**A fast failure is slower here than under `forward`.** `forward` returns a `SERVFAIL` from
its chosen upstream immediately; `race` waits to see whether another upstream can do
better. That is the intended trade, but it is a regression on that one path.

**Losing legs are neither cancelled nor waited for.** They run to completion, which is what
returns their connection to the pool for the next query. They hold no reference to the
client's connection, so finishing late is harmless.

**No health checking.** Every upstream is asked every time, so the query itself is the
health check: an upstream that is down simply never wins.

## Metrics

Exported when the `prometheus` plugin is enabled. Deliberately low cardinality: `to` is
bounded by the Corefile, and there are no per-domain or per-client labels.

| Metric | Type | Labels |
| --- | --- | --- |
| `coredns_race_wins_total` | counter | `to` |
| `coredns_race_leg_failures_total` | counter | `to`, `reason` (`error`, `mismatch`, `unusable`) |
| `coredns_race_queries_total` | counter | `result` (`won`, `fallback`, `failed`) |

Per-upstream latency needs nothing extra: `pkg/proxy` already reports
`coredns_proxy_request_duration_seconds{proxy_name="race",to,rcode}` and
`coredns_proxy_conn_cache_{hits,misses}_total` for every leg.

Whether both upstreams are earning their place — one winning nearly everything means the
other is only paying for itself in traffic:

```promql
sum by (to) (rate(coredns_race_wins_total[5m]))
```

How often a connection had to be dialled rather than reused:

```promql
sum(rate(coredns_proxy_conn_cache_misses_total[5m]))
  / sum(rate(coredns_proxy_conn_cache_hits_total[5m]) + rate(coredns_proxy_conn_cache_misses_total[5m]))
```

`coredns_race_queries_total{result="failed"}` above zero means every upstream was
unreachable — the only case where the client gets a synthesized failure.

## Example

```
race . tls://9.9.9.11 tls://149.112.112.11 {
    tls_servername dns.quad9.net
    expire 30s
}
```
