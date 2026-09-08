.PHONY: build bin vet lint test test-race test-concurrency conformance vuln check

# Compiles every package and writes nothing: this is the check that the tree
# builds, not a way to obtain something to run.
build:
	go build ./...

# The binary itself. BIN names where it lands, so a deployment can build
# straight into place: make bin BIN=/usr/local/bin/mcp-reverse-proxy
BIN ?= mcp-reverse-proxy
bin:
	go build -o $(BIN) ./cmd/mcp-reverse-proxy

vet:
	go vet ./...

lint:
	golangci-lint run

test:
	go test ./...

# Every goroutine here owns a child process; races are not a style concern.
test-race:
	go test -race ./...

# The packages where several goroutines touch the same state: the pool's
# bookkeeping, the fan-in of backend changes, and the endpoint that drives both.
# A single pass can miss an interleaving that only shows up occasionally, so
# these run repeatedly rather than once.
test-concurrency:
	go test -race -count=5 ./internal/pool ./internal/aggregate ./internal/frontend

# The official MCP conformance suite. It downloads a Node package and runs the
# gateway against it, so it is a separate target rather than part of `check`.
CONFORMANCE_VERSION ?= 0.2.0-alpha.10
conformance:
	MCP_CONFORMANCE=1 MCP_CONFORMANCE_VERSION=$(CONFORMANCE_VERSION) \
		go test -count=1 -timeout 20m -run TestConformance ./internal/conformance

# Known vulnerabilities in the dependency graph, symbol-level: a finding here
# means the vulnerable function is reachable from this code, not merely present
# in a module. Pinned rather than @latest so a run is reproducible, and outside
# `check` for the same reason as conformance — it fetches the tool and queries
# the vulnerability database over the network.
GOVULNCHECK_VERSION ?= v1.7.0
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

check: build vet lint test-race
