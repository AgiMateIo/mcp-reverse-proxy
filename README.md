# mcp-reverse-proxy

One HTTP Streamable MCP endpoint, speaking revision `2026-07-28`, in front of
several local stdio MCP servers.

## What it does

The gateway serves a single modern MCP endpoint over HTTP and runs the
configured stdio MCP servers as its own child processes, fanning every request
out to them:

- **Bridges the two protocol eras.** Most stdio servers still speak revision
  `2025-11-25` or earlier. The gateway probes each backend with
  `server/discover` and falls back to the legacy `initialize` handshake, so a
  client on `2026-07-28` reaches a backend that has never heard of it.
- **Aggregates them into one surface.** Tools, prompts and resources of every
  backend appear under one listing, namespaced: `gh__create_issue`, and
  resources as `mcp-proxy://gh/<percent-encoded URI>`. The separator is two
  underscores, because a single one is ordinary inside tool names and could not
  be split back apart.
- **Authenticates and isolates.** It is an OAuth 2.1 resource server: tokens are
  validated for signature, expiry, issuer and audience, and the subject is the
  pair `iss` + `sub`. Every subject gets its own child processes, keyed by
  `(subject, fingerprint of the resolved server configuration)`.
- **Bounds what it spends.** Process count grows as subjects × servers, so the
  pool has limits, an idle TTL, LRU eviction and restart-on-exit.

## What it is for

Isolating stdio MCP servers on a remote or virtual server.

A stdio MCP server is a child process on the machine of whoever uses it. It has
no network address and no notion of authorization, and the credentials it needs
live in its environment — which means on every laptop that runs it. Sharing one
with a team means shipping the tokens along with it.

The gateway moves those servers onto a host you control. What was a local
subprocess becomes an endpoint with a URL, reachable by anyone your
authorization server issues a token to, and the credentials stay on that host.
Because the pool is keyed by subject, two people never share a process: one
subject's environment — their tokens — is never visible to another's backend.

Under the `x-mcp-config` header a subject may supply its own credentials per
request instead, which is the same isolation seen from the other side: the
gateway holds nothing, and each subject's values reach only their own processes.

## Running it

```console
$ go build ./cmd/mcp-reverse-proxy
$ ./mcp-reverse-proxy \
    -config /etc/mcp-reverse-proxy/config.yaml \
    -resource https://gateway.example.com/mcp \
    -issuer https://auth.example.com/ \
    -key https://auth.example.com/=/etc/mcp-reverse-proxy/auth.pem \
    -addr 127.0.0.1:8080
listening on 127.0.0.1:8080
```

Everything that can fail at startup does — a missing configuration file, an
unreadable key, a `define-new` policy without a command allowlist — so a gateway
that is listening is a gateway that is configured. The `listening on <addr>`
line goes to standard output, which is how a deployment binding port 0 learns
where it ended up. `SIGINT` or `SIGTERM` stops it: the listener closes first,
requests in flight get the grace period, and only then are the backends stopped.

There is **no unauthenticated mode**. Two public facts are all a client needs to
begin:

```console
$ curl http://127.0.0.1:8080/.well-known/oauth-protected-resource
{"resource":"https://gateway.example.com/mcp","authorization_servers":[…],
 "scopes_supported":["mcp:access"],"bearer_methods_supported":["header"]}

$ curl -X POST http://127.0.0.1:8080/mcp
401 Unauthorized
WWW-Authenticate: Bearer resource_metadata="…", scope="mcp:access"
```

The gateway is a resource server only; the authorization server is external and
its public key is named with `-key`. Two things a hand-written request has to
carry, both of which the SDK clients set for you:

- the `Mcp-Method` header on every POST, and `Mcp-Name` on `tools/call`, each
  matching the body;
- `io.modelcontextprotocol/protocolVersion` **and**
  `io.modelcontextprotocol/clientCapabilities` in `params._meta`.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config` | — | Path to the configuration file. Required. |
| `-resource` | — | The canonical URI clients reach this endpoint by, and the audience their tokens must name. Required; a relative URI is refused. |
| `-issuer` | — | An authorization server to trust. Required; repeat for several. |
| `-key` | — | An issuer's public key, as `issuer=path` or `issuer#kid=path`, PEM-encoded. Required; repeat for several. Only the public half is ever read. |
| `-addr` | `127.0.0.1:8080` | Address to listen on. Bind to loopback and terminate TLS in front of it. |
| `-path` | `/mcp` | Path the MCP endpoint is served at. The metadata document is always at `/.well-known/oauth-protected-resource`. |
| `-shutdown-timeout` | `30s` | How long a shutdown waits for requests in flight. |

Missing required flags are reported together, not one restart at a time.

Scopes: `mcp:access` for the endpoint itself, plus `mcp:config:env`,
`mcp:config:override` and `mcp:config:define` for the header policy modes below.

## Configuration

One YAML file, named with `-config`. Unknown fields are refused rather than
ignored, so a typo fails at startup instead of changing behavior silently.
[`config.example.yaml`](config.example.yaml) is this file with every option set
and commented.

```yaml
servers:
  - id: gh
    command: /usr/local/bin/mcp-github
    args: ["--read-only"]
    env:
      GITHUB_TOKEN: "…"
    era: auto

limits:
  maxProcesses: 64
  maxProcessesPerSubject: 8
  idleTtl: 5m
  probeTimeout: 2s
  maxHeaderBytes: 8192

policy:
  mode: "off"
  commandAllowlist: ["/usr/local/bin/mcp-github"]
  envDenylist: ["LD_PRELOAD", "DYLD_INSERT_LIBRARIES"]
```

- **`servers`** — the backends. `id` namespaces their tools and resources and
  must be unique; `env` is where the subject's credentials go and is never
  logged. `era` is `auto` (probe, fall back), `modern` (probe, refuse to fall
  back, so a backend that silently degrades becomes a connection error) or
  `legacy` (no probe, saving its timeout on every cold start). Defaults to
  `auto`.
- **`limits`** — the values above are the defaults. They exist because process
  count grows as subjects × servers; a deployment is expected to set its own.
- **`policy`** — what the `x-mcp-config` request header may do: `off`,
  `env-only`, `override-known` or `define-new`. Defaults to `off`, and the
  deployment's mode is a ceiling the token's scopes are then checked against.

Two notes worth having before the first deploy:

- **Quote `env` values YAML would type for you.** They are strings, so
  `API_VERSION: "2"`, not `API_VERSION: 2`. A number or a boolean is refused at
  startup; an unquoted date is refused too, because it is the one that would
  otherwise reach the backend rewritten.
- **`define-new` is remote code execution by design.** It lets an authenticated
  subject choose the command, arguments and environment of a process the gateway
  runs on the host. It requires a `commandAllowlist`, and reading
  [`docs/deployment.md`](docs/deployment.md) before it is enabled.

JSON configuration files still load — YAML is a superset — with one difference:
a duplicate key is refused rather than resolved to the last occurrence.

The `x-mcp-config` header carries a JSON document of the same shape, with every
field but `id` optional. `env` merges key-wise with the server's configured
environment; every other field replaces it. Its modes, scopes and merge rules
are in [`docs/deployment.md`](docs/deployment.md).

## Further reading

- [`docs/deployment.md`](docs/deployment.md) — every option, the `define-new`
  risk, and keeping the header out of a reverse proxy's logs.
- [`config.example.yaml`](config.example.yaml) — a commented configuration.
- [`openspec/`](openspec/) — the behavior specifications this implements.
