SHELL := /bin/bash

BINARY  ?= homedns
IMAGE   ?= ghcr.io/webd97/homedns
CHART   ?= charts/homedns

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
COREDNS_VERSION = $(shell go list -m -f '{{.Version}}' github.com/coredns/coredns)

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.revision=$(REVISION)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: test
test: ## Run unit and integration tests
	go test ./... -race -timeout 300s

.PHONY: test-live
test-live: ## Check the parser against the live upstream blocklists (needs network)
	HOMEDNS_LIVE_LISTS=1 go test ./plugin/blocklist/ -run TestLiveLists -v -count=1

.PHONY: loadtest
loadtest: ## Fire every domain from two real blocklists at the binary
	./scripts/loadtest.sh

.PHONY: lint
lint: ## Vet and format check
	go vet ./...
	@out=$$(gofmt -l . ); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: run
run: build ## Run locally against test/Corefile.local
	./$(BINARY) -conf test/Corefile.local

.PHONY: image
image: ## Build the OCI image for the host platform
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(REVISION) \
		--build-arg COREDNS_VERSION=$(COREDNS_VERSION) \
		-t $(IMAGE):$(VERSION) .

.PHONY: image-multiarch
image-multiarch: ## Build (not push) the multi-arch image
	docker buildx build --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(REVISION) \
		--build-arg COREDNS_VERSION=$(COREDNS_VERSION) \
		-t $(IMAGE):$(VERSION) .

.PHONY: chart-lint
chart-lint: ## Lint and render the Helm chart
	helm lint $(CHART)
	helm template homedns $(CHART) >/dev/null
	helm template homedns $(CHART) --set service.splitTcpUdp=true >/dev/null

.PHONY: coredns-version
coredns-version: ## Print the CoreDNS version this build embeds
	@echo $(COREDNS_VERSION)

.PHONY: clean
clean:
	rm -f $(BINARY)
