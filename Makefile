SHELL := /bin/bash
GO ?= go
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/vorash/vora/internal/version.Version=$(VERSION)

PROTO_DIR := api/proto/v1
PROTO_FILES := $(wildcard $(PROTO_DIR)/*.proto)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build all packages
	$(GO) build ./...

.PHONY: binaries
binaries: ## Build the vora binaries into bin/
	mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ ./cmd/...

.PHONY: proto
proto: ## Generate Go code from protobuf definitions
	@command -v protoc >/dev/null || { echo "protoc not found"; exit 1; }
	protoc --proto_path=. \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		$(PROTO_FILES)

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-integration
test-integration: ## Run integration tests (real runner, unix socket)
	$(GO) test -tags=integration ./tests/integration/...

.PHONY: test-security
test-security: ## Run the isolation security suite
	$(GO) test -tags=security ./tests/security/...

.PHONY: bench
bench: ## Run the benchmark harness
	$(GO) test -tags=benchmark -bench=. -run='^$$' ./tests/benchmark/...

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
