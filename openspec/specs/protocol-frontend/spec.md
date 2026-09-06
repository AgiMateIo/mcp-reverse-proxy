# protocol-frontend Specification

## Purpose

Defines the gateway's external contract: a single HTTP Streamable endpoint speaking MCP revision `2026-07-28` — how the protocol version is negotiated, which headers are mandatory, what shape results take, and how clients may cache them.

## Requirements

### Requirement: Per-request protocol version negotiation

The gateway SHALL determine the protocol version independently for each request from the `io.modelcontextprotocol/protocolVersion` field in `_meta`. The gateway MUST NOT require any preliminary handshake.

#### Scenario: Supported version

- **WHEN** a request carries a version in `_meta` that the gateway supports
- **THEN** the request is processed and the result carries `io.modelcontextprotocol/serverInfo` in its `_meta`

#### Scenario: Unsupported version

- **WHEN** a request carries a version the gateway does not support
- **THEN** an error with code `-32022` is returned, carrying `data.supported` listing the supported versions and `data.requested`

#### Scenario: Header disagrees with request body

- **WHEN** the `MCP-Protocol-Version` header value differs from the version in the request body's `_meta`
- **THEN** an error with code `-32020` is returned

#### Scenario: Legacy handshake attempt

- **WHEN** a client sends an `initialize` request
- **THEN** an error is returned whose message names the protocol versions the gateway supports

### Requirement: Capability discovery via server/discover

The gateway SHALL implement the `server/discover` RPC and return its supported protocol versions, aggregated capabilities, and server identity.

#### Scenario: Discovery before any other request

- **WHEN** a client calls `server/discover`
- **THEN** the result contains `supportedVersions`, `capabilities`, and `serverInfo`

#### Scenario: Deprecated capabilities are not advertised

- **WHEN** a client calls `server/discover`
- **THEN** the result MUST NOT contain the `sampling`, `roots`, `logging`, or `elicitation` capabilities, even if some backend supports them

#### Scenario: Capability set depends on request context

- **WHEN** two subjects call `server/discover` with different `x-mcp-config` values
- **THEN** each receives the capability set corresponding to its own permitted configuration

### Requirement: Mandatory request headers

The gateway SHALL require the `Mcp-Method` and `Mcp-Name` headers on every POST request, as mandated by revision `2026-07-28`.

#### Scenario: Missing mandatory header

- **WHEN** a POST request arrives without the `Mcp-Method` header
- **THEN** `400 Bad Request` is returned

#### Scenario: Header contradicts the body

- **WHEN** the `Mcp-Method` value does not match the `method` field of the request body
- **THEN** an error with code `-32020` is returned

### Requirement: Result shape

Every result the gateway returns SHALL carry a `resultType` field. The gateway SHALL set it to `"complete"` regardless of the era of the backend that produced the result.

#### Scenario: Result from a legacy backend

- **WHEN** a legacy-era backend returns a result without a `resultType` field
- **THEN** the client receives the result with `resultType: "complete"`

#### Scenario: Interim results are never returned

- **WHEN** any request is processed
- **THEN** the gateway MUST NOT return a result with `resultType: "input_required"`, because the Multi Round-Trip Requests pattern is out of scope

### Requirement: Cache semantics of listing results

Results of `tools/list`, `prompts/list`, `resources/list`, `resources/templates/list`, and `resources/read` SHALL carry the `ttlMs` and `cacheScope` fields. The `cacheScope` value MUST always be `"private"`.

#### Scenario: Listing tools

- **WHEN** a client calls `tools/list`
- **THEN** the result carries `ttlMs` and `cacheScope: "private"`

#### Scenario: Public caching is forbidden

- **WHEN** any cacheable result is returned
- **THEN** the `cacheScope` value MUST NOT be `"public"`, because the surface depends on the subject and on the `x-mcp-config` header

#### Scenario: Shortened lifetime when a backend is unavailable

- **WHEN** `tools/list` runs while one of the backends is unavailable
- **THEN** the returned `ttlMs` is smaller than it would be with every backend available

### Requirement: Deterministic listing order

For an unchanged resolved configuration, repeated calls to listing methods SHALL return their entries in the same order.

#### Scenario: Repeated listing

- **WHEN** `tools/list` is called twice in succession with no change to configuration or backend set
- **THEN** the order of entries is identical in both results

### Requirement: Rejection of methods removed from the specification

The gateway SHALL reject methods that revision `2026-07-28` removed.

#### Scenario: Calling a removed method

- **WHEN** a client calls `ping`, `logging/setLevel`, `resources/subscribe`, or `resources/unsubscribe`
- **THEN** a "method not found" error with code `-32601` is returned
