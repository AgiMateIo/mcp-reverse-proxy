# Deploying mcp-reverse-proxy

The gateway exposes one HTTP Streamable MCP endpoint speaking revision
`2026-07-28` and runs the configured stdio MCP servers as its own child
processes. Everything below follows from that: the gateway executes programs on
the host, holds each subject's secrets in those processes' environments, and
accepts a header that can change what is executed.

## Running it

```
mcp-reverse-proxy \
  -config /etc/mcp-reverse-proxy/config.json \
  -resource https://gateway.example.com/mcp \
  -issuer https://auth.example.com/ \
  -key https://auth.example.com/=/etc/mcp-reverse-proxy/auth.pem \
  -addr 127.0.0.1:8080
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config` | — | Path to the configuration file below. Required. |
| `-resource` | — | The canonical URI clients reach this endpoint by, and the audience their tokens must name. Required; a fragment is stripped, and a relative URI is refused. |
| `-issuer` | — | An authorization server to trust. Required; repeat for several. |
| `-key` | — | An issuer's public key, as `issuer=path` or `issuer#kid=path`, PEM-encoded (`PUBLIC KEY` or `CERTIFICATE`). Required; repeat for several. Only the public half is ever read — a resource server verifies signatures and has no use for a private key. |
| `-addr` | `127.0.0.1:8080` | Address to listen on. Bind to a loopback address and terminate TLS in front of it. |
| `-path` | `/mcp` | Path the MCP endpoint is served at. The metadata document is always at `/.well-known/oauth-protected-resource`. |
| `-shutdown-timeout` | `30s` | How long a shutdown waits for requests in flight before it stops waiting. |

Everything that can fail at startup does: a missing configuration file, a
`define-new` policy without a command allowlist, an unreadable key, a resource
that is not an absolute URI. On success the gateway writes `listening on <addr>`
to standard output, which is how a deployment binding port 0 learns where it is.

`SIGINT` or `SIGTERM` stops it: the listener closes first, requests in flight are
given the grace period, and only then are the backend processes stopped — in
that order, so that no request is answered by a process that has already been
killed.

## Configuration file

One JSON file, named with `-config`. Unknown fields are refused rather
than ignored, so a typo fails at startup instead of changing behavior silently.

```json
{
  "servers": [
    {
      "id": "gh",
      "command": "/usr/local/bin/mcp-github",
      "args": ["--read-only"],
      "env": { "GITHUB_TOKEN": "…" },
      "era": "auto"
    }
  ],
  "limits": {
    "maxProcesses": 64,
    "maxProcessesPerSubject": 8,
    "idleTtl": "5m",
    "probeTimeout": "2s",
    "maxHeaderBytes": 8192
  },
  "policy": {
    "mode": "off",
    "commandAllowlist": ["/usr/local/bin/mcp-github"],
    "envDenylist": ["LD_PRELOAD", "DYLD_INSERT_LIBRARIES"]
  }
}
```

### `servers`

The backends the deployment declares. May be empty only under the `define-new`
policy mode, where every server arrives in the request header instead.

| Option | Meaning |
| --- | --- |
| `id` | Namespace for the server's tools, prompts and resources. Clients see `<id>__<name>`, and resources as `mcp-proxy://<id>/<encoded URI>`. Must be unique; a collision in the resulting names fails startup rather than picking a winner. |
| `command` | The executable to run. Started in its own process group so that termination reaches the whole descendant tree. |
| `args` | Arguments passed to it. Optional. |
| `env` | Environment given to the child process. Usually where the subject's credentials live, which is why the process pool is keyed by subject. Never logged. |
| `era` | Protocol era of the backend: `auto`, `modern` or `legacy`. Defaults to `auto`. |

`era` is worth setting explicitly:

- `auto` — probe with `server/discover`, fall back to the legacy `initialize`
  handshake if the probe fails. Correct for anything, and costs one probe
  timeout on every cold start against a backend that answers nothing.
- `modern` — probe, and refuse to fall back. A backend that silently degrades to
  legacy becomes a connection error instead of a quiet change of behavior.
- `legacy` — send no probe at all. Saves the probe timeout on every cold start,
  and cold starts are frequent because the pool is keyed by subject.

### `limits`

Process count grows as subjects × servers, so these are operational necessities
rather than tuning knobs. Each has a default, but a deployment is expected to
set its own.

| Option | Default | Meaning |
| --- | --- | --- |
| `maxProcesses` | 64 | Live child processes across all subjects. When the limit is reached, the least recently used idle backend is evicted; if every backend is serving a request, the new request is refused rather than interrupting somebody else's. |
| `maxProcessesPerSubject` | 8 | Live child processes for one subject, so that one subject cannot consume the host on everyone else's behalf. Exceeding it refuses the request naming quota exhaustion. |
| `idleTtl` | `5m` | How long a backend may serve no request before it is stopped. |
| `probeTimeout` | `2s` | Bound on the `server/discover` probe. Without it a legacy backend that answers nothing hangs the connection until the request's context expires. |
| `maxHeaderBytes` | 8192 | Cap on the `x-mcp-config` header value. Reverse proxies commonly cap a header at 8 KiB; a larger value here would only be refused further out. |

Durations are strings Go's `time.ParseDuration` accepts (`"90s"`, `"5m"`,
`"1h30m"`).

### `policy`

What the `x-mcp-config` request header may do. Its zero value is the closed one:
header configuration is opted into, not discovered to be on.

