# AGENTS.md

Orientation for agents working in this repo. README.md is for users; this is the
why, the traps, and the things that are easy to get wrong.

## What this is

One DNS server for a home LAN, replacing Pi-hole (blocklists), external-dns
(Gateway API), and a standalone CoreDNS (forwarding + static records). Deployed
in-cluster with a LoadBalancer on :53.

Two features, very different in weight:

- **`plugin/blocklist/`** — written here. This is where the work is.
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

`directives.go` splices `blocklist` before `cache` and `k8s_gateway` before
`kubernetes`. `insertBefore` **panics on a missing anchor** — deliberate: a
blocklist that silently runs after `cache` is the kind of bug nobody notices, so
an upstream reorder must fail the bump PR instead.

Placement reasoning, if you ever move them:

- `blocklist` before `cache` — blocked names never reach an upstream, and a
  refreshed list applies immediately rather than after cache expiry.
- `blocklist` after `prometheus`/`log`/`errors` — blocked queries still get
  counted and logged.
- `blocklist` before `hosts` — the integration test depends on this; see below.

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
and are re-downloaded; Gateway API records are rebuilt from informers; counters
reset (use `increase(...[24h])` for Pi-hole-style daily totals). If you add
anything needing disk, that is a design change — raise it rather than adding a
PVC.

## Upstream tracking

`bump-coredns.yml` runs weekly, bumps CoreDNS, and opens a PR **only if** build,
`go test -race`, and the chart-Corefile boot all pass. Otherwise it files an
issue. Dependabot handles everything else and ignores CoreDNS so the two don't
fight.

When a bump fails, check in this order: the plugin chain changed upstream
(`TestDirectiveOrder`), the plugin API changed, or `k8s_gateway` is behind — it
pins its own CoreDNS version and may need a release first. Go MVS resolves to
the higher version, so our pin wins; the gate is what proves that's safe.

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
