# sesh-shim

`sesh-shim` is the A2A v1.0 HTTPS+JSON-RPC gateway for sesh. It accepts
incoming agent-to-agent requests over HTTPS and bridges them to a single local
adapter agent running on the sesh NATS mesh, using NATS request/reply on the
`agents.prompt.cc.<machine>.<agent>.<name>` subject hierarchy.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `SESH_SHIM_LISTEN` | `0.0.0.0:8443` | HTTPS bind address |
| `NATS_URL` | _(hub.nats.url)_ | NATS server URL; falls back to `~/.sesh/hub.nats.url` via sesh-ops/conn |
| `SESH_SHIM_TLS_CERT` | _(self-signed in dev)_ | PEM TLS certificate file path |
| `SESH_SHIM_TLS_KEY` | _(self-signed in dev)_ | PEM TLS private key file path |
| `SESH_SHIM_SIGNING_KEY` | _(ephemeral in dev)_ | PEM ES256 private key for AgentCard JWS |
| `SESH_SHIM_KID` | _(auto-derived)_ | Key ID published in JWKS |
| `SESH_SHIM_AUTH` | `jwt` | Auth mode: `jwt` or `none-dev-only` |
| `SESH_SHIM_JWKS_URL` | _(required when auth=jwt)_ | Upstream JWKS URL for JWT validation |
| `SESH_SHIM_AGENT` | _(required)_ | Adapter agent token to advertise |
| `SESH_SHIM_OWNER` | _(required)_ | Owner token to advertise |
| `SESH_SHIM_NAME` | _(defaults to agent)_ | Adapter instance name (third subject token) |
| `SESH_SHIM_SCOPE_KIND` | `project` | Task scope kind for KV bucket naming |
| `SESH_SHIM_SCOPE_ID` | _(required)_ | Task scope ID for KV bucket naming |
| `SESH_SHIM_GATEWAY_URL` | _(required)_ | Public-facing URL advertised in the AgentCard |
| `SESH_SHIM_MACHINE` | _(os.Hostname)_ | Machine token (first subject segment); sanitized automatically |
| `SESH_SHIM_DEV` | `false` | Dev mode: self-signed TLS + ephemeral signing key permitted |
| `SESH_SHIM_SHUTDOWN_GRACE` | `5s` | Max drain/shutdown wait |
| `SESH_SHIM_PUSH_ENCRYPTION_KEY` | _(ephemeral in dev)_ | Hex AES-256-GCM key (64 chars) or file path for push notifications |
| `SESH_SHIM_PUSH_WORKER_DISABLED` | `false` | Disable the JetStream delivery worker (CRUD still works) |
| `SESH_SHIM_PUSH_MAX_RETRIES` | `4` | Max push delivery retries (total attempts = 1 + this) |

## Quick start (dev mode)

```sh
SESH_SHIM_DEV=true \
SESH_SHIM_AGENT=claude \
SESH_SHIM_OWNER=myuser \
SESH_SHIM_SCOPE_ID=my-project \
SESH_SHIM_GATEWAY_URL=https://localhost:8443 \
SESH_SHIM_AUTH=none-dev-only \
  sesh-shim
```

In dev mode, TLS cert and signing key are auto-generated; the NATS URL is read
from `~/.sesh/hub.nats.url` if `NATS_URL` is not set.

## Help

```sh
sesh-shim --help
```
