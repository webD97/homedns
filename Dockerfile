# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first: the CoreDNS module graph is large (221 modules, 240MB of
# zips). Deliberately no --mount=type=cache: a cache mount lives on the builder
# and is never carried by cache-from/cache-to, so on a fresh CI runner it is
# empty while this layer is still a cache hit -- the RUN never executes, and the
# build below then re-downloads the entire graph on every single run, over a
# proxy that only has to flake once to fail a release. Baking the modules into
# the layer instead means the layer cache restores them, and this stays the only
# step in the build that touches the network at all.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=""

# GOPROXY=off keeps this step hermetic: if the layer above ever stops covering
# what the build needs, it fails here with "module lookup disabled" instead of
# quietly going back to the proxy. The build cache mount stays -- unlike the
# module cache it only ever costs time, never correctness.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOPROXY=off \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/homedns .

# The blocklist fetcher speaks HTTPS, so the image needs a CA bundle and nothing
# else. Taken from distroless rather than the builder because distroless is
# rebuilt on every ca-certificates update; golang:1.26-bookworm carries whatever
# Debian snapshot that tag was built against. Pinned to BUILDPLATFORM — the
# bundle is a single arch-independent file, so there is no reason to pull this
# image once per target architecture.
FROM --platform=$BUILDPLATFORM gcr.io/distroless/static-debian12:latest AS certs

FROM scratch

ARG VERSION=dev
ARG REVISION=""
ARG COREDNS_VERSION=unknown

LABEL org.opencontainers.image.title="homedns" \
      org.opencontainers.image.description="CoreDNS with Pi-hole style blocklists and Kubernetes Gateway API resolution" \
      org.opencontainers.image.source="https://github.com/webd97/homedns" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.base.name="scratch" \
      io.homedns.coredns.version="${COREDNS_VERSION}"

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/homedns /usr/local/bin/homedns

# Redundant (Go probes this path anyway), but it makes the image's one non-binary
# file self-documenting and gives a lever for a custom bundle.
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

# Numeric, not a name: scratch has no /etc/passwd. This is also what kubelet
# wants — it can only enforce runAsNonRoot against a numeric USER. Matches
# podSecurityContext.runAsUser in the chart. Binding :53 needs NET_BIND_SERVICE,
# granted by the chart's securityContext.
USER 65532:65532

EXPOSE 53/udp 53/tcp 9153/tcp

ENTRYPOINT ["/usr/local/bin/homedns"]
CMD ["-conf", "/etc/coredns/Corefile"]
