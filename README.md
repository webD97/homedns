# homedns

A custom [CoreDNS](https://coredns.io) distribution for a home network, packaged as an OCI
image and a Helm chart.

It replaces three things that usually run side by side:

| Replaced | By |
| --- | --- |
| Pi-hole | the [`blocklist`](plugin/blocklist/) plugin in this repo, with Prometheus metrics |
| external-dns | [`k8s_gateway`](https://github.com/k8s-gateway/k8s_gateway) — reads Gateway API resources directly instead of pushing records to a provider |
| a standalone CoreDNS | the same binary; every stock CoreDNS plugin is still available |

The result is one DNS server for the LAN: a filtering forwarder that also answers for
hostnames declared by `HTTPRoute` resources in the cluster.

## Quick start

```console
helm install homedns oci://ghcr.io/webd97/charts/homedns \
  --namespace homedns --create-namespace \
  --set gatewayAPI.zones={home.example.com} \
  --set service.loadBalancerIP=192.168.1.53
```

Then point the LAN's DHCP-advertised DNS server at that address.

Locally, without Kubernetes:

```console
make run                                    # serves on :1053
dig @127.0.0.1 -p 1053 doubleclick.net      # NXDOMAIN
dig @127.0.0.1 -p 1053 example.com          # resolves
curl -s localhost:9153/metrics | grep blocklist
```

## Stateless by design

No PersistentVolume, and no volume at all beyond the Corefile ConfigMap.

| State | Lives in | On restart |
| --- | --- | --- |
| Gateway API records | k8s informers | rebuilt from the API server in seconds |
| Blocklist domains | memory | re-downloaded |
| DNS cache | memory | discarded |
| Prometheus counters | memory | reset — use `increase(...[24h])` for Pi-hole-style daily totals |

Being PV-free means a fresh pod starts with no blocklist. The `blocklist` plugin therefore
reports **not ready** until a list loads, so the LoadBalancer sends it no traffic it would
answer unfiltered — then gives up after `readyTimeout` and serves unfiltered anyway,
flagging `coredns_blocklist_fail_open`. Losing ad blocking beats losing DNS for the whole
house.

## Configuration

The chart generates the Corefile from `values.yaml`; set `corefile` to write it yourself.
See [`charts/homedns/values.yaml`](charts/homedns/values.yaml) for everything.

```yaml
blocklist:
  urls:
    - https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
    - https://big.oisd.nl/domainswild
  # This server cannot resolve its own list URLs — that is circular, and cluster
  # DNS may forward back here. These must be literal IPs.
  bootstrapDNS: [1.1.1.1, 9.9.9.9]
  allow: [analytics.mycompany.example]

gatewayAPI:
  zones: [home.example.com]     # only hostnames in these zones are answered
  resources: [HTTPRoute]

hosts:                           # static LAN records, inlined into the Corefile
  - ip: 10.0.0.5
    names: [nas.home.example]

upstream:
  servers: [tls://1.1.1.1, tls://1.0.0.1]
  tlsServername: cloudflare-dns.com
```

A `Service` carrying both TCP and UDP on :53 is the default and works on Kubernetes 1.26+
with MetalLB or Cilium. Set `service.splitTcpUdp=true` for load balancers that still
refuse mixed protocols.

## Metrics

Pi-hole's headline numbers, on the standard CoreDNS metrics endpoint:

| | |
| --- | --- |
| total queries | `coredns_dns_requests_total` |
| blocked | `coredns_blocklist_blocked_total` |
| domains on the blocklist | `coredns_blocklist_domains_total` |
| % blocked | `blocked_total / (blocked_total + allowed_total)` |

Full list and suggested alerts: [`plugin/blocklist/README.md`](plugin/blocklist/README.md).

There are deliberately no per-domain or per-client labels — "top 10 clients" style panels
would mean unbounded metric cardinality.

## How this is built

`homedns` is **not** a CoreDNS fork. It is its own Go module that imports CoreDNS as a
library, so tracking upstream is `go get github.com/coredns/coredns@vX.Y.Z`.

`main.go` blank-imports `core/plugin` (registering every stock plugin) plus our two
additions, and [`directives.go`](directives.go) splices their names into
`dnsserver.Directives` — a plain `[]string` that CoreDNS reads back at Corefile-parse
time, so mutating it from an `init` is safe.

Order matters: `blocklist` goes immediately before `cache` (blocked names never reach an
upstream, and a refreshed list applies at once) while staying below `prometheus`/`log` so
blocked queries are still counted; `k8s_gateway` goes beside the other Kubernetes sources.
`insertBefore` **panics if an anchor is missing**, so an upstream reordering becomes a red
CI run on the CoreDNS bump PR rather than a silently misplaced plugin.

The image is `FROM scratch` and holds two files — the static binary and a CA
bundle for the blocklist fetches. It runs as UID 65532 with a read-only root
filesystem and `NET_BIND_SERVICE` as its only capability.

## Keeping up with CoreDNS

Dependabot, in its own PR — CoreDNS sits in a single-member group in
[`.github/dependabot.yml`](.github/dependabot.yml) so it is never batched with
anything else and can be reverted on its own.

There is no separate gate workflow: CI runs on every pull request, so a CoreDNS
bump has to pass the whole thing — `go test -race` including the directive-order
test, rendering the chart, booting the real binary against the Corefile the chart
generates, and a multi-arch image build. A breaking upstream change shows up as a
red PR.

Images and charts are published from `v*` tags — there is no `:main` or automatic
per-commit tag. Every PR still builds the multi-arch image, it just isn't pushed.
To publish a build from an arbitrary commit, run the release workflow by hand:

```console
gh workflow run release.yml --ref my-branch            # 0.0.0-dev.<sha>
gh workflow run release.yml --ref my-branch -f version=1.5.0-test.1
```

Manual builds never move `:latest`.

A second scheduled workflow re-parses the live blocklists and checks the result against
the entry count each publisher declares in its own header. A format change is otherwise
silent: entries still load and the domain count still looks right while nothing matches.

## Development

```console
make test          # unit + integration (builds the binary, queries it over the wire)
make test-live     # check the parser against the live blocklists
make loadtest      # fire ~576k real blocked domains at the binary
make lint
make chart-lint
make image
```

## License

Apache-2.0.
