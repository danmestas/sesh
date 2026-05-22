# Agent Role & Class Registration

**Date:** 2026-05-21
**Status:** Proposed
**Author:** dmestas (synthesized from coordination-subject work)
**Related:**
- `docs/proposals/2026-05-20-sesh-parallel-coordination-subjects.md` (consumer of the role/class fields proposed here)
- `cli/agent_watcher.go:33-65` (registration site to extend)
- `cli/session.go:24-29` (`AgentRef` to extend)
- `internal/refagent/agent.go:172-179` (metadata builder to extend)

## Problem

The coordination-subject proposal needs role-aware routing (`sesh.task.<machine>.project.<id>.workers.implementer`). But today no agent **carries** a role on the wire. Agent registration captures only `agent`, `owner`, `protocol_version`, `session` (`internal/refagent/agent.go:172-179`). Sesh's `AgentRef` struct mirrors this (`cli/session.go:24-29`). There is no `role` field anywhere in the data model.

`orch-spawn` has `ORCH_ROLE` (passed to `orch-agent-shim` per `docs/orch-agent-shim.md:99-104`), but it never lands in `metadata.*` and is therefore invisible to `$SRV.INFO.agents` consumers and unable to drive subject subscriptions.

Without role/class on registration:
- Coordination subjects can be **published** but not **subscribed correctly** — adapters don't know what role they are.
- Spy exclusion is impossible at the subject layer (spies have no flag distinguishing them from workers).
- `agents[]` array in session JSON can't render "who's playing what" — operators see a flat list with no semantics.

## Decision

Extend agent registration with two new fields:

1. **`role`** — free-form short token (`isRecommendedToken` rules: `^[a-z0-9_-]+$`, 1-63 chars). Examples: `implementer`, `verifier`, `planner`, `coordinator`, `spy`, `worker`.

2. **`class`** — enum: `active` (default) or `observer`. Spies and other non-tasking watchers MUST set `class=observer`. The class distinction is what `<target>` segments in coordination subjects key off of (`workers.*` matches `class=active`; `spies.*` matches `class=observer`).

Both flow from environment to metadata to wire:

```
$SESH_ROLE  ──▶  Config.Role  ──▶  metadata.role   ──▶  $SRV.INFO.agents responses
$SESH_CLASS ──▶  Config.Class ──▶  metadata.class  ──▶  $SRV.INFO.agents responses
                                                   ──▶  AgentRef.Role / AgentRef.Class  (session JSON)
```

## Spec

### Environment

| Var | Required? | Default | Validation |
|---|---|---|---|
| `SESH_ROLE` | optional | `worker` | `isRecommendedToken` (`^[a-z0-9_-]+$`, 1-63 chars). Reject empty string. |
| `SESH_CLASS` | optional | `active` | One of `active`, `observer`. |

