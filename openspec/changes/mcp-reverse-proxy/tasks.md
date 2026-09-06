## 1. Project skeleton

- [x] 1.1 Initialize the Go module and package layout (`cmd/`, `internal/frontend`, `internal/backend`, `internal/pool`, `internal/config`, `internal/policy`, `internal/auth`, `internal/aggregate`) and verify `go build ./...` succeeds
- [x] 1.2 Pin `github.com/modelcontextprotocol/go-sdk` to an explicit version in `go.mod` and verify `go mod verify` passes
- [x] 1.3 Define internal interfaces wrapping SDK client and server entry points so SDK version changes stay contained, and verify no package outside `internal/backend` and `internal/frontend` imports the SDK (enforced by a lint rule or an import test)
- [x] 1.4 Add linting, `go vet`, and a race-enabled test target, and verify all three run clean on the empty skeleton

## 2. Test fixtures

- [x] 2.1 Build a legacy-era stdio fixture server that answers `initialize` and errors on unknown pre-handshake methods, and verify it responds to a manual `initialize` over stdin
- [x] 2.2 Build a modern-era stdio fixture server that answers `server/discover`, and verify it returns a `DiscoverResult`
- [x] 2.3 Build a silent fixture that never answers `server/discover`, and verify it exercises the probe timeout path
- [x] 2.4 Build a misbehaving fixture that emits an unsolicited `sampling/createMessage` request, and verify it reproduces the capability-masking violation scenario
- [ ] 2.5 Parameterize the legacy, silent, and misbehaving fixtures by the protocol revision they answer `initialize` with, defaulting to `2025-11-25`, and verify a manual `initialize` answered with `2024-11-05`

## 3. Base configuration

- [x] 3.1 Define the configuration file schema (server id, command, args, env, era, limits) and verify a valid sample file loads into the expected struct
- [x] 3.2 Implement startup validation rejecting missing files, parse errors, and servers lacking mandatory fields, and verify each failure names the file and the specific problem
- [x] 3.3 Default the `era` field to `auto` when omitted, and verify with a fixture config that omits it
- [x] 3.4 Implement the resolved-configuration fingerprint as an irreversible hash, and verify identical inputs hash equally while a changed env value does not

## 4. Backend connections

- [x] 4.1 Spawn a stdio backend in its own process group (`Setpgid`) and connect via the SDK client, and verify a `tools/list` round trip against the modern fixture
- [x] 4.2 Implement era detection with a bounded probe deadline, and verify the modern fixture resolves to modern and the legacy fixture falls back to `initialize`
- [x] 4.3 Adopt the revision a legacy backend names in its `initialize` result rather than the offered one, and verify a fixture answering `2024-11-05` connects and lists tools identically to a `2025-11-25` one
- [x] 4.4 Fail connection when a backend names a revision outside the supported set, and verify the error names the server and the reported revision
- [x] 4.5 Handle the `-32022` retry path selecting a mutually supported version, and verify the client does not fall back to `initialize` on that error
- [x] 4.6 Enforce the probe timeout, and verify the silent fixture falls back to legacy within the configured deadline rather than hanging
- [x] 4.7 Honor pinned `era: legacy` by skipping the probe entirely, and verify no `server/discover` is written to the fixture's stdin
- [x] 4.8 Fail connection on pinned `era: modern` without falling back, and verify that against both the legacy and the silent fixture no `initialize` is written to the fixture's stdin and the error names the server and the mismatch
- [x] 4.9 Cache the detected era for the process lifetime, and verify a second request issues no probe
- [x] 4.10 Mask `roots`, `sampling`, `logging`, and `elicitation` from advertised client capabilities, and verify the legacy fixture's received `initialize` params omit them
- [x] 4.11 Reject unsolicited server-to-client requests with an error and a log record, and verify the misbehaving fixture does not disrupt a concurrent second backend
- [x] 4.12 Normalize legacy responses (add `resultType: "complete"`, remap `-32002` to `-32602`, supply missing `ttlMs` and `cacheScope`), and verify each transformation with a unit test
- [x] 4.13 Implement graceful shutdown closing stdin first with timed escalation to `SIGTERM` then `SIGKILL` across the process group, and verify no descendant survives a fixture that spawns a child
- [x] 4.14 Capture backend stderr into the log without treating it as failure, and verify a fixture writing to stderr still serves requests
- [x] 4.15 Restart on unexpected exit, failing in-flight requests as retryable, and verify the next request reaches a fresh process

## 5. Minimal frontend vertical slice

- [x] 5.1 Serve the modern HTTP endpoint via the SDK handler for a single configured backend, and verify an end-to-end `tools/list` over HTTP returns the fixture's tools
- [x] 5.2 Implement `server/discover` returning supported versions, aggregated capabilities, and server info, and verify deprecated capabilities are absent from the result
- [x] 5.3 Validate `Mcp-Method` and `Mcp-Name` headers, returning `400` when absent and `-32020` on mismatch with the body, and verify both cases
- [x] 5.4 Reject `initialize` with an error naming supported versions, and verify a legacy client receives an actionable message
- [x] 5.5 Reject the removed methods `ping`, `logging/setLevel`, `resources/subscribe`, and `resources/unsubscribe` with `-32601`, and verify each
- [x] 5.6 Enforce per-request version negotiation returning `-32022` with `data.supported` and `data.requested`, and verify an unsupported version request
- [x] 5.7 Guarantee `resultType: "complete"` on every result and never emit `input_required`, and verify against both fixture eras
- [x] 5.8 Attach `ttlMs` and `cacheScope: "private"` to all cacheable results, and verify `"public"` never appears in any response

## 6. Authorization

