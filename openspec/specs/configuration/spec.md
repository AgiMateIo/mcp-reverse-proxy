# configuration Specification

## Purpose

Defines where the gateway obtains stdio server connection parameters: the base configuration file and the `x-mcp-config` HTTP header, the rules for merging them, the policy modes that bound what the header may do, and the handling of secrets.

## Requirements

### Requirement: Base configuration from a file

The gateway SHALL read its list of servers from a configuration file at startup. Each server is described by an identifier, an executable command, arguments, environment variables, and an era mode.

#### Scenario: Valid file

- **WHEN** the gateway starts with a valid configuration file
- **THEN** the servers it lists become the base set for every subject

#### Scenario: Invalid file

- **WHEN** the configuration file is missing, does not parse, or contains a server lacking a mandatory field
- **THEN** startup fails with an error naming the file and the specific problem

#### Scenario: Default era mode

- **WHEN** a server description omits the `era` field
- **THEN** the `auto` mode applies to that server

### Requirement: Configuration from a request header

The gateway SHALL accept supplementary configuration in the `x-mcp-config` header as a JSON document.

#### Scenario: Valid header

- **WHEN** a request carries `x-mcp-config` with valid JSON permitted by the effective policy
- **THEN** the configuration applies to that request only and affects neither other requests nor other subjects

#### Scenario: Malformed JSON

- **WHEN** the header value does not parse as JSON
- **THEN** `400 Bad Request` is returned with a message that does not contain the header value itself

#### Scenario: Size limit exceeded

- **WHEN** the header value exceeds the gateway's configured size limit
- **THEN** `400 Bad Request` is returned with a message that explicitly names the exceeded limit

### Requirement: Configuration merge rules

Header configuration SHALL be layered over the base configuration. Environment variables merge key by key; all other fields are replaced wholesale.

#### Scenario: Overriding a single environment variable

- **WHEN** the header supplies `env` with one key for a server that has three environment variables in the base configuration
- **THEN** the resolved configuration holds three variables, the one matching by key replaced with the header's value

#### Scenario: Replacing arguments

- **WHEN** the header supplies `args` for a known server
- **THEN** the resolved `args` fully replaces the base value rather than extending it

#### Scenario: Declaring a new server

- **WHEN** the header describes a server whose identifier is absent from the base configuration
- **THEN** under a permitting policy the server is added to the resolved set for that request

### Requirement: Header policy modes

The gateway SHALL support the modes `off`, `env-only`, `override-known`, and `define-new`, bounding what the header may do. The effective mode is determined by the deployment configuration and the subject's privileges.

#### Scenario: Mode off

- **WHEN** the `off` mode is in effect and a request carries the `x-mcp-config` header
- **THEN** the request is rejected with an error stating plainly that header configuration is disabled, and the configuration MUST NOT be applied silently

#### Scenario: Mode off offers no privilege escalation

- **WHEN** a request is rejected because of the `off` mode
- **THEN** the response MUST NOT carry an `insufficient_scope` challenge, because the mode is set by the deployment and no widening of token privileges can change it

#### Scenario: Mode off does not obstruct ordinary requests

- **WHEN** the `off` mode is in effect and a request carries no `x-mcp-config` header
- **THEN** the request is served against the base configuration without any restriction

#### Scenario: Mode env-only permits environment overrides

- **WHEN** the `env-only` mode is in effect and the header overrides `env` of a known server
- **THEN** the configuration is applied

#### Scenario: Mode env-only forbids command substitution

- **WHEN** the `env-only` mode is in effect and the header overrides `command` or `args` of a known server
- **THEN** the request is rejected with an insufficient privileges error

#### Scenario: Mode override-known forbids declaring a new server

- **WHEN** the `override-known` mode is in effect and the header describes a server absent from the base configuration
- **THEN** the request is rejected with an insufficient privileges error

#### Scenario: Mode define-new

- **WHEN** the `define-new` mode is in effect and the header describes a new server with a permitted command
- **THEN** the configuration is applied

#### Scenario: Default mode

- **WHEN** no policy mode is set in the deployment configuration
- **THEN** the `off` mode applies

### Requirement: Constraints on executable commands and environment

The gateway SHALL check commands against an allowlist and SHALL reject denied environment variable keys regardless of the effective policy mode.

#### Scenario: Command outside the allowlist

- **WHEN** a server's resolved configuration names a command absent from the allowlist
- **THEN** the request is rejected even under the `define-new` mode

#### Scenario: Denied environment key

- **WHEN** the header supplies an environment variable whose key is on the denylist
- **THEN** the request is rejected with an error naming the rejected key

#### Scenario: Empty command allowlist

- **WHEN** no command allowlist is configured while the `define-new` mode is in effect
- **THEN** gateway startup fails with a configuration error

### Requirement: Handling of secrets in configuration

The `x-mcp-config` header value and backend environment variables MUST NOT appear in logs, error messages, or telemetry.

#### Scenario: Rejection by policy

- **WHEN** a request is rejected for violating header policy
- **THEN** the error message names the reason and the violated rule but contains no environment variable values

#### Scenario: Logging a request

- **WHEN** a request carrying `x-mcp-config` is written to the log
- **THEN** the header value is absent from the record or replaced by a placeholder

#### Scenario: Configuration fingerprint

- **WHEN** the resolved configuration is used as part of a process identifier in the pool
- **THEN** an irreversible fingerprint of it is used, and the original values are not retained in observable state
