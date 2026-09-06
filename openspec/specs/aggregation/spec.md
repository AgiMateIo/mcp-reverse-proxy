# aggregation Specification

## Purpose

Defines how several independent stdio MCP servers appear to a client as one surface: how tools and prompts are named, how resource URIs are mapped, where calls are routed, what happens when an individual backend is unavailable, and how change notifications are merged.

## Requirements

### Requirement: Tool and prompt namespacing

Tool, prompt, and prompt template names SHALL be prefixed with the backend identifier from the configuration, separated by two underscore characters.

#### Scenario: Naming a tool

- **WHEN** the backend identified as `gh` provides a tool named `search`
- **THEN** the client sees it as `gh__search`

#### Scenario: Merged listing

- **WHEN** a client calls `tools/list` with three backends connected
- **THEN** the result contains the tools of all three backends, each under its prefixed name

#### Scenario: Prefixed name exceeds the length limit

- **WHEN** a resulting prefixed name exceeds the maximum allowed tool name length
- **THEN** configuration resolution fails with an error naming the backend and the tool

### Requirement: Name collision detection

The gateway SHALL fail configuration resolution when resulting names collide, and MUST NOT silently pick a winner.

#### Scenario: Identical resulting names

- **WHEN** two backends produce the same resulting tool name
- **THEN** configuration resolution fails with an error naming both backends and the conflicting name

#### Scenario: Duplicate backend identifiers

- **WHEN** the resolved configuration contains two servers with the same identifier
- **THEN** configuration resolution fails with an error

### Requirement: Call routing

The gateway SHALL determine the target backend from the name prefix and SHALL pass the backend the original, unprefixed name.

#### Scenario: Calling a tool

- **WHEN** a client calls `tools/call` with the name `gh__search`
- **THEN** the call is routed to backend `gh` with the tool name `search`

#### Scenario: Unknown prefix

- **WHEN** a client calls a tool whose prefix matches no backend in the resolved configuration
- **THEN** a "tool not found" error is returned

#### Scenario: Name without a prefix

- **WHEN** a client calls a tool whose name contains no namespace separator
- **THEN** a "tool not found" error is returned, and the request MUST NOT be broadcast to every backend

### Requirement: Resource URI mapping

Backend resource identifiers SHALL be wrapped into gateway URIs of the form `mcp-proxy://<backend-identifier>/<percent-encoded original URI>`, and SHALL be unwrapped again on read.

#### Scenario: Listing resources

- **WHEN** the backend `docs` provides a resource with URI `file:///readme.md`
- **THEN** the client sees it under the URI `mcp-proxy://docs/file%3A%2F%2F%2Freadme.md`

#### Scenario: Reading a resource

- **WHEN** a client calls `resources/read` with a gateway URI
- **THEN** the request is routed to the owning backend with the resource's original URI

#### Scenario: Malformed gateway URI

- **WHEN** a client supplies a URI that does not parse as a gateway URI or refers to an unknown backend
- **THEN** an error with code `-32602` is returned

### Requirement: Resilience to an unavailable backend

An individual backend being unavailable MUST NOT cause listing methods to fail as a whole.

#### Scenario: Listing while a backend is unavailable

- **WHEN** one of the backends does not respond during `tools/list`
- **THEN** the entries of the available backends are returned, and the unavailability is recorded in the log naming the backend

#### Scenario: Calling a tool of an unavailable backend

- **WHEN** a client calls a tool belonging to an unavailable backend
- **THEN** an error is returned that explicitly identifies the backend as unavailable

#### Scenario: Every backend unavailable

- **WHEN** no backend in the resolved configuration responds
- **THEN** listing methods return empty lists rather than an error

### Requirement: Merging of change notifications

The gateway SHALL merge `listChanged` notifications from all backends into the client's single `subscriptions/listen` stream, regardless of the source backend's era.

#### Scenario: Client subscribes

- **WHEN** a client opens `subscriptions/listen` subscribing to `toolsListChanged`
- **THEN** the gateway acknowledges the subscription and tags subsequent notifications with the `io.modelcontextprotocol/subscriptionId` field

#### Scenario: Notification from a legacy backend

- **WHEN** a legacy-era backend emits a tool list change notification
- **THEN** the client receives it on its `subscriptions/listen` stream in the shape of revision `2026-07-28`

#### Scenario: Notification outside the subscription

- **WHEN** a backend emits a prompt list change notification while the client subscribed only to `toolsListChanged`
- **THEN** the notification is not forwarded to the client

#### Scenario: Stream breaks

- **WHEN** a client's `subscriptions/listen` stream is broken
- **THEN** the gateway releases the resources tied to that subscription while the backends continue serving other clients