- [x] 6.1 Serve the RFC 9728 protected resource metadata document, and verify it exposes `resource`, `authorization_servers`, and `scopes_supported` with a canonical fragment-free resource URI
- [x] 6.2 Reject unauthenticated requests with `401` and a `WWW-Authenticate` challenge carrying `resource_metadata` and `scope`, and verify the header contents
- [x] 6.3 Validate token signature, expiry, issuer, and audience against the canonical URI, and verify each rejection path returns `401`
- [x] 6.4 Reject tokens supplied in the query string, and verify the token is not accepted
- [x] 6.5 Derive the subject identifier from the `iss` and `sub` pair, and verify two issuers sharing a `sub` value yield distinct subjects
- [x] 6.6 Reject validated tokens lacking `sub` with `401`, and verify with a crafted token
- [x] 6.7 Confirm the presented access token never reaches child process env or args, and verify by asserting on the spawned fixture's observed environment

## 7. Process pool

- [ ] 7.1 Key the pool by subject and resolved configuration fingerprint, and verify reuse for a repeat request from the same subject
- [ ] 7.2 Verify two subjects with byte-identical configuration receive separate processes, asserting on distinct process identifiers
- [ ] 7.3 Verify the same subject changing an env value receives a separate process rather than the cached one
- [ ] 7.4 Implement idle TTL termination, and verify a process exits after the configured idle period
- [ ] 7.5 Implement global limit with least-recently-used eviction, and verify the oldest idle process is terminated when the limit is reached
- [ ] 7.6 Implement the per-subject limit rejecting requests beyond quota, and verify the error explicitly names quota exhaustion
- [ ] 7.7 Add pool size and eviction metrics, and verify they are exposed and change under a load test

## 8. Header configuration and policy

- [ ] 8.1 Parse the `x-mcp-config` header as JSON, returning `400` on malformed input without echoing the value, and verify the response body contains no header content
- [ ] 8.2 Enforce a header size limit returning `400` naming the exceeded limit, and verify with an oversized header
- [ ] 8.3 Implement merge semantics: key-wise `env` merge, wholesale replacement of other fields, and verify all three merge scenarios from the spec
- [ ] 8.4 Implement the four policy modes with `off` as the default, and verify each mode permits and rejects exactly what the spec states
- [ ] 8.5 Verify `off` rejects a present header with an error and no `insufficient_scope` challenge, while a request without the header is served normally
- [ ] 8.6 Gate elevated modes on token scopes returning `403` with `insufficient_scope` and the complete required scope set in one challenge, and verify no incremental challenging occurs
- [ ] 8.7 Enforce the command allowlist and env key denylist regardless of mode, and verify `define-new` still rejects a disallowed command
- [ ] 8.8 Fail startup when `define-new` is enabled without a command allowlist, and verify the configuration error
- [ ] 8.9 Redact the header from logs, errors, and telemetry, and verify by scanning captured log output during a policy rejection test

## 9. Aggregation

- [ ] 9.1 Prefix tool, prompt, and prompt template names with the backend id and a double underscore, and verify a three-backend merged `tools/list`
- [ ] 9.2 Fail configuration resolution on colliding resulting names, naming both backends and the conflict, and verify the error text
- [ ] 9.3 Fail configuration resolution on duplicate backend identifiers and on prefixed names exceeding the length limit, and verify both
- [ ] 9.4 Route `tools/call` by prefix, stripping it before dispatch, and verify the backend receives the unprefixed name
- [ ] 9.5 Return "tool not found" for unknown prefixes and unprefixed names without broadcasting, and verify no fixture other than the intended one receives the call
- [ ] 9.6 Implement the `mcp-proxy://` resource URI wrapping and unwrapping, and verify a list-then-read round trip preserves the original backend URI
- [ ] 9.7 Return `-32602` for unparseable gateway URIs and unknown backends, and verify both
- [ ] 9.8 Keep listing methods succeeding when a backend is unavailable, omitting its entries, logging the backend, and shortening `ttlMs`, and verify the reduced value
- [ ] 9.9 Return an explicit unavailability error on calling a tool of a downed backend, and verify the error identifies the backend
- [ ] 9.10 Return empty lists rather than an error when every backend is unavailable, and verify with all fixtures stopped
- [ ] 9.11 Guarantee deterministic listing order across repeated calls, and verify two consecutive `tools/list` results are identical

## 10. Subscription fan-in

- [ ] 10.1 Accept `subscriptions/listen`, acknowledge the subscription, and tag notifications with `io.modelcontextprotocol/subscriptionId`, and verify the tag on a delivered notification
- [ ] 10.2 Fan in `listChanged` notifications from modern backends, and verify a change in the modern fixture reaches the client stream
- [ ] 10.3 Fan in change notifications from legacy backends through the legacy mechanism, normalizing them to the `2026-07-28` shape, and verify the client sees the modern form
- [ ] 10.4 Filter notifications to the client's subscribed types, and verify a prompt list change is withheld from a tools-only subscriber
- [ ] 10.5 Release subscription resources on stream break without disturbing other clients, and verify a second client's stream survives the first disconnecting

## 11. Integration and hardening

- [ ] 11.1 Write an end-to-end test covering two subjects, two backends of different eras, and header configuration, and verify no cross-subject process reuse occurs
- [ ] 11.2 Run the change against the MCP conformance suite for revision `2026-07-28`, and verify the frontend passes the applicable checks
- [ ] 11.3 Add a race-detector run over the pool and fan-in tests, and verify it reports no data races
- [ ] 11.4 Add a soak test spawning and evicting processes under load, and verify no process or file descriptor leak after the run
- [ ] 11.5 Write deployment documentation covering policy modes, the `define-new` risk, and the requirement to suppress `x-mcp-config` in reverse proxy logs, and verify the document names every configuration option
