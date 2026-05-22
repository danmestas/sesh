# @agent-ops/sesh-channels

Canonical role/class types, validators, and defaults for the **sesh agent registration protocol**. TS port of [`github.com/danmestas/sesh/internal/agentmeta`](https://github.com/danmestas/sesh/tree/main/internal/agentmeta).

Use this package in any NATS-channel adapter that registers an agent on the sesh mesh (claude / pi / omp / gemini / grok / your own). It owns the regex, the length bound, the class enum, and the defaulting policy — adapter code becomes a one-line import.

## Install

```bash
npm i @agent-ops/sesh-channels
# or
bun add @agent-ops/sesh-channels
```

## Usage

The 90% case: read role + class from env at boot, hand the result to your `AgentService` / `svcm.add` metadata.

```ts
import { readRoleClass } from "@agent-ops/sesh-channels";
import { AgentService } from "@synadia-ai/agent-service";

const { role, class: cls } = readRoleClass(); // throws ConfigError on invalid input

const svc = new AgentService({
  nc,
  agent: "claude-code",
  owner: process.env.SESH_OWNER ?? "anon",
  name: "main",
  metadata: { role, class: cls /* ... your other metadata ... */ },
});
```

Or, for the full adapter config (NATS_URL + agent + owner + session + role + class) in one call:

```ts
import { readAdapterConfig } from "@agent-ops/sesh-channels";

const cfg = readAdapterConfig("claude-code"); // throws ConfigError on bad role/class

const svc = new AgentService({
  nc,
  agent: cfg.agent,
  owner: cfg.owner,
  name: cfg.session || "main",
  metadata: { role: cfg.role, class: cfg.class, session: cfg.session },
});
```

## API

| Symbol | Purpose |
|---|---|
| `readRoleClass()` | Read `SESH_ROLE` / `SESH_CLASS` from env, apply defaults, validate. Returns `{ role, class }`. Throws `ConfigError` on invalid input. |
| `readAdapterConfig(defaultAgent)` | Compose `readRoleClass` with NATS_URL / SESH_AGENT / SESH_OWNER / SESH_SESSION env reads. Returns a full `AdapterConfig`. Throws on bad role/class. |
| `ConfigError` | Thrown on invalid input. Distinct class so callers can `instanceof` check. |
| `AgentClass` (type) | Literal union `"active" \| "observer"`. |
| `AdapterRoleClass` (type) | `{ role: string; class: AgentClass }`. |
| `AdapterConfig` (type) | `AdapterRoleClass` + NATS_URL / agent / owner / session. |

Internal helpers (`validateRole`, `validateClass`, defaults) are intentionally not exported — callers shouldn't bypass validation, and the boundary is `readRoleClass` / `readAdapterConfig`.

## Canonical rules

Mirrored verbatim from [the proposal](https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md#canonical-roleclass-rules-cite-this-section-verbatim):

```
role regex     : ^[a-z0-9_-]+$
role length    : 1..63 bytes inclusive
role default   : "worker"
class values   : "active" | "observer"
class default  : "active"
```

Defaulting rule: empty / unset → apply default. Any other value: validate; on failure, throw at boot.

## Versioning

Tracks sesh's agent-registration wire format. Minor bumps for additive metadata fields (e.g., capabilities); major bumps for changes that break adapter compatibility.

## Source of truth

When the rules change, this package and `internal/agentmeta` MUST be updated together. The proposal at [`docs/proposals/2026-05-21-agent-role-registration.md`](https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md) is the canonical document.
