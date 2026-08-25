# blocklist

Answers `NXDOMAIN` for domains named by downloaded blocklists, the way Pi-hole does.

Lists are held in memory only and re-downloaded on an interval, so the plugin needs no
persistent storage.

## Syntax

```
blocklist {
    url           URL...
    bootstrap_dns IP|IP:PORT...
    allow         DOMAIN...
    refresh       DURATION
    ready_timeout DURATION
}
```

| Option | Default | Meaning |
| --- | --- | --- |
| `url` | — | A blocklist to download. Repeatable, and accepts several URLs per line. At least one is required. |
| `bootstrap_dns` | `1.1.1.1 9.9.9.9` | Resolvers used to look up the `url` hostnames. Must be literal IPs. |
| `allow` | — | Names never to block. Each covers the name and its subdomains, and wins over any block. |
| `refresh` | `24h` | How often to re-download. Minimum `1m`. |
| `ready_timeout` | `60s` | How long to report not-ready waiting for a first list before serving unfiltered. |

## Behaviour

**Blocked names get a plain `NXDOMAIN`**, authoritative, with no SOA — matching Pi-hole
and dnsmasq. Clients pick their own negative-caching TTL, so an `allow` entry added later
takes effect immediately.

**A listed domain blocks itself and all its subdomains.** There is one matching rule and
no flag to change it; it is what the lists themselves intend (oisd's header reads *"All
domains and their subdomains should be blocked"*) and what people expect. Matching is on
label boundaries, so listing `tracker.net` does not block `mytracker.net`.

**The plugin must run before `cache`**, which the binary arranges automatically — see
`directives.go`. Blocked names never reach an upstream, and a refreshed list applies at
once.

## Bootstrap DNS

This process *is* the network's resolver, so resolving `raw.githubusercontent.com`
through itself is circular. Inside Kubernetes the pod's `/etc/resolv.conf` points at
cluster DNS, which in this deployment may forward straight back here. Fetches therefore
never use the ambient resolver: they dial `bootstrap_dns` directly.

That is also why `bootstrap_dns` refuses hostnames — resolving one is the exact problem
it exists to avoid.

## List formats

Both common shapes are detected per line; the caller declares nothing.

```
0.0.0.0 ads.example.com        # /etc/hosts format (StevenBlack, ...)
*.ads.example.com              # bare domain list, wildcard (oisd, ...)
ads.example.com                # bare domain list (hagezi, ...)
```

Comments (`#` to end of line) are stripped, entries are lowercased, and a trailing `.`,
a leading `*.` and a leading `.` are removed. Dropped as unblockable: bare IPs,
single-label names, the twelve well-known local names an `/etc/hosts` preamble maps
(`localhost`, `broadcasthost`, `ip6-allnodes`, …), and anything that is not a valid
query name — which is what keeps Adblock syntax like `||ads.example.com^` from being
stored as a domain that can never match.

Verified against the live lists: this parser produces exactly the entry count each one
declares in its own header (82,561 for StevenBlack; 269,005 for oisd big). `make
test-live` re-runs that check, and a scheduled workflow does it weekly — a publisher
changing format is otherwise a silent failure, since entries still load and the domain
count still looks healthy while nothing matches.

## Startup and failure behaviour

The plugin implements `Ready()`, so CoreDNS's `ready` plugin reports 503 until a list has
loaded and a Kubernetes readiness probe keeps traffic away from a replica that would
answer unfiltered.

After `ready_timeout` it reports ready **anyway** and serves unfiltered, setting
`coredns_blocklist_fail_open` to 1 and logging a warning. For a home network's only
resolver, losing ad blocking is a better failure than losing DNS.

A source that fails to refresh keeps its last good contents, so one dead URL never empties
the list. A `200` response that parses to zero domains is treated as a failure too — that
is an error page or a moved list, not an empty blocklist.

## Metrics

Exported when the `prometheus` plugin is enabled. Deliberately low cardinality: no
per-domain or per-client labels.

| Metric | Type | Labels |
| --- | --- | --- |
| `coredns_blocklist_blocked_total` | counter | `server` |
| `coredns_blocklist_allowed_total` | counter | `server` |
| `coredns_blocklist_domains_total` | gauge | — |
| `coredns_blocklist_source_domains` | gauge | `source` |
| `coredns_blocklist_source_last_success_timestamp_seconds` | gauge | `source` |
| `coredns_blocklist_source_update_failures_total` | counter | `source` |
| `coredns_blocklist_reload_duration_seconds` | histogram | — |
| `coredns_blocklist_fail_open` | gauge | — |

Percent blocked:

```promql
sum(rate(coredns_blocklist_blocked_total[5m]))
  / sum(rate(coredns_blocklist_blocked_total[5m]) + rate(coredns_blocklist_allowed_total[5m]))
```

Worth alerting on `coredns_blocklist_fail_open == 1` and on
`time() - coredns_blocklist_source_last_success_timestamp_seconds > 172800`.

## Examples

```
blocklist {
    url https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
    url https://big.oisd.nl/domainswild
    bootstrap_dns 1.1.1.1 9.9.9.9
    allow analytics.mycompany.example
    refresh 24h
}
```

## Memory

Entries are held as two sorted string slices and looked up with binary search over the
query name's parent suffixes. Redundant names — those already covered by a listed
ancestor — are pruned at load.

StevenBlack and oisd big together are 327,255 unique names, 287,060 after pruning, about
10 MB resident.
