# sigbrowse Makefile
#
# Common targets:
#   make build     build the sigbrowse binary into ./bin
#   make test      run the test suite
#   make check     gofmt + go vet + tests (CI gate)
#   make up        bring up the Docker compose stack
#   make ingest    run an ingest pass inside the container
#   make journal   rebuild the journal (mechanical + digests)

BINARY      := sigbrowse
PKG         := github.com/joestump/sigbrowse
BIN_DIR     := bin
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(PKG)/internal/cli.Version=$(VERSION) \
               -X $(PKG)/internal/cli.Commit=$(COMMIT) \
               -X $(PKG)/internal/cli.BuildDate=$(BUILD_DATE)

GO          ?= go

.PHONY: all build run test cover check fmt fmt-check vet tidy clean up down ingest journal

all: check build

build: ## Build the binary
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/sigbrowse

run: build ## Build then run the web UI
	$(BIN_DIR)/$(BINARY) serve

test: ## Run all tests
	$(GO) test ./...

cover: ## Run tests with coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fmt: ## Format the code
	$(GO) fmt ./...

fmt-check: ## Fail if any file is not gofmt-clean
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## Run go vet
	$(GO) vet ./...

tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

check: fmt-check vet test ## CI gate: format check, vet, tests

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.out

up: ## Start the Docker compose stack
	docker compose up -d --build

down: ## Stop the Docker compose stack
	docker compose down

ingest: ## Run an ingest pass in the container
	docker compose run --rm sigbrowse ingest

journal: ## Rebuild the journal in the container
	docker compose run --rm sigbrowse journal
