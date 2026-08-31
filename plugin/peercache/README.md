# peercache

Races sibling replicas against the upstream on a local cache miss, so a name already
resolved by one replica is cheap on all of them.

Holds nothing on disk. The stock `cache` plugin keeps serving every client query; this
plugin only adds a second opinion in front of the upstream.

## Syntax

```
peercache {
    selector LABEL=VALUE[,LABEL=VALUE...]
    port     PORT
}
```

| Option | Default | Meaning |
| --- | --- | --- |
| `selector` | — | Label selector matching the pods of this deployment. Required. |
| `port` | `8053` | Port for sibling probes. Bound on the pod IP, and the port siblings are dialled on. |

## Why

Two replicas behind a LoadBalancer with `externalTrafficPolicy: Local` means clients are
pinned to whichever node their traffic lands on, so the two caches diverge permanently. A
name warm on one replica costs a full DoT round trip on the other.

On a cache miss `peercache` asks every sibling and the upstream **at the same time** and
takes the first useful answer. A sibling that holds the name replies in well under a
millisecond against 20–40 ms for DNS-over-TLS, so it usually wins.

This is not a way to reduce upstream traffic. The upstream leg is always sent, so upstream
query volume is unchanged; what it buys is latency, and resilience when the upstream is
slow or unreachable but a sibling still holds the name.

## Behaviour

**Only queries that would go upstream are raced.** The plugin sits below `cache`, `hosts`
and `k8s_gateway`, so anything answered locally never reaches it — there is nothing to
gain from asking a sibling for data it holds identically.

**A sibling that does not have the name answers `REFUSED`**, immediately, without
resolving anything. `REFUSED` classifies as an error response, so no cache anywhere keeps
it, and the leg simply drops out of the race.

**`NOERROR` and `NXDOMAIN` win the race.** `SERVFAIL`, `REFUSED`, truncation and timeouts
do not. If nothing useful arrives, the upstream leg's result is returned unchanged, so a
failure looks exactly as it would without this plugin in the chain.

**A hung sibling costs nothing.** Probes are abandoned after 500 ms, but the upstream leg
has been running the whole time, so the client never waits on a peer.

**Answers won from a peer are stored too**, which propagates a name across three or more
replicas in two hops.

## The probe listener

Probes are served on their own port, off the plugin chain, by a handler that has no next
plugin at all. That placement does four things:

- Probes are never counted by `prometheus`, logged by `log`, or measured by `blocklist`,
  so a busy replica does not inflate its sibling's query and latency graphs.
- A probe can never be relayed onwards, so `A → B → A` cannot exist and the `loop`
  plugin's startup `HINFO` probe cannot bounce between replicas into its `log.Fatalf`.
- The port is on the pod IP and in no Service, so it is not reachable from the LAN.
- Probes from a source outside the discovered peer set are refused, keeping the
  household's resolution history off the rest of the cluster network.

Names blocked by `blocklist` never reach this plugin, so they never enter the store and
can never be served to a sibling.

## The probe store

Answers this replica resolves are kept in a small fixed store (4096 entries, random
eviction) used *only* to answer siblings. It is not a second resolver cache and must not
grow into one: a wrong or stale entry costs a missed peer hit, never a wrong answer to a
client.

TTLs are rebased on the way out, so a sibling is offered the time actually remaining. An
entry with under 5 seconds left is dropped rather than served — otherwise a hot name
bounces between replicas at an ever-shrinking TTL instead of being refreshed upstream.

`DO` and `CD` queries are neither stored nor served. Keying and answering those correctly
means the DNSSEC and RFC 6840 5.7–5.8 handling that makes the stock cache large, and there
is no need to take it on: excluded queries just take the upstream leg on both sides, which
costs nothing because it was running anyway.

## Peer discovery

A Kubernetes informer watches pods matching `selector` in the pod's own namespace and
republishes the peer set on every change. Pods are not filtered on readiness: a replica
still loading its blocklist can answer probes from its store, and warming it before it
takes client traffic is worth doing.

Cluster DNS is unusable here — the Deployment sets `dnsPolicy: Default`, so a headless
Service name would not resolve from inside the pod. The API server is reachable anyway,
because in-cluster config is built from `KUBERNETES_SERVICE_HOST`, never from a name.

The plugin needs `POD_IP` from the downward API: it is both what the listener binds to and
how a replica excludes itself from its own peer set.

**Nothing here is fatal.** A missing `POD_IP`, an unreachable API server, a failed bind —
each logs a warning and leaves the peer set empty, which means every query takes the
upstream leg alone, exactly as if the plugin were absent. Losing DNS for the whole house
because the API server blipped would be a much worse failure than losing peer racing.

## Metrics

Exported when the `prometheus` plugin is enabled. Deliberately low cardinality: no
per-domain or per-client labels.

| Metric | Type | Labels |
| --- | --- | --- |
| `coredns_peercache_wins_total` | counter | `source` (`peer`, `upstream`) |
| `coredns_peercache_probes_total` | counter | `direction` (`sent`, `received`) |
| `coredns_peercache_probe_duration_seconds` | histogram | `direction` |
| `coredns_peercache_probe_failures_total` | counter | `reason` (`timeout`, `error`, `not_a_peer`) |
| `coredns_peercache_peers` | gauge | — |
| `coredns_peercache_store_entries` | gauge | — |

Share of races won by a sibling — the number that says whether this is earning its keep:

```promql
sum(rate(coredns_peercache_wins_total{source="peer"}[5m]))
  / sum(rate(coredns_peercache_wins_total[5m]))
```

`coredns_peercache_peers` dropping to 0 on a multi-replica deployment means discovery is
broken; queries still work, but every one of them pays full upstream latency.

## Example

```
peercache {
    selector app.kubernetes.io/name=homedns,app.kubernetes.io/instance=homedns
    port 8053
}
```
