# AGENTS.md

## Project Overview

`mcp-reverse-proxy` is a multi-tenant gateway. It exposes one HTTP Streamable MCP
endpoint speaking revision `2026-07-28` and fans requests out to several local
stdio MCP servers, most of which still speak the legacy `2025-11-25` revision.

Planning lives in `openspec/`. Read `openspec/changes/mcp-reverse-proxy/design.md`
before making architectural decisions and the files under
`openspec/changes/mcp-reverse-proxy/specs/` for required behavior. Specs are the
contract; this file is only about how the code is written.

## Toolchain

- Build with the current Go release; keep the `go.mod` floor at `1.25.0` to match
  the `modelcontextprotocol/go-sdk` requirement, and pin `toolchain` to the
  release actually used.
- `go build ./...`, `go test ./...`, `go test -race ./...`, `golangci-lint run`.
- `make vuln` scans the dependency graph with `govulncheck`. It is not part of
  `make check`: it fetches the tool and queries the vulnerability database, so it
  belongs where the network is expected. Most of the graph arrives through the
  SDK, so a finding is usually resolved by a dependency bump rather than a fix.

## Layout

```
cmd/                  entry point
internal/frontend     modern HTTP endpoint (SDK server)
internal/backend      stdio connections, era detection, normalization (SDK client)
internal/pool         child process pool
internal/config       base file and header config resolution
internal/policy       header policy modes, allowlists
internal/auth         OAuth resource server
internal/aggregate    namespacing, routing, notification fan-in
```

## Code Style

Standard Go conventions and `gofmt`. Prefer self-documenting code; comment the
*why*, not the *what*.

## Project Rules

These are not general Go advice — each one exists because a spec requirement or a
design decision depends on it.

**Secrets are redacted by type, not by discipline.** The `x-mcp-config` header
value and backend environment variables must never reach logs, errors, or
telemetry. Carry them in a type whose `LogValue() slog.Value` returns a
placeholder, so `slog` redacts them automatically. Never log such a value by
unwrapping it.

**`context.Context` is the first parameter of every function that can block, and
every backend call carries a deadline.** The era probe falls back only on error;
a silent legacy backend hangs until the context expires. A missing deadline is a
hang in production, not a style issue.

**Time-dependent tests use `testing/synctest`.** Idle TTL, LRU eviction, probe
timeout, and shutdown escalation are all timing behavior. Tests that sleep are
flaky; run them in a synctest bubble with virtual time instead.

**No bare `go func()`.** Use `sync.WaitGroup.Go` or `errgroup`. Every goroutine
has an owner that waits for it. A leaked goroutine here leaks a child process.

**SDK types stay inside `internal/backend` and `internal/frontend`.** No package
outside those two imports the SDK. It is a young library; a version bump must not
ripple through the codebase.

**Child processes are spawned by one constructor** that sets `Setpgid`, so signals
reach the whole descendant tree. Do not call `exec.Command` anywhere else.

## Errors

Wrap with `%w`; compare with `errors.Is` and `errors.As`, never by matching
strings. Define sentinel errors for the categories the specs require callers to
distinguish: subject quota exhaustion, pinned-era mismatch, and header policy
violation.

## Testing

Table-driven tests by default. Backend behavior is exercised against the stdio
fixtures (modern, legacy, silent, misbehaving) rather than real MCP servers —
the failure modes the specs describe cannot be reproduced on demand otherwise.
Run `go test -race ./...` before considering work done.

## Linting

`golangci-lint` v2. `gosec` rule `G204` flags subprocess execution with variable
arguments — that is exactly the `define-new` policy mode. Do not disable it
globally; suppress individual sites with a comment explaining the guard that makes
them safe.
