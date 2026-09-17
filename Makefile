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

.PHONY: bench
bench: ## Run the benchmark harness
	$(GO) test -tags=benchmark -bench=. -run='^$$' ./tests/benchmark/...

.PHONY: bench-report
bench-report: ## Run benchmarks and write a report to docs/benchmarks/
	./scripts/bench.sh

.PHONY: images
images: ## Build sandbox rootfs image cache
	./scripts/build-images.sh

.PHONY: fmt
fmt: ## Format Go code
	$(GO) fmt ./...

.PHONY: vet
vet: ## Vet Go code
	$(GO) vet ./...

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
