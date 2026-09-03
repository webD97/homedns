# AGENTS.md

Orientation for agents working in this repo. README.md is for users; this is the
why, the traps, and the things that are easy to get wrong.

## What this is

One DNS server for a home LAN, replacing Pi-hole (blocklists), external-dns
(Gateway API), and a standalone CoreDNS (forwarding + static records). Deployed
in-cluster with a LoadBalancer on :53.

The moving parts, very different in weight:

- **`plugin/blocklist/`** — written here. This is where the work is.
- **`plugin/peercache/`** — written here. Off by default in the chart.
- **`plugin/race/`** — written here. Off by default in the chart. Replaces
  `forward` rather than sitting in front of it.
- **Gateway API** — a dependency on `github.com/k8s-gateway/k8s_gateway`, not
  our code. Configuration lives in the chart. Don't reimplement it; if something
  is missing, check whether upstream already supports it.

## Build architecture

Not a CoreDNS fork. Own Go module importing CoreDNS as a library, so tracking
upstream is `go get github.com/coredns/coredns@vX.Y.Z`. This works because of
three upstream facts, all worth re-checking if a bump breaks:

1. `github.com/coredns/coredns/core/plugin` is importable and its blank imports
   register every stock plugin.
2. `core/dnsserver.Directives` is a plain `var []string` — mutable.
3. `core/dnsserver/register.go` hands caddy
   `Directives: func() []string { return Directives }`, evaluated at
   Corefile-parse time rather than captured at init. That is what makes
   mutating it from `package main`'s `init()` safe.

`directives.go` splices `blocklist` before `cache`, `k8s_gateway` before
`kubernetes`, and `peercache` then `race` before `forward` — in that order,
because the last insert before an anchor takes the slot nearest it, which is
what puts `race` between the two. `insertBefore` **panics on a missing anchor** — deliberate: a blocklist that silently runs after `cache` is
the kind of bug nobody notices, so an upstream reorder must fail the CoreDNS
bump PR instead.

Placement reasoning, if you ever move them:

- `blocklist` before `cache` — blocked names never reach an upstream, and a
  refreshed list applies immediately rather than after cache expiry.
- `blocklist` after `prometheus`/`log`/`errors` — blocked queries still get
  counted and logged.
- `blocklist` before `hosts` — the integration test depends on this; see below.
- `peercache` before `forward`, after `cache`/`hosts`/`k8s_gateway` — it races
  the upstream, so every query reaching it must already be a local miss that
  nothing in the cluster can answer. Asking a sibling for a name `hosts` or
  `k8s_gateway` holds identically is pure waste.
- `race` between `peercache` and `forward` — it does `forward`'s job, so it takes
  `forward`'s place, and `peercache`'s upstream leg is what runs through it.

## blocklist: decisions that look arbitrary but aren't

**Bootstrap DNS is load-bearing, not a nicety.** This process *is* the resolver,
so resolving `raw.githubusercontent.com` through itself is circular; in-cluster
`/etc/resolv.conf` points at kube-dns, which in this deployment may forward back
here. Fetches dial configured IPs directly. `PreferGo: true` on the `net.Resolver`
is **required** — the cgo resolver ignores the `Dial` hook and silently falls
back to the system resolver. `bootstrap_dns` rejects hostnames for the same
reason. The Deployment also sets `dnsPolicy: Default`.

**Two parsing traps, both silent failures.** Both were found by running the
parser against the real files, and both produce a healthy-looking domain count
while nothing matches:

- oisd prefixes *every* entry with `*.`. Not stripping it loads 269k
  unmatchable entries.
- Other list syntaxes look like bare domains. `||ads.example.com^` is a single
  dot-containing field; `isDomainName` is what rejects it.

Also non-negotiable: the `localNames` set. An `/etc/hosts` preamble maps
`localhost`, `local`, `broadcasthost`, the `ip6-*` names, and a literal
`0.0.0.0 0.0.0.0`. Taking field 1 of every IP-led line blocks localhost.

