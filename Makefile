BIN := bonsai
PKG := ./cmd/bonsai

# Embed version into the binary at build time so `bonsai --version` is honest.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS_DEV  := -X main.version=$(VERSION)
LDFLAGS_SLIM := -s -w -X main.version=$(VERSION)

.PHONY: build build-slim test vet check clean

# Dev build: keeps symbol table + DWARF for `go tool pprof`, panics with line numbers.
build:
	go build -ldflags "$(LDFLAGS_DEV)" -o $(BIN) $(PKG)

# Release / CI build: stripped + path-trimmed. ~30% smaller, reproducible.
# Use this for what we ship to users.
build-slim:
	go build -ldflags "$(LDFLAGS_SLIM)" -trimpath -o $(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

check: vet test build-slim

clean:
	rm -f $(BIN)
