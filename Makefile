# msgbrowse Makefile
#
# Common targets:
#   make build     build the msgbrowse binary into ./bin
#   make test      run the test suite
#   make check     gofmt + go vet + tests (CI gate)
#   make desktop-linux build the Linux desktop shell into ./bin (cgo, WebKit2GTK)
#   make desktop-test  headless tests for the desktop module (pure Go)
#   make up            bring up the Docker compose stack
#   make signal-import import the signal-export archive (in the container)
#   make embed         compute embeddings for new messages (in the container)
#   make journal       rebuild the journal (mechanical + digests)

BINARY      := msgbrowse
PKG         := github.com/joestump/msgbrowse
BIN_DIR     := bin
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(PKG)/internal/cli.Version=$(VERSION) \
               -X $(PKG)/internal/cli.Commit=$(COMMIT) \
               -X $(PKG)/internal/cli.BuildDate=$(BUILD_DATE)

GO          ?= go

# --- CSS toolchain (dev-time only; the built app.css is committed) ---
# Pinned versions of the Tailwind v4 standalone CLI (single binary, no Node) and
# the daisyUI package. Downloaded into .tools/ by `make css`; never committed.
TAILWIND_VERSION := v4.3.1
DAISYUI_VERSION  := 5.6.3
TOOLS_DIR        := .tools
# Map uname → Tailwind release asset suffix (linux/macos × x64/arm64).
UNAME_S          := $(shell uname -s)
UNAME_M          := $(shell uname -m)
TW_OS            := $(if $(filter Darwin,$(UNAME_S)),macos,linux)
TW_ARCH          := $(if $(filter arm64 aarch64,$(UNAME_M)),arm64,x64)
TW_ASSET         := tailwindcss-$(TW_OS)-$(TW_ARCH)

# The SQLite driver is pure-Go (modernc.org/sqlite) with FTS5 built in, so the
# build needs no C toolchain and no build tag. Pin CGO off to keep it that way.
export CGO_ENABLED = 0

# --- Desktop shell (ADR-0017 / SPEC-0010) ---
# The desktop shell is the repository's only cgo code, quarantined in its own
# Go module (cmd/msgbrowse-desktop/go.mod) behind the `desktop` build tag so
# the core stays CGO_ENABLED=0. The desktop targets are never prerequisites of
# any core target. The Linux build needs the webview dev headers:
#   sudo apt-get install -y libgtk-3-dev libwebkit2gtk-4.1-dev pkg-config
# `webkit2_41` links webkit2gtk-4.1 (Ubuntu 24.04+); on distros still shipping
# webkit2gtk-4.0, override: make desktop-linux DESKTOP_TAGS=desktop,production
DESKTOP_DIR  := cmd/msgbrowse-desktop
DESKTOP_BIN  := msgbrowse-desktop
DESKTOP_TAGS ?= desktop,production,webkit2_41

.PHONY: all build install run test cover check fmt fmt-check vet tidy clean clean-tools css up up-bundled down logs signal-import embed journal desktop-linux desktop-test

all: check build

build: ## Build the binary
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/msgbrowse

install: ## Install the binary into $GOBIN/$GOPATH/bin
	$(GO) install -ldflags "$(LDFLAGS)" ./cmd/msgbrowse

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

desktop-linux: ## Build the Linux desktop shell into ./bin (cgo; needs GTK3/WebKit2GTK dev packages)
	cd $(DESKTOP_DIR) && CGO_ENABLED=1 $(GO) build -tags $(DESKTOP_TAGS) -o ../../$(BIN_DIR)/$(DESKTOP_BIN) .

desktop-test: ## Run the desktop module's headless tests (pure Go; no webview toolchain needed)
	cd $(DESKTOP_DIR) && $(GO) test ./...

css: $(TOOLS_DIR)/tailwindcss $(TOOLS_DIR)/daisyui/package/index.js ## Rebuild internal/web/static/app.css (Tailwind + daisyUI; dev-time only)
	$(TOOLS_DIR)/tailwindcss \
	  -i internal/web/tailwind/input.css \
	  -o internal/web/static/app.css \
	  --minify

$(TOOLS_DIR)/tailwindcss:
	@mkdir -p $(TOOLS_DIR)
	@echo "downloading Tailwind $(TAILWIND_VERSION) ($(TW_ASSET))…"
	curl -fsSL -o $@ "https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/$(TW_ASSET)"
	chmod +x $@

$(TOOLS_DIR)/daisyui/package/index.js:
	@mkdir -p $(TOOLS_DIR)/daisyui
	@echo "downloading daisyUI $(DAISYUI_VERSION)…"
	curl -fsSL "https://registry.npmjs.org/daisyui/-/daisyui-$(DAISYUI_VERSION).tgz" | tar -xz -C $(TOOLS_DIR)/daisyui

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.out

clean-tools: ## Remove the downloaded CSS toolchain
	rm -rf $(TOOLS_DIR)

up: ## Start msgbrowse (points at your external LiteLLM via .env)
	docker compose up -d --build

up-bundled: ## Start msgbrowse + the bundled LiteLLM proxy
	docker compose --profile bundled-llm up -d --build

down: ## Stop the Docker compose stack
	docker compose --profile bundled-llm down

logs: ## Tail the msgbrowse container logs
	docker compose logs -f msgbrowse

signal-import: ## Import the signal-export archive (in the container)
	docker compose run --rm msgbrowse signal-import

embed: ## Compute embeddings for new messages (in the container)
	docker compose run --rm msgbrowse embed

journal: ## Rebuild the journal in the container
	docker compose run --rm msgbrowse journal
