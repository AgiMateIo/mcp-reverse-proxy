## Purpose

Defines the gateway's behavior as an OAuth 2.1 Resource Server: publishing protected resource metadata, validating presented tokens, deriving the subject identifier, and relating token scopes to the header configuration modes.

## ADDED Requirements

### Requirement: Publishing protected resource metadata

The gateway SHALL publish an OAuth 2.0 Protected Resource Metadata document per RFC 9728 and name its associated authorization servers in it.

#### Scenario: Requesting the metadata

- **WHEN** a client requests `/.well-known/oauth-protected-resource`
- **THEN** a document containing the `resource`, `authorization_servers`, and `scopes_supported` fields is returned

#### Scenario: Canonical resource identifier

- **WHEN** the metadata document is produced
- **THEN** the `resource` field holds the gateway's canonical URI with no fragment

#### Scenario: Minimal scope set

- **WHEN** the metadata document is produced
- **THEN** `scopes_supported` holds the base access scope and MUST NOT hold `offline_access`

### Requirement: Requiring a presented token

The gateway SHALL reject requests lacking a valid access token and SHALL point the client at its metadata.

#### Scenario: Request without a token

- **WHEN** a request reaches the MCP endpoint without an `Authorization` header
- **THEN** `401 Unauthorized` is returned with a `Bearer` scheme `WWW-Authenticate` header carrying the `resource_metadata` and `scope` parameters

#### Scenario: Token in the query string

- **WHEN** a token is supplied as a query string parameter rather than in the `Authorization` header
- **THEN** `401 Unauthorized` is returned and the token MUST NOT be accepted

### Requirement: Access token validation

The gateway SHALL verify that a presented token was issued by a trusted authorization server and is intended for this resource.

#### Scenario: Valid token

- **WHEN** an unexpired token from a trusted issuer is presented whose audience matches the gateway's canonical URI
- **THEN** the request is processed

#### Scenario: Token intended for another audience

- **WHEN** a correctly signed token is presented whose audience does not match the gateway's canonical URI
- **THEN** `401 Unauthorized` is returned and the request MUST NOT be processed

#### Scenario: Expired token

- **WHEN** a token past its expiry is presented
- **THEN** `401 Unauthorized` is returned

#### Scenario: Unknown issuer

- **WHEN** a token is presented whose issuer is not listed in the resource metadata
- **THEN** `401 Unauthorized` is returned

### Requirement: Subject identifier

The gateway SHALL derive the subject identifier from the issuer and subject pair of the validated token and SHALL use it as a component of the process identifier in the pool.

#### Scenario: Forming the identifier

- **WHEN** a token is validated successfully
- **THEN** the subject identifier is formed from the token's `iss` and `sub` values

#### Scenario: Matching subjects from different issuers

- **WHEN** two tokens from different issuers carry the same `sub` value
- **THEN** the subjects are treated as distinct and do not share child processes

#### Scenario: Token lacking mandatory claims

- **WHEN** a validated token carries no `sub` claim
- **THEN** `401 Unauthorized` is returned

### Requirement: Privileges for header configuration

Header policy modes beyond the base one SHALL require dedicated scopes in the token.

#### Scenario: Insufficient privileges to declare a server

- **WHEN** a subject presents a header declaring a new server while its token lacks the scope for that mode
- **THEN** `403 Forbidden` is returned with a `WWW-Authenticate` header carrying `error="insufficient_scope"`, the required `scope`, and `resource_metadata`

#### Scenario: Complete set of required scopes in one response

- **WHEN** a request lacks several scopes at once
- **THEN** all of them are listed in a single `WWW-Authenticate` header, and the gateway MUST NOT emit them one at a time across successive responses

#### Scenario: Sufficient privileges

- **WHEN** the subject's token carries the scope corresponding to the requested mode
- **THEN** the header configuration is applied within the bounds of that mode

### Requirement: No token transit

An access token presented to the gateway MUST NOT be passed to backend child processes.

#### Scenario: Starting a backend

- **WHEN** the gateway starts a child process on behalf of a subject
- **THEN** neither the process environment variables nor its arguments contain the access token presented to the gateway

#### Scenario: Source of backend credentials

- **WHEN** a backend requires credentials of its own
- **THEN** they are taken exclusively from the server's resolved configuration
