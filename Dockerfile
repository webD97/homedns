# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first: the CoreDNS module graph is large, so keeping this layer
# keyed only on go.mod/go.sum saves re-downloading it on every source change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=""

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/homedns .

# distroless static already carries the CA bundle, which the blocklist plugin
# needs to fetch lists over HTTPS.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG REVISION=""
ARG COREDNS_VERSION=unknown

LABEL org.opencontainers.image.title="homedns" \
      org.opencontainers.image.description="CoreDNS with Pi-hole style blocklists and Kubernetes Gateway API resolution" \
      org.opencontainers.image.source="https://github.com/webd97/homedns" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      io.homedns.coredns.version="${COREDNS_VERSION}"

COPY --from=build /out/homedns /usr/local/bin/homedns

# Binding :53 needs NET_BIND_SERVICE, granted by the Helm chart's
# securityContext rather than by running as root.
USER nonroot:nonroot

EXPOSE 53/udp 53/tcp 9153/tcp

ENTRYPOINT ["/usr/local/bin/homedns"]
CMD ["-conf", "/etc/coredns/Corefile"]
