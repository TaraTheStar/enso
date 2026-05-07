# enso — make targets
#
# Common: make build / make test / make run.
# Quality: make check (fmt + vet + test + build).
# Use BIN=./enso (or any path) to override the output location.

BIN ?= ./bin/enso
PKG := ./cmd/enso
GOFLAGS ?= -trimpath
# git describe gives a clean tag for releases (`v0.1.0`) and a
# tag+commit string mid-cycle (`v0.1.0-3-gabc123[-dirty]`). Falls back
# silently if git isn't around.
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null)
LDFLAGS ?= -s -w $(if $(VERSION),-X main.version=$(VERSION))
CGO_ENABLED ?= 0

GO ?= go
export CGO_ENABLED

.PHONY: all build install run tui daemon test test-race vet fmt fmt-check tidy clean help

all: build

## build: compile the enso binary into $(BIN)
build:
	@mkdir -p $(dir $(BIN))
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

## install: install enso into $$(go env GOBIN || go env GOPATH)/bin
install:
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(PKG)

## run: build and launch the TUI
run: tui
tui: build
	$(BIN) tui

## daemon: build and run the daemon in the foreground
daemon: build
	$(BIN) daemon

## test: run the unit tests
test:
	$(GO) test ./...

## test-race: run the unit tests with the race detector (requires CGO)
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

## vet: run go vet on every package
vet:
	$(GO) vet ./...

## fmt: format every .go file in-place with gofmt
fmt:
	gofmt -w .

## fmt-check: fail if anything is not gofmt-clean (CI-friendly)
fmt-check:
	@diff=$$(gofmt -l .); \
	if [ -n "$$diff" ]; then \
		echo "gofmt: the following files are not formatted:"; \
		echo "$$diff"; \
		exit 1; \
	fi

## tidy: refresh go.mod / go.sum
tidy:
	$(GO) mod tidy

## check: fmt-check + vet + test + build (full pre-commit gate)
check: fmt-check vet test build

## clean: remove the build output directory
clean:
	rm -rf $(dir $(BIN))

## help: list available targets
help:
	@awk 'BEGIN {FS=":.*?##"; print "enso — available targets:\n"} /^## [a-z][a-zA-Z0-9_-]*:/ {sub(/^## /, ""); printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