Set by:
- `orch-spawn` — already reads role from `--outfit`/`--cut`/`--role` flags; add a step to export `SESH_ROLE` and `SESH_CLASS` alongside `ORCH_ROLE` for forward compat. Existing `ORCH_ROLE` stays so legacy consumers don't break.
- `sesh up --exec` (issue [sesh#89](https://github.com/danmestas/sesh/issues/89)) — when shipped, must propagate `SESH_ROLE`/`SESH_CLASS` from its own flags into the spawned child's env.
- Operators launching adapters by hand — set in shell before invoking.

### `Config` struct (`internal/refagent/agent.go`)

```go
type Config struct {
    Agent           string
    Owner           string
    Session         string
    Role            string  // new — defaults to "worker"
    Class           string  // new — defaults to "active"
    NATSURL         string
    ProtocolVersion string
}
```

`NewConfigFromEnv` reads `SESH_ROLE` / `SESH_CLASS` with the defaults above, validates, errors on invalid (don't paper over).

### Metadata

`metadata.role` and `metadata.class` are added to the NATS Micro service registration (alongside existing `agent`, `owner`, `protocol_version`, `session`). The fields are **opaque to Synadia's SDK** — the v0.3 protocol allows unknown metadata keys (`§5.6` and `§12`) — so this change does NOT break Synadia compatibility.

### `AgentRef` (`cli/session.go:24-29`)

```go
type AgentRef struct {
    Agent      string `json:"agent"`
    Owner      string `json:"owner"`
    Session    string `json:"session,omitempty"`
    Role       string `json:"role,omitempty"`        // new
    Class      string `json:"class,omitempty"`       // new
    InstanceID string `json:"instance_id"`
    Subject    string `json:"subject"`
}
```

`agent_watcher.go` reads `metadata.role` / `metadata.class` from the `$SRV.INFO.agents` response and populates these. Absent fields are interpreted as `role="worker"`, `class="active"` for back-compat (no breakage when an old adapter registers without setting them).

### `agents[]` in session JSON

Adds the two fields per entry. Tools that already render the array (e.g., `sesh-ops`, dashboards) gain semantic context for free.

```json
{
  "pid": 42629,
  "scope": "session",
  "nats_url": "nats://127.0.0.1:65261",
  "agents": [
    { "agent": "claude-code", "owner": "dmestas", "instance_id": "...", "subject": "agents.prompt.cc.dmestas.foo",
      "role": "implementer", "class": "active" },
    { "agent": "pi", "owner": "dmestas", "instance_id": "...", "subject": "agents.prompt.pi.dmestas.foo",
      "role": "spy", "class": "observer" }
  ]
}
```

### Subject construction (consumer side)

Per the coordination-subjects proposal, role-aware subjects derive from these fields. The helper SDK (open question 2 of that proposal) takes a config struct and builds:

- Publish subject: `sesh.<verb>._local.<scope>.<scope-id>.<target>.<role>`
- Subscribe filter for an `active` worker:
  - `sesh.task._local.project.<project-id>.workers.<role>` (their own role)
  - `sesh.task._local.project.<project-id>.workers.>` (broadcast to all workers)
- Subscribe filter for an `observer`:
  - `sesh.report._local.>`, `sesh.blackboard._local.>` — explicitly NOT `task.*` / `control.*`

### Validation rules

- `role`: `^[a-z0-9_-]+$`, 1-63 chars. Reject `..`, `/`, `\`, NUL, whitespace, leading `$`. Same as `validateLabel` in `cli/label.go`.
- `class`: must be `active` or `observer`. Anything else: error at boot.
- `role` collisions within a session are **allowed** (multi-implementer setups are valid). Disambiguation happens at the queue-group / instance level — see coordination-subjects proposal's queue-group policy.

### Canonical role/class rules (cite this section verbatim)

Adapters in any language MUST implement the rules below identically. This section is the single source of truth — every adapter's local `validateRole`/`validateClass` is a literal port of these rules.

```
role regex     : ^[a-z0-9_-]+$
role length    : 1..63 bytes inclusive
role default   : "worker"
class values   : "active" | "observer" (no others)
class default  : "active"
```

Defaulting rule (both fields): empty string / unset env var → apply default. Any other value: validate; on failure, error at boot (do not silently coerce).

When porting to a new language, copy these rules verbatim into a `// SOURCE: docs/proposals/2026-05-21-agent-role-registration.md` comment block above the validator. Do not paraphrase — drift between adapters is the primary risk this section exists to prevent.

## Migration

Phase 1 (this proposal, sesh):
- Extend `Config`, `AgentRef`, `agent_watcher.go` parsing.
- Default fallbacks for absent fields.
- Update session JSON schema doc.

Phase 2 (agent-channels — see companion ADR):
- Each adapter (`claude-nats-channel`, `pi-nats-channel`, `omp-nats-channel`, `grok-nats-channel`, `gemini-nats-channel`) reads `$SESH_ROLE` / `$SESH_CLASS` at boot and sets the metadata fields in the NATS Micro service registration.

Phase 3 (orch):
- `orch-spawn` exports `SESH_ROLE` and `SESH_CLASS` (in addition to existing `ORCH_ROLE`).

Phase 4 (sesh up --exec, gated on sesh#89):
- Add `--role` / `--class` flags or honor inherited env.

All four phases are independently shippable. Without phase 2, sesh logs a warning that the adapter doesn't expose role/class; coordination subjects still work but every agent registers as `role=worker class=active` by default.

## Acceptance

- [ ] `Config` gains `Role` / `Class` with env-var population and validation.
- [ ] `agent_watcher.go` parses `metadata.role` / `metadata.class` from `$SRV.INFO.agents` with safe defaults.
- [ ] `AgentRef` JSON gains the two fields (`omitempty`).
- [ ] Session manifest schema doc updated to show the new fields.
- [ ] At least one adapter (claude-nats-channel) updated to read the env and set the metadata.
- [ ] Integration test: spawn a session with `SESH_ROLE=implementer SESH_CLASS=active` and assert the values appear in `agents[]` within `~1s` (one agent_watcher poll).
- [ ] Integration test: spawn a second agent with `SESH_CLASS=observer` and assert it appears alongside but with `class=observer`.
- [ ] Backward-compat test: spawn an adapter that does NOT set the env / metadata and assert it appears with `role=worker class=active` defaults, no errors.

## Open Questions

1. **Should `role` be enumerated or free-form?** Free-form is more flexible (operators add new roles without a code change) but loses discoverability (no enum = no autocomplete, typos cause silent mis-routing). Recommend free-form for now; revisit if typos become a real failure mode.

2. **Should `class` grow beyond `active | observer`?** Conceivable additions: `passive` (idle, awake but not subscribed to work), `quarantine` (registered but blocked from being a target). Leave at the two-value enum for v1.

3. **Per-agent multi-role?** An agent that's BOTH `implementer` AND `verifier`? Unlikely useful; reject for v1, single-role per registration.

4. **Role vs `agent` field redundancy.** Is `role=implementer` meaningfully different from `agent=implementer-bot`? Yes: `agent` is the canonical adapter identifier (`claude-code`, `pi`, `omp`), `role` is the function being played in the swarm. One Claude Code instance might be `agent=claude-code role=implementer`; another `agent=claude-code role=reviewer`. Same code, different function.

5. **Should the helper SDK be in sesh or a separate `sesh-coordination-sdk` package?** Likely sesh for v1 (Go), with a TS port to agent-channels for adapter use.