| Option | Meaning |
| --- | --- |
| `mode` | `off`, `env-only`, `override-known` or `define-new`. Defaults to `off`. |
| `commandAllowlist` | Executables a header may cause to run. Enforced in every mode, and mandatory under `define-new` — startup fails without it. |
| `envDenylist` | Environment keys a header may never set, in any mode. `LD_PRELOAD` and its equivalents belong here: setting one turns an allowed command into an arbitrary one. |

## The `x-mcp-config` header

The header carries a JSON document of the same shape as the file's `servers`,
with every field but `id` optional:

```json
{"servers": [{"id": "gh", "env": {"GITHUB_TOKEN": "…"}}]}
```

Merge rules: `env` merges key-wise with the base server's environment; every
other field replaces the base wholesale. A patch whose `id` is unknown declares
a new server.

### Policy modes

| Mode | What the header may do | Required token scope |
| --- | --- | --- |
| `off` | Nothing. A request that carries the header at all is refused. | — |
| `env-only` | Override `env` of a server the file already declares. | `mcp:config:env` |
| `override-known` | Override any field of a server the file already declares, including `command` and `args`. | `mcp:config:override` |
| `define-new` | Additionally declare servers the file never mentioned. | `mcp:config:define` |

Two layers gate every mode above `off`: the deployment's `mode` is the ceiling,
and the token's scopes decide what the subject may reach within it. A subject
whose token lacks the scope gets `403` with
`WWW-Authenticate: Bearer error="insufficient_scope"` naming every missing scope
at once, so one step-up authorization is enough. A header asking for more than
the deployment's ceiling gets a plain refusal with no challenge: no token could
change it, and a challenge would send the client on an authorization trip that
cannot help.

`off` refuses a present header rather than ignoring it. Ignoring it would leave
the subject believing its configuration was applied while it worked against a
different set of servers.

### `define-new` is remote code execution

Under `define-new` an authenticated subject chooses `command`, `args` and `env`
for a process the gateway runs on the host as its own user. That is arbitrary
code execution by design, not a side effect.

Enable it only where all of these hold:

- The `commandAllowlist` is a short list of executables that are safe to run
  with attacker-chosen arguments. Startup refuses `define-new` without one.
- The `envDenylist` covers the loader variables that turn an allowed command
  into an arbitrary one — at least `LD_PRELOAD` and `DYLD_INSERT_LIBRARIES`.
- The `mcp:config:define` scope is granted deliberately, to a small set of
  subjects, by an authorization server you control.
- The host treats the gateway as a trust boundary: dedicated user, no ambient
  cloud credentials, filesystem and network reachable from it kept to what the
  backends need.

`env` values are handed to the child process, so a subject with any elevated
mode can read them back only through what the backend does with them — but the
values themselves are the subject's own secrets, which is the reason the pool
never shares a process between subjects.

## Keep `x-mcp-config` out of your reverse proxy's logs

The header carries credentials. The gateway redacts it everywhere on its own
side — logs, errors, telemetry — but anything in front of the gateway has its
own logging, and access logs are commonly shipped and retained far longer than
the credentials' lifetime.

Suppress the header explicitly. It is not enough to avoid naming it: many
default log formats include all request headers.

nginx logs no headers beyond those named in `log_format`, so the rule is simply
never to name it, and to clear it if you construct a debug format:

```nginx
# Never reference $http_x_mcp_config in a log_format.
log_format mcp '$remote_addr "$request" $status $body_bytes_sent';
access_log /var/log/nginx/mcp.log mcp;
```

Envoy, whose default access log is configurable per header, needs the header
omitted from the format and stripped before any downstream logging tap:

```yaml
access_log:
  - name: envoy.access_loggers.file
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
      path: /var/log/envoy/mcp.log
      # No REQ(x-mcp-config) anywhere in this format.
      log_format:
        text_format_source:
          inline_string: "%START_TIME% %REQ(:METHOD)% %REQ(:PATH)% %RESPONSE_CODE%\n"
```

Also check, in this order: the TLS terminator or CDN in front of the proxy, any
request-mirroring or tracing filter (OpenTelemetry HTTP instrumentation captures
request headers when configured to), and crash reporters that serialize the
request.

## Authorization

The gateway is an OAuth 2.1 resource server only; the authorization server is
external. It publishes RFC 9728 protected resource metadata at
`/.well-known/oauth-protected-resource`, naming the canonical resource URI, the
accepted issuers and the supported scopes. Tokens are validated for signature,
expiry, issuer and audience against that canonical URI, and are accepted only
from the `Authorization` header — a token in the query string is refused.

The subject is the pair `iss` + `sub`, not `sub` alone: `sub` is unique only
within an issuer, and the metadata may name several. A validated token without
`sub` is refused.

Scopes: `mcp:access` for the endpoint itself, plus the per-mode scopes in the
table above.

## Operating notes

- **Process count** grows as subjects × servers. Watch the pool's size,
  eviction and rejection counts; a rising rejection count means
  `maxProcesses` or `maxProcessesPerSubject` is below what the deployment
  actually needs.
- **A backend that is down** does not fail a listing: its entries are omitted,
  the backend is named in the log, and the result's `ttlMs` is shortened so
  clients come back sooner. Calling one of its tools does fail, naming the
  backend.
- **Restarts** are transparent. A backend that exits unexpectedly is replaced on
  the next request; the in-flight one fails as retryable, and the protocol is
  stateless, so repeating it is all a client has to do.
- **Shutdown** closes each backend's stdin first, then escalates to `SIGTERM`
  and `SIGKILL` across the process group, so descendants a backend spawned go
  with it.
