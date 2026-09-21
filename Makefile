SHELL := /bin/bash
GO ?= go
# Sandbox binaries must be statically linked so they run inside minimal
# rootfs images (e.g. Alpine, which has no glibc).
CGO_ENABLED ?= 0
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/vorash/vora/internal/version.Version=$(VERSION)

PROTO_DIR := api/proto/v1
PROTO_FILES := $(wildcard $(PROTO_DIR)/*.proto)
PROTO_PATH := $(shell go env GOBIN):$(shell go env GOPATH)/bin:$(PATH)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build all packages
	$(GO) build ./...

.PHONY: binaries
binaries: ## Build the vora binaries into bin/
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ ./cmd/...

.PHONY: release-artifacts
release-artifacts: ## Build dist/ artifacts for the current version (binaries, tarball, checksums)
	./scripts/release.sh --version "$(VERSION)"

.PHONY: push-images
push-images: ## Build and publish sandbox images (TAG=v0.4.0 [REGISTRY=...] [LOAD=1] [LATEST=1])
	./scripts/push-images.sh --tag "$(TAG)" \
		$(if $(REGISTRY),--registry "$(REGISTRY)",) \
		$(if $(filter 1,$(LOAD)),--load,) \
		$(if $(filter 1,$(LATEST)),--latest,)

.PHONY: install-system
install-system: ## Install dist/ binaries and system units on this host (needs root; API=1 for vora-api)
	./scripts/install-system.sh --from dist $(if $(filter 1,$(API)),--api,)

.PHONY: install-rootless
install-rootless: binaries ## Install and start the rootlesskit-wrapped vora-runner user service
	mkdir -p $(HOME)/.local/bin
	install -m 0755 bin/vora-runner bin/vora-init $(HOME)/.local/bin/
	install -m 0755 scripts/vora-rootless.sh $(HOME)/.local/bin/vora-rootless.sh
	mkdir -p $(HOME)/.config/systemd/user
	install -m 0644 deploy/systemd/vora-runner.service $(HOME)/.config/systemd/user/vora-runner.service
	systemctl --user daemon-reload
	systemctl --user enable vora-runner.service
	systemctl --user restart vora-runner.service
	@echo "vora-runner user service started; socket: $$XDG_RUNTIME_DIR/vora/runner.sock"

.PHONY: proto
proto: ## Generate Go code from protobuf definitions
	@command -v protoc >/dev/null || { echo "protoc not found"; exit 1; }
	@command -v protoc-gen-go >/dev/null || { echo "protoc-gen-go not found (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest)"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null || { echo "protoc-gen-go-grpc not found (go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest)"; exit 1; }
	PATH="$(PROTO_PATH)" protoc --proto_path=. \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		$(PROTO_FILES)
	$(GO) mod tidy

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-sdk
test-sdk: ## Build and test the TypeScript SDK (needs `npm install` in sdk/typescript)
	cd sdk/typescript && npm run build && npm run typecheck && npm test

.PHONY: test-integration
test-integration: ## Run integration tests (real runner, unix socket)
	$(GO) test -tags=integration ./tests/integration/...

.PHONY: test-security
test-security: ## Run the isolation security suite
	$(GO) test -tags=security ./tests/security/...

.PHONY: test-security-all
test-security-all: binaries ## Run the isolation matrix against bwrap AND gVisor
	$(GO) test -tags=security ./tests/security/...
	@command -v rootlesskit >/dev/null 2>&1 || { echo "rootlesskit not installed; skipped the gVisor leg"; exit 0; }
	rootlesskit \
		--state-dir="$${XDG_RUNTIME_DIR}/vora-rootlesskit-sec" \
		--net=slirp4netns --mtu=65520 --disable-host-loopback \
		--copy-up=/etc --copy-up=/run --copy-up=/var/lib --propagation=rslave \
		bash -c 'cd "$(CURDIR)" && VORA_INIT_BINARY="$(CURDIR)/bin/vora-init" $(GO) test -tags=security -run "Test.*/gvisor" ./tests/security/...'

.PHONY: test-gvisor
test-gvisor: binaries ## Run the gVisor integration suite under rootlesskit (Method 2)
	@command -v rootlesskit >/dev/null 2>&1 || { echo "rootlesskit not installed; skipped the gVisor leg"; exit 0; }
	rootlesskit \
		--state-dir="$${XDG_RUNTIME_DIR}/vora-rootlesskit-gvisor" \
		--net=slirp4netns --mtu=65520 --disable-host-loopback \
		--copy-up=/etc --copy-up=/run --copy-up=/var/lib --propagation=rslave \
		bash -c 'cd "$(CURDIR)" && VORA_INIT_BINARY="$(CURDIR)/bin/vora-init" $(GO) test -tags=integration -count=1 -run "TestGvisor" ./tests/integration/...'

.PHONY: test-network
test-network: ## Run the per-sandbox netns tests (restricted, full, private) under rootlesskit
	@command -v rootlesskit >/dev/null 2>&1 || { echo "rootlesskit not installed; skipped"; exit 0; }
	rootlesskit \
		--state-dir="$${XDG_RUNTIME_DIR}/vora-rootlesskit-net" \
		--net=slirp4netns --mtu=65520 --disable-host-loopback \
		--copy-up=/etc --copy-up=/run --copy-up=/var/lib --propagation=rslave \
		bash -c 'cd "$(CURDIR)" && $(GO) test -tags=integration -run TestNetwork ./tests/integration/...'

.PHONY: test-broker
test-broker: ## Run the credential-broker end-to-end test under rootlesskit
	@command -v rootlesskit >/dev/null 2>&1 || { echo "rootlesskit not installed; skipped"; exit 0; }
	rootlesskit \
		--state-dir="$${XDG_RUNTIME_DIR}/vora-rootlesskit-broker" \
		--net=slirp4netns --mtu=65520 --disable-host-loopback \
		--copy-up=/etc --copy-up=/run --copy-up=/var/lib --propagation=rslave \
		bash -c 'cd "$(CURDIR)" && $(GO) test -tags=integration -run TestBroker ./tests/integration/...'

.PHONY: test-dns
test-dns: ## Run the gateway-resolver test under rootlesskit
	@command -v rootlesskit >/dev/null 2>&1 || { echo "rootlesskit not installed; skipped"; exit 0; }
	rootlesskit \
		--state-dir="$${XDG_RUNTIME_DIR}/vora-rootlesskit-dns" \
		--net=slirp4netns --mtu=65520 --disable-host-loopback \
		--copy-up=/etc --copy-up=/run --copy-up=/var/lib --propagation=rslave \
		bash -c 'cd "$(CURDIR)" && $(GO) test -tags=integration -run TestDNSThroughNetns ./tests/integration/...'

.PHONY: bench
bench: ## Run the benchmark harness
	$(GO) test -tags=benchmark -bench=. -run='^$$' ./tests/benchmark/...

.PHONY: bench-report
bench-report: ## Run benchmarks and write a report to docs/benchmarks/
	./scripts/bench.sh

.PHONY: images
images: ## Build sandbox rootfs image cache
	./scripts/build-images.sh

.PHONY: setup-containerd
setup-containerd: ## Install/verify the rootless containerd OCI image store (ADR 009)
	./scripts/setup-containerd.sh

.PHONY: pull-images
pull-images: ## Import the Docker-built rootfs images into containerd
	./scripts/pull-images.sh

.PHONY: fmt
fmt: ## Format Go code
	$(GO) fmt ./...

.PHONY: vet
vet: ## Vet Go code
	$(GO) vet ./...

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
