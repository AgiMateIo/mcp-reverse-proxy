## Purpose

Defines how the gateway connects to local stdio MCP servers: how a backend's protocol era is detected, how legacy responses are normalized to the modern shape, and how child processes are spawned, isolated between subjects, reused, and terminated.

## ADDED Requirements

### Requirement: Backend protocol era detection

The gateway SHALL detect a backend's era by probing with the `server/discover` method whenever the server is configured with `era: auto`. The probe MUST be bounded by a timeout.

The three era modes differ in whether the probe is sent and whether falling back to the legacy handshake is permitted, and no two of them are interchangeable:

- `auto` sends the probe and falls back to `initialize` on failure.
- `modern` sends the probe and MUST NOT fall back, so that a backend which starts answering as legacy fails loudly instead of silently losing the modern result shape.
- `legacy` does not send the probe at all, which is what keeps a backend that never answers `server/discover` from costing a probe timeout on every cold start. Cold starts are frequent, because the pool is keyed by subject.

#### Scenario: Backend answers the probe

- **WHEN** the backend returns a valid result for `server/discover`
- **THEN** the connection proceeds in the modern era without sending `initialize`

#### Scenario: Backend does not support the requested version

- **WHEN** the backend answers the probe with an error of code `-32022` listing its supported versions
- **THEN** the gateway selects a mutually supported version and retries the probe rather than falling back to `initialize`

#### Scenario: Backend returns an arbitrary error

- **WHEN** the backend answers the probe with any error that is not a recognized modern-era error
- **THEN** the gateway falls back to the legacy `initialize` handshake, offering revision `2025-11-25`, followed by `notifications/initialized`

#### Scenario: Backend stays silent in response to the probe

- **WHEN** the backend does not answer `server/discover` within the configured probe timeout
- **THEN** the gateway stops waiting and falls back to the legacy handshake, offering revision `2025-11-25`

#### Scenario: Era pinned by configuration

- **WHEN** a server is configured with `era: legacy`
- **THEN** no `server/discover` probe is sent at all and the connection is established directly with the `initialize` handshake

#### Scenario: Era pinned to modern and honored

- **WHEN** a server is configured with `era: modern` and the backend answers the probe
- **THEN** the connection proceeds in the modern era exactly as it would under `auto`

#### Scenario: Pinned era conflicts with the backend

- **WHEN** a server is configured with `era: modern` but the backend answers the probe with an error or does not answer it within the probe timeout
- **THEN** the connection fails with an error naming the server and the detected mismatch, and no `initialize` is sent

### Requirement: Legacy revision negotiation

The legacy era SHALL cover every published revision older than `2026-07-28`, not `2025-11-25` alone. The gateway SHALL adopt the revision a backend names in its `initialize` result rather than the one it offered.

#### Scenario: Backend answers with an older revision

- **WHEN** a backend answers the fallback `initialize` with a published revision older than the offered `2025-11-25`, down to the earliest published revision `2024-11-05`
- **THEN** the session proceeds at the revision the backend named, and its responses are normalized exactly as those of a `2025-11-25` backend

#### Scenario: Backend names a revision the gateway does not support

- **WHEN** a backend answers `initialize` with a revision outside the set the gateway supports
- **THEN** the connection fails with an error naming the server and the revision the backend reported

### Requirement: Caching of the detected era

A backend's detected era SHALL be cached for the lifetime of its process. The gateway MUST NOT repeat the probe on every request.

#### Scenario: Subsequent requests to the same process

- **WHEN** a second or later request reaches an already connected backend
- **THEN** the `server/discover` probe is not performed again

#### Scenario: Process restart

- **WHEN** a backend process has been restarted
- **THEN** the era is detected anew for the new process

### Requirement: Masking deprecated capabilities from backends

The gateway MUST NOT advertise the `roots`, `sampling`, `logging`, or `elicitation` capabilities to backends.

#### Scenario: Handshake with a legacy backend

- **WHEN** the gateway sends `initialize` to a legacy backend
- **THEN** the advertised capabilities contain neither `roots`, nor `sampling`, nor `elicitation`

#### Scenario: Backend violates the contract

- **WHEN** a backend issues a server-to-client `sampling/createMessage`, `roots/list`, or `elicitation/create` request despite the capability not being advertised
- **THEN** the gateway answers the backend with an error, records the event in its log, and does not disrupt the other backends

### Requirement: Normalization of legacy backend responses

Responses from legacy-era backends SHALL be normalized to the shape of revision `2026-07-28` before being returned to the client.

#### Scenario: Missing resultType

- **WHEN** a legacy backend returns a result without a `resultType` field
- **THEN** the gateway adds `resultType: "complete"`

#### Scenario: Obsolete "resource not found" error code

- **WHEN** a legacy backend returns an error with code `-32002` for a missing resource
- **THEN** the client receives an error with code `-32602`

#### Scenario: Missing cache fields

- **WHEN** a legacy backend returns a listing result without `ttlMs` and `cacheScope`
- **THEN** the gateway supplies those fields itself

### Requirement: Per-subject process isolation

A backend child process SHALL be identified by the pair of subject and resolved server configuration fingerprint. A process created for one subject MUST NOT be reused for another.

#### Scenario: Repeat request from the same subject

- **WHEN** a subject addresses a server whose resolved configuration has not changed for that subject
- **THEN** the already running process is reused

#### Scenario: Two subjects with identical configuration

- **WHEN** two different subjects address a server with byte-identical resolved configuration
- **THEN** a separate process is used for each subject

#### Scenario: Same subject changes the environment

- **WHEN** a subject repeats a request after changing an `env` value through the `x-mcp-config` header
- **THEN** a separate process is used rather than the previously started one

### Requirement: Pool limits and eviction

The gateway SHALL bound the number of concurrently live child processes both globally and per subject, and SHALL terminate idle processes.

#### Scenario: Idle timeout expires

- **WHEN** a process has served no request for longer than the configured idle period
- **THEN** the process is terminated and its slot in the pool is released

#### Scenario: Global limit reached

- **WHEN** a new process is required while the global limit is exhausted
- **THEN** the least recently used process is terminated before the new one is created

#### Scenario: Per-subject limit reached

- **WHEN** a subject requests a process beyond its own limit
- **THEN** the request is rejected with an error that explicitly names the exhausted subject quota

### Requirement: Child process lifecycle

The gateway SHALL terminate child processes predictably, leaving no orphaned descendants.

#### Scenario: Graceful shutdown

- **WHEN** the gateway finishes with a backend
- **THEN** the process's standard input is closed first and the gateway waits for it to exit on its own

#### Scenario: Process does not exit on its own

- **WHEN** the process has not exited within the allotted time after its standard input was closed
- **THEN** forced termination is applied, covering the process's entire descendant tree

#### Scenario: Unexpected process exit

- **WHEN** a backend process exits on its own
- **THEN** requests in flight fail with a retryable error, and the next request to that backend starts a fresh process

#### Scenario: Backend writes to standard error

- **WHEN** a backend writes to its standard error stream
- **THEN** the gateway records the output in its log and MUST NOT treat the mere act of writing as a failure