**Every entry is a subtree, and there is no flag to change it.** It is what the
lists intend (oisd's header: *"All domains and their subdomains should be
blocked"*) and what users expect. Verified that neither list contains a public
suffix or a major domain apex, so promoting entries to subtrees does not
over-block. Matching is on label boundaries — `tracker.net` must not block
`mytracker.net`, and there is a test for it.

**Fail-open is a deliberate policy, not a bug.** No PV means a cold pod has no
list. `Ready()` returns false until one loads, so the LoadBalancer withholds
traffic; after `ready_timeout` it reports ready anyway, serves unfiltered, and
sets `coredns_blocklist_fail_open`. For a home network's only resolver, losing
ad blocking beats losing DNS. Don't "fix" this into failing closed.

**Never let the list go empty.** A failed refresh keeps `lastGood`; a 200 that
parses to zero domains is treated as a failure (it is an error page or a moved
list). One dead URL must not disable filtering.

**Metrics are aggregates only.** No per-domain or per-client labels — Pi-hole's
"top 10 clients" panel would mean unbounded cardinality. This was an explicit
decision; don't add label dimensions without raising it.

## peercache: the two decisions everything else follows from

**The probe listener is off the plugin chain, on its own port.** It would have
been less code to answer sibling probes on `:53` and read them out of the stock
`cache`. That was rejected, and both reasons matter:

- Probes on `:53` traverse `prometheus`, `log` and `blocklist` before reaching
  `cache`, and nothing distinguishes them from client queries. A busy replica
  would permanently inflate its sibling's `coredns_dns_requests_total`, drag its
  latency percentiles toward zero (probes answer in microseconds), and *deflate
  the blocked share* — the Pi-hole headline number — because a probe is always
  an allowed query at the receiver. Six dashboard panels would have needed
  subtraction terms forever.
- A chain-attached probe handler can forward. Two replicas pointed at each other
  then bounce `loop`'s startup HINFO probe back and forth, and on the third pass
  `plugin/loop` calls **`log.Fatalf`** and kills the pod. The off-chain listener
  has no `Next` and structurally cannot relay, so the loop cannot exist. There
  is a test asserting the chain is never entered; keep it.

**The probe store is not a second resolver cache.** The stock `cache` plugin
still serves every client query. `peercache`'s store exists only to answer
siblings, which is why it can be dumb: `DO` and `CD` queries are neither stored
nor served, sidestepping the DNSSEC and RFC 6840 5.7-5.8 AD-bit handling that
makes the stock cache ~1200 lines. That is safe precisely because a store miss
costs nothing — the upstream leg is racing regardless. Don't grow this into a
cache; if it needs prefetch or serve-stale, the answer is that the stock plugin
already has them.

**Entries below 5s remaining are dropped, not served.** Otherwise a hot name
ping-pongs between replicas at an ever-shrinking TTL and never gets refreshed.

**The losing leg must not hold the live ResponseWriter.** When a peer wins,
`ServeDNS` returns while the upstream leg is still running; the server may
cancel the context and reuse the connection at that moment.
`plugin/pkg/nonwriter` embeds the real writer, so it is *not* safe here —
`internal/detached.Writer` delegates nothing and answers `LocalAddr`/`RemoteAddr`
from values snapshotted before the goroutines launch. It lives in `internal/`
because `race` needs the same guarantee.

**Discovery is Kubernetes-only, and never fatal.** Cluster DNS cannot be used:
the Deployment sets `dnsPolicy: Default`, so a headless Service name does not
resolve from inside the pod. The API server is reachable anyway because
`rest.InClusterConfig` builds its URL from `KUBERNETES_SERVICE_HOST`, never from
a name. A missing `POD_IP`, an unreachable API server or a failed bind logs a
warning and leaves the peer set empty — every query then takes the upstream leg
alone, exactly as without the plugin. Same reasoning as blocklist's fail-open:
losing DNS for the house because the API server blipped is the worse failure.
CI depends on this too, booting the `peerCache.enabled=true` Corefile outside a
cluster.

**The listener retries its bind.** The `reload` plugin re-runs setup in place,
so `OnStartup` can fire before the previous listener has released the port.
Shutdown closes the `PacketConn` rather than calling `dns.Server.Shutdown`,
which races a server that has not started serving yet.

**It is a latency feature, not a traffic one.** The race always sends the
upstream leg, so upstream QPS is unchanged. What it buys is latency and
resilience. Anyone "optimising" this into peers-first-then-upstream is changing
the tradeoff, not fixing a bug.

## race: the decisions everything else follows from

**It exists because the tail belongs to *one* upstream, not to "the upstream".**
`forward`'s default policy is `random`, so a resolver having a bad moment sets
client latency about half the time. That is only worth fixing if the upstreams
fail independently, and two anycast addresses of the same operator are a
reasonable place to expect they do not — so it was measured before anything was
written. 590 paired queries, the same name sent to both Quad9 addresses at once:

| | p50 | p90 | p95 | p99 |
| --- | --- | --- | --- | --- |
| `9.9.9.11` | 20 ms | 39 ms | 126 ms | 635 ms |
| `149.112.112.11` | 16 ms | 33 ms | 65 ms | 180 ms |
| first to answer | 16 ms | 25 ms | 33 ms | 107 ms |

Per-query correlation r = 0.064, and of the 17 queries where either leg exceeded
200 ms, none were slow on both. Redo this measurement before changing the
upstream list — it is the only thing that says whether the plugin earns its
N-times-the-traffic cost.

**Aggregate correlation is the wrong statistic, and it says the opposite.**
Correlating the two upstreams' 10-minute slow-query *rates* out of Prometheus
gives r = 0.73, which reads as "their tails move together, this will not help".
That is a shared query mix — both resolvers meet the same hard names in the same
window — not per-query dependence. It nearly killed the plugin. Only paired
queries answer the question.

**The reconnect rate is not the tail.** 32% of upstream queries miss the
connection cache, which looks like the culprit until you notice `forward`
already enables TLS session resumption (`forward/setup.go`), so a miss costs a
*resumed* handshake. Mean (40 ms) against p50 (26 ms) puts it at ~30 ms on a
third of queries: ~10 ms of mean, invisible at p99. `expire` is 30s because it
is free, not because it fixes anything. A keepalive ticker was designed and
dropped for the same reason — `Transport.Dial` + `Yield` refreshes a pooled
connection with no query at all, and both are exported — but it buys mean, not
tail, and it muddies the dial-rate metric that would justify it. Build it only
if that panel still shows misses.

**Losing legs are never cancelled.** `pkg/proxy`'s `lookupDNS` ignores its
context entirely — the parameter is literally `_ctx` — so cancelling buys
nothing, and a leg that runs to completion is what hands its connection back to
the pool via `transport.Yield`. Aborting losers would close connections instead
of reusing them, which is backwards.

**Three things `pkg/proxy` will not do for you**, each silent when missed:
`Connect` mutates `state.Req.Id`, so every leg needs its own `r.Copy()`;
`ClientSessionCache` is set by `forward`, not by `pkg/tls` or `pkg/proxy`, so
omitting it downgrades every reconnect to a full handshake; and `ErrCachedClosed`
is retried by `forward.ServeDNS`, not by `Connect`, so a caller that does not
loop on it fails a query whenever the remote closed a pooled connection.

**No health checking.** `forward` skips proxies that are `Down()`; `race` asks
all of them every time, so the query *is* the health check. `Start()` is still
called on each proxy, but only because it also starts the transport's connection
manager — `Healthcheck()` never is, so `up.Probe` sends no traffic.

**A fast failure is slower than under `forward`**, which returns its chosen
upstream's `SERVFAIL` at once while `race` waits to see if another does better.
Intended, and the one path where this is a regression.

**Two different races.** The `race` plugin is the upstream contest; `PeerCache.race`
is the sibling one. With both enabled a query that misses everything generates one
peer probe plus N upstream queries.

## Traps that already cost time

- **`caddy.Controller.NextArg()` returns the `{` token.** `RemainingArgs()` is
  the one that stops at a brace and rolls back. Every "takes no arguments" check
  must use `RemainingArgs`.
- **In-place dedup aliasing** in `newNameSet`: reading `sorted[i-1]` is wrong
  because that slot may already have been overwritten by an earlier append.
  Track the previous value in a local. Pruning likewise writes into a fresh
  slice, because pruning in place would corrupt the set `hasAncestor` searches.
- **`prometheus.DefaultRegisterer` is what gets served.** CoreDNS's metrics
  plugin takes it as its own registry, so package-level `promauto` just works —
  no manual registration needed.
- **`strings.Builder` has no `ReadFrom`.** Use `io.ReadAll`.
- **`coredns -plugins` output is alphabetical**, not chain order. It confirms
  registration only. Use `TestDirectiveOrder` to check ordering.

## Testing strategy

Four layers, each covering something the others can't:

1. **Unit** — parsing, matching, lifecycle. Run with `-race`; the matcher swap
   test exists specifically for it.
2. **Golden counts** (`TestParseListFixtures`) — checked-in excerpts in
   `testdata/` carrying every shape found in the real files.
3. **Live** (`TestLiveLists`, `HOMEDNS_LIVE_LISTS=1`) — downloads the real lists
   and asserts the parse matches the count each publisher declares in its own
   header. This is the oracle that caught the `*.` bug. A weekly workflow runs
   it, because a format change is otherwise invisible.
4. **Integration** (`test/`) — builds the binary, serves a Corefile, queries
   over the wire.

**Do not remove the `hosts` entries in the integration test's Corefile.** They
shadow every blocked name, so an NXDOMAIN proves the blocklist acted rather than
the name simply not existing. Without them the test passes for the wrong reason.

The chart is also tested by rendering its Corefile and booting the real binary
against it — a chart that renders valid YAML but an invalid Corefile is the
failure mode worth catching.

## Load testing

`make loadtest` (or `scripts/loadtest.sh`) starts the binary with two real
blocklists, fires **every** domain on them at it, and prints the metrics. It is
a correctness check as much as a throughput one: a domain that answers anything
but NXDOMAIN was parsed into a form that cannot match.

Measured on a 16-core dev box, 576,100 domains (Pi-hole-Optimized comprehensive
+ StevenBlack), 48 workers:

| | |
| --- | --- |
| list load | 1.6s for 612k raw → 421,894 subtrees after pruning |
| throughput | ~124k q/s, all NXDOMAIN |
| latency | p50 371µs, p99 1.35ms |
| RSS | ~150 MB steady |

RSS scales with list size but is dominated by the Go runtime and parse
transients rather than the matcher itself (which is ~10 MB for this set):

| lists | subtrees | RSS |
| --- | --- | --- |
| StevenBlack only (chart default) | 45,675 | ~73 MB |
| comprehensive + StevenBlack | 421,894 | ~150 MB |

The chart requests 128Mi and limits 512Mi off the back of this. The **limit** is
the number that matters: a refresh transiently holds the raw list text, the
parsed names, and the rebuilt sorted set at once, so peak runs well above steady
state. Adding large lists needs the limit raised, not just the request.

Two gotchas the script itself had to learn:

- **Its query filter must mirror `parse.go` exactly**, including the
  `localNames` rejection. Otherwise it asks about `localhost.localdomain`, which
  the plugin deliberately never blocks, and reports a failure that isn't one.
- **`dig -f` is not viable** as a load generator: it is sequential and took over
  two minutes for 2,000 queries, which extrapolates to ~10 hours here. Hence
  `scripts/loadgen/`.

## The hosts plugin also reads /etc/hosts

CoreDNS's `hosts` plugin defaults to `/etc/hosts` when given no FILE, *in
addition* to any inline entries. The chart's generated Corefile uses the inline
form, so a deployed pod also answers from the container's own `/etc/hosts`
(localhost entries and the pod IP). Harmless in practice, but it is why a name
can resolve that no chart value mentions — that is how the load test's one
mismatch produced NOERROR rather than SERVFAIL. This holds on the `scratch`
image too: the file comes from the container runtime, not the image.

## Container image

`FROM scratch` with exactly two files: the binary and a CA bundle. Verified with
`podman export | tar -t` — if that listing ever grows, something crept in.

It was `gcr.io/distroless/static-debian12:nonroot`, which is 964 files of which
we used one. The CA bundle is still copied *from* distroless in a throwaway
`certs` stage rather than from the builder, because distroless is rebuilt on
every `ca-certificates` update while `golang:1.26-bookworm` carries whatever
Debian snapshot that tag was built from. Dependabot's docker ecosystem tracks
both `FROM` lines.

Consequences of scratch, all deliberate:

- **`USER` must be numeric** (`65532:65532`). There is no `/etc/passwd` to
  resolve a name against, and kubelet can only enforce `runAsNonRoot` against a
  numeric UID anyway. Must stay in step with `podSecurityContext.runAsUser`.
- **No `/tmp`.** Nothing here writes to it, and the chart sets
  `readOnlyRootFilesystem: true`, so it would be unwritable regardless.
- **No tzdata**, so `time.Local` is UTC. This costs nothing: CoreDNS's
  `coremain.LogFlags` defaults to `0`, so it prints no timestamps at all, and
  nothing in the tree formats local time. Embedding `time/tzdata` was tried and
  reverted — 414 KB for no observable behaviour. Re-check this only if something
  starts emitting wall-clock time.
- **`/etc/hosts` and `/etc/resolv.conf` still exist at runtime** — the container
  runtime bind-mounts them and creates the mount points. Confirmed on scratch:
  querying `localhost` still answers `127.0.0.1` from the `hosts` plugin.

The CA bundle is the one thing a broken build breaks silently: without it the
blocklist fetch fails TLS, every source errors, and the plugin fails open after
`ready_timeout`. `SSL_CERT_FILE` is set explicitly — redundant, since Go probes
that path anyway, but it makes the dependency greppable.

## Statelessness

No PV, and no volume beyond the Corefile ConfigMap. Blocklists live in memory
and are re-downloaded; Gateway API records are rebuilt from informers;
`peercache`'s probe store is memory-only and simply starts empty; counters reset
(use `increase(...[24h])` for Pi-hole-style daily totals). If you add anything
needing disk, that is a design change — raise it rather than adding a PVC.

## Releases

`release.yml` publishes on a **bare SemVer tag** — `0.2.0`, not `v0.2.0` — or on
demand via `workflow_dispatch`. Never on a branch push. `ci.yml` builds the
multi-arch image on every PR with `push: false`, so the build is proven without
publishing.

Both paths run `ci.yml` first, via the `workflow_call` trigger, with
`skip_image: true` — the release builds the real image with the version ldflags
seconds later, so a throwaway one first is the same multi-arch Go compile twice
for no extra signal.

Manual publish, from any branch or commit:

```console
gh workflow run release.yml --ref my-branch
gh workflow run release.yml --ref my-branch -f version=1.5.0-test.1
```

With no `version` input it derives `0.0.0-dev.<short-sha>`. The version is
validated against SemVer **before** anything is pushed — `helm package` would
reject a bad one only after the image was already published, which is the wrong
order to fail in.

Three things to preserve if you touch this:

- **The version is a job output** (`needs.image.outputs.version`), resolved once
  in the `image` job. It used to be parsed from `refs/tags/v*` in both jobs,
  which silently produces garbage on a branch ref.
- **`:latest` is gated explicitly** to a non-prerelease tag push, not
  `latest=auto`. A manual dev build must never be able to claim it.
- **CI is a gate, not a suggestion.** Don't add a publish path that skips the
  `ci` job. A tag push matches none of `ci.yml`'s own triggers, so without the
  gate the one run that publishes is the only one that never ran the tests.

The chart is not optional: image and chart carry the same version, and a run
that published one without the other leaves the pair inconsistent. A tag
additionally pushes `:X.Y.Z` and `:X.Y`; every run pushes `sha-<full>`, signs
the digest with cosign, attaches an SBOM, and pushes the chart at the same
version to `oci://ghcr.io/<owner>/charts`.

## Upstream tracking

Dependabot only. CoreDNS is a **single-member group** in `dependabot.yml` so it
always lands in its own PR, never batched, never `ignore`d. There used to be a
`bump-coredns.yml` that resolved the latest release, bumped it, ran a gate, and
opened a PR only on success — it was removed as redundant: `ci.yml` runs on
`pull_request`, so Dependabot's PR already gets build, `go test -race`, the chart
render, the chart-Corefile boot and the image build.

The one behaviour that changed: a breaking bump now shows up as a **red PR**
rather than no PR plus a filed issue. Don't re-add a gate workflow to get the
issue back; a red PR on a named branch is the same signal.

When a bump fails, check in this order: the plugin chain changed upstream
(`TestDirectiveOrder`), the plugin API changed, or `k8s_gateway` is behind — it
pins its own CoreDNS version and may need a release first. Go MVS resolves to
the higher version, so our pin wins; CI is what proves that's safe.

## Conventions

- Comments explain **why**, not what. Design reasoning belongs here, not inline.
- Corefile options need a default that works; the plugin has five options total
  and that is intentional. Adding one needs justification.
- Chart values that would produce a broken Corefile fail in `validate.yaml` at
  render time rather than crash-looping a pod.
- Verify against real data rather than assuming — every parsing rule here came
  from running it against the actual list files.

## Commands

```console
make test          # unit + integration
make test-live     # parser vs the live lists (network)
make lint          # vet + gofmt
make chart-lint    # helm lint + render both service shapes
make run           # local server on :1053
make coredns-version
```

## Status

All verified, including the **container image** — built with podman for
`linux/amd64` and `linux/arm64`, and run end to end: fetched a real blocklist
over HTTPS (45,675 subtrees), answered NXDOMAIN for a listed domain and its
subdomain, and came up clean under `--read-only --cap-drop=ALL`.

Not yet exercised: the chart against a live cluster, and `release.yml`'s
push/sign/SBOM path.
