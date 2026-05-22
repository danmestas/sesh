> **[SUPERSEDED 2026-05-22]** — The implementation diverged from this proposal during design review.
>
> What shipped (see `docs/synadia-agents-on-sesh.md` § 8.1 for the canonical contract):
>
> - **Wire shape:** `agents.<verb>.<machine>.<project>.<session>[.<role>[.<worker_id>]]` — the sesh-owned `sesh.*` namespace was dropped in favor of layering on Synadia's `agents.*` namespace with the verb at position 2 distinguishing intent. Token count selects tier (5 = orch front door, 6 = role pool with queue group, 7 = direct address by instance_id).
> - **No `<scope>` or `<scope-id>` segments** — only project/session, which is what `scope=project` would have collapsed to in practice. Workflow/session/agent scopes are deferred until a real consumer needs them.
> - **No `<target>` segment** — `workers`/`spies` redundancy with class metadata was eliminated; spy exclusion is **verb-based** (observers subscribe to `agents.report.*` only, never `agents.prompt.*`).
> - **No `sesh.*` SDK package** — the 600 LoC of typed `Subject`/`Filter`/`Verb`/`Scope` ceremony was replaced with `fmt.Sprintf` at the ~3 call sites in `internal/refagent/coordinate.go`. NATS native subject-count matching does all the tier routing.
> - **Heartbeat extension:** role/class added to the §8.3 heartbeat payload so coordinators build `{instance_id → role, class}` from passive heartbeat observation rather than `$SRV.INFO.agents` polling.
>
> The original 7-token shape `sesh.<verb>.<machine>.<scope>.<scope-id>.<target>.<role>` below is preserved for design history but is NOT the implemented contract.

# Sesh Parallel Coordination Subjects (Scope 2)

**Date:** 2026-05-20 (amended 2026-05-21)
**Status:** Proposed
**Author:** Grok (with input from sesh coordination patterns and scoped-memory work)
**Related:**
- `docs/scoped-memory.md` (five-scope model)
- `docs/coordination-patterns.md`
- `docs/swarm-workflow.md`
- `docs/specs/2026-05-19-grok-synadia-nats-channel-design.md`
- `docs/proposals/2026-05-21-agent-role-registration.md` (companion — adds `role`/`class` to agent metadata so coordination subjects have something to address)
- `.agents/skills/nats-design-subject/`

## Amendments (2026-05-21)

1. **Machine slot added to the subject** — reserve a `<machine>` token in the second position so federation / multi-host routing doesn't require a wire-shape change later. Default `_local` for single-host setups.
2. **`project-id` introduced as distinct from `project-code`** — `project-code` (`.sesh/project-code`, SHA1 of `hostname + projectName` per `cli/paths.go:208`) is hostname-salted on purpose for fossil-sync isolation. It is therefore unusable as a routing key — "the same project" on two hosts gets two project-codes. Introduce a separate `project-id` (hostname-free) that flows into the subject's `<scope-id>` for `scope=project`.
3. **Role / class registration is split out** to its own proposal (`2026-05-21-agent-role-registration.md`) — the wire shape here depends on agents carrying `role` and `class` in metadata, which is a separable change.
4. **Queue-group policy stated per-verb** — broadcast-style verbs (`broadcast`, `announce`, `report`, `blackboard`) MUST NOT use a queue group (fan-out). Work-stealing verbs (`task`) MAY use a queue group named after the role for load-balancing within that role.
5. **Open question 1 (verb vocabulary lock)** — proposed verbs are now committed; see updated list below. Still open: whether to add `query` as a peer of `task`.

## Problem

Sesh has a rich, explicit scoping model (Hub / **Project** / Session / Workflow / Agent) and a growing set of multi-agent coordination patterns (generator–verifier, orchestrator–subagent, agent teams, blackboard, etc.).

When multiple `sesh` sessions run under the same project (`sesh up --scope=project`), operators and agents need flexible communication:

- Broadcast or fan-out to **multiple workers** in a project or swarm.
- Exclude **spies** (monitoring / auditor agents) from tasking traffic.
- Target a **specific sesh**, swarm, or function/role inside a sesh.
- Support workflow-scoped coordination that crosses sessions but is narrower than the whole project.

The Synadia Agent Protocol for NATS v0.3 (the `agents.*` subjects used by the pi, claude-code, and upcoming grok channels) is deliberately rigid:

- Fixed 5-token shape: `agents.<verb>.<agent-abbrev>.<owner>.<session>`
- `AgentSubject` helper and all channel implementations assume this shape.
- Discovery, collision detection, heartbeats, and status endpoints are built on top of it.

This shape is excellent for **direct, addressable, streaming request/reply** to one specific agent instance. It is a poor fit for project-level broadcast, role-based targeting, and spy exclusion.

Attempting to cram `.project.swarm.function` into the 5th token loses real NATS wildcard power and fights the shared protocol.

## Decision

**Use two parallel, coexisting subject spaces:**

1. **Synadia Agent Protocol subjects** (`agents.*`)  
   - Strictly for direct, 1:1 (or small-group) prompt/response with a specific running agent instance.  
   - Remains 100% compatible with the official v0.3 protocol, the `@synadia-ai/agents` SDK, heartbeats, `$SRV.INFO.agents` discovery, and all first-class channel adapters (pi, claude-code, grok, etc.).

2. **Sesh Coordination Subjects** (`sesh.*`)  
   - A sesh-owned, fully controllable subject hierarchy designed for the five scopes, project swarms, role-based routing, and the coordination patterns in `coordination-patterns.md`.  
   - This is where broadcast, selective fan-out, spy exclusion, and swarm orchestration live.

The two spaces are complementary, not in competition. Most real workflows will use both.

## Proposed Subject Hierarchy

```
sesh.<verb>.<machine>.<scope>.<scope-id>.<target>.<role-or-capability>
```

7 tokens. The machine slot is the second position (after the verb) so federation / leaf-gateway routing can prefix-match on a single slot.

### Segment Definitions

| Segment               | Values / Examples                          | Notes |
|-----------------------|--------------------------------------------|-------|
| `sesh`                | fixed prefix                               | Avoids collision with other systems on the hub |
| `<verb>`              | `task`, `broadcast`, `control`, `announce`, `blackboard`, `report` | Intent. `query` is a candidate addition (still open). |
| `<machine>`           | `_local`, `dmestas-mbp`, `gpu-rig-7`, `machine-id` (SHA of `/etc/machine-id` first 8 hex) | Default `_local` for single-host. Multi-host setups populate a stable host identity. |
| `<scope>`             | `hub`, `project`, `session`, `workflow`, `agent` | Matches `scoped-memory.md` |
| `<scope-id>`          | For `scope=project`: `<project-id>` (NOT `project-code`). For `scope=session`: session label. For `scope=workflow`: trace-id (first 8 hex). For `scope=agent`: agent instance id. | See "Project identifier vs project-code" below. |
| `<target>`            | `workers`, `spies`, `swarm-alpha`, `reviewers`, `all` | Optional grouping inside the scope. |
| `<role-or-capability>`| `worker`, `spy`, `coordinator`, `implementer`, `verifier`, `analyze`, `review` | Sourced from `metadata.role` on agent registration (see companion proposal). |

### Machine slot — why and how

The single-host single-hub world doesn't need it, but mesh-over-internet does. Adding the token after the wire shape ships means rewriting every subscription. Reserving the slot now costs 6 bytes per subject and zero behavior change.

Sentinel default: `_local`. Mesh-aware deployments populate it with a host identity stable across reboots:
- macOS: `IOPlatformUUID` first 8 hex
- Linux: `/etc/machine-id` first 8 hex
- Convenience override: `$SESH_MACHINE` env var

Wildcard patterns:
- All sessions on a host: `sesh.>.dmestas-mbp.>` (technically `sesh.*.dmestas-mbp.>`)
- A specific role across all hosts: `sesh.task.*.project.<id>.workers.implementer`
- Anything on the local host: `sesh.*._local.>`

### Project identifier vs project-code (`<scope-id>` for `scope=project`)

`project-code` (`cli/paths.go:208`) is `SHA1("sesh:" + hostname + ":" + projectName)`. The hostname salt is deliberate — fossil cross-leaf sync uses it so two hosts working on "the same project" don't collide on the hub's fossil store (`cli/paths.go:191-196`). **This makes project-code unusable as a routing key** for `scope=project` traffic, because "talk to the planners of project foo, anywhere" requires a single shared identifier.

Introduce `project-id`:
- Derivation: `SHA1("sesh:project:" + projectName)` — hostname-free.
- Storage: alongside `project-code` in `.sesh/project-id` (8-char hex form printed; full 40 stored).
- Use: subject's `<scope-id>` for `scope=project`. Also the right key for cross-host blackboards.
- `project-code` stays as-is and continues to drive fossil-sync isolation. The two are not interchangeable.

For readability in tooling, the human `projectName` (cwd basename) is carried in agent `metadata.project_name` — never in the subject. Git's plumbing-vs-porcelain split.

### Queue-group policy (per verb)

NATS Micro registers endpoints under a queue group by default — load-balancing across subscribers, not fan-out. For coordination subjects the policy is verb-specific:

| Verb | Queue group | Semantics |
|---|---|---|
| `task` | role-name (e.g. `implementer`) | Work-stealing among same-role instances |
| `broadcast` | none | Every subscriber receives |
| `announce` | none | Pub/sub, every listener notified |
| `report` | none | Multiple observers may consume independently |
| `blackboard` | none | All watchers see updates |
| `control` | role-name | Single instance acts per command |
| `query` (if adopted) | role-name | Request/reply with one responder |

Adapters subscribing to `sesh.*` MUST honor this. Helper SDK (open question 2) should enforce.

### Concrete Examples (single-host, `<machine>=_local`)

**Project-level tasking (exclude spies automatically):**
- `sesh.task._local.project.a3f2c1d8.workers.implementer`
- `sesh.task._local.project.a3f2c1d8.workers.verifier`

**Specific swarm inside a project:**
- `sesh.task._local.project.a3f2c1d8.swarm-alpha.worker`

**Workflow-scoped (cross-session but narrower than project):**
- `sesh.task._local.workflow.a1b2c3d4.coordinator.dispatch`

**Blackboard / shared findings (from coordination patterns):**
- `sesh.blackboard._local.project.a3f2c1d8.update.research`
- `sesh.blackboard._local.workflow.a1b2c3d4.findings`

**Spy / monitoring traffic (read-only):**
- `sesh.report._local.project.a3f2c1d8.spies.all`

**Session-private orchestration:**
- `sesh.control._local.session.myapp-alpha.local-orchestrator`

**Agent-level (very narrow, process lifetime):**
- `sesh.report._local.agent.myapp-alpha.claude-123.status`

### Multi-host examples

**Same role, all hosts:**
- `sesh.task.*.project.a3f2c1d8.workers.implementer` — any host serves a planner of this project

**One host, all sessions:**
- `sesh.*.gpu-rig-7.>` — everything on `gpu-rig-7`

## Wildcard & Subscriber Patterns

Because the hierarchy is real dots, subscribers get efficient, natural filtering:

- All project tasking (any host): `sesh.task.*.project.a3f2c1d8.>`
- All project tasking (this host only): `sesh.task._local.project.a3f2c1d8.>`
- Only worker traffic in the project: `sesh.task.*.project.a3f2c1d8.workers.>`
- Everything a particular swarm should see: `sesh.task.*.project.a3f2c1d8.swarm-alpha.>`
- All blackboard updates for a project: `sesh.blackboard.*.project.a3f2c1d8.>`

Spies simply never subscribe to the `workers.*` or `task.*` lanes — they only listen on `report.*` or `blackboard.*` subjects.

## Relationship to the Synadia Agent Protocol

| Concern                        | Synadia Agent Protocol (`agents.*`)          | Sesh Coordination Subjects (`sesh.*`)          |
|--------------------------------|----------------------------------------------|------------------------------------------------|
| Direct prompt of one sesh      | Primary (streaming `response` chunks + terminator) | Not used                                       |
| Heartbeats & liveness          | Primary (`agents.hb.*`, `agents.status.*`)   | Optional health reports on `sesh.report.*`     |
| Discovery of running agents    | `$SRV.INFO.agents` + heartbeats              | Secondary (used for role/scope metadata)       |
| Project / swarm broadcast      | Poor fit                                     | Primary                                          |
| Role-based targeting (spy exclusion) | Requires client-side filtering            | Natural via subject structure                    |
| Cross-session workflow traffic | Possible but flat                            | First-class (workflow scope)                     |
| 1:1 request/reply semantics    | Strong (per-request reply subjects)          | Can be layered on top if needed                  |

**Recommended usage rule:**

- If you need to **talk to one specific running agent instance** and want the full streaming + ack + query chunk experience → use `agents.prompt.<agent>.<owner>.<session>`.
- If you need **broadcast, role filtering, project scope, or swarm coordination** → use `sesh.*` subjects.
- Orchestrators commonly do both: discover via `agents.*`, then dispatch either directly or via coordination subjects.

## Integration with Existing Sesh Primitives

- **JetStream KV** (scoped-memory buckets): Agents announce presence or findings on `sesh.blackboard.*` subjects; watchers update the corresponding `sesh_project_*` / `sesh_workflow_*` KV buckets.
- **Fossil** (especially `--scope=project`): Use `sesh.announce.project.<name>.fossil-commit` or similar to notify peers that new content is available at the shared trunk.
- **Message envelope** (traceparent): Include the W3C `traceparent` header on all `sesh.*` messages so workflow-scoped traffic can be correlated.
- **EdgeSync / Fossil autosync**: Can publish lightweight notifications on coordination subjects when commits land.

## Mapping to Coordination Patterns

| Pattern                  | Primary Subjects Used                          | Notes |
|--------------------------|------------------------------------------------|-------|
| Generator–verifier       | `sesh.task.project.<name>.workers.*` + direct `agents.prompt.*` for final handoff | Verifier subscribes only to `verifier` lane |
| Orchestrator–subagent    | `sesh.task.project.<name>.workers.<role>`      | Orchestrator publishes tasks; workers reply on `agents.*` or `sesh.report.*` |
| Agent teams              | `sesh.task.project.<name>.swarm-<x>.>`         | Each swarm has its own target segment |
| Message bus              | `sesh.announce.project.<name>.>` + `sesh.blackboard.*` | Growing ecosystem of listeners |
| Blackboard / shared state| `sesh.blackboard.project.<name>.update.<topic>` | Complements KV/Fossil |
| Hierarchical multi-tier  | Mix of `project` + `workflow` + `session` scopes | Different tiers use different scope segments |

## Security & Authorization Considerations

- On a shared hub, use NATS accounts or per-role nkeys to enforce that only authorized identities can publish to `sesh.task.project.*` or `sesh.control.*`.
- Spies can be given read-only permissions on `report.*` and `blackboard.*` subjects.
- The `sesh.*` space is a natural boundary for subject-based authorization that is independent of the `agents.*` protocol.

## Migration & Coexistence

- Existing single-session usage continues unchanged on `agents.*`.
- New multi-sesh / project-swarm usage adopts `sesh.*` subjects immediately.
- The nats-channel adapters (pi, claude, grok) require **no changes** for the coordination space — they only need to know which extra subjects a particular sesh should subscribe to based on its role and `--scope`.
- Over time, sesh control-plane components (agent_watcher, recipes, `orch`, etc.) can be updated to prefer or require the coordination subjects for swarm-aware behavior.

## Open Questions / Future Work

1. ~~Exact verb vocabulary~~ — **resolved (2026-05-21 amendment)**: `task`, `broadcast`, `control`, `announce`, `blackboard`, `report` are committed. `query` remains a candidate.
2. Sesh-owned SDK / helper library for `sesh.*` (analogous to `AgentSubject`) — **must land** alongside the role-registration proposal so adapters have one canonical way to construct subjects and enforce queue-group policy.
3. How much coordination traffic should move to JetStream streams vs core NATS pub/sub? Especially: `blackboard` and `announce` are natural KV-watchable; `task` is natural for work-queue streams.
4. "Sesh coordination profile" document mapping the six patterns to recommended subject + KV + JetStream combinations — yes, after the SDK lands.
5. **(new)** Machine identity derivation — `_local` default plus host-stable identity (`/etc/machine-id` short, IOPlatformUUID short, or `$SESH_MACHINE` override). Lock the canonical algorithm before federation pilot.
6. **(new)** `project-id` storage and discoverability — does it go in `.sesh/project-id` as a sibling of `.sesh/project-code`, or as a second field in a single JSON? Either way, it MUST be writable from `sesh up` on first init and read by the channel adapters at boot.
7. **(new)** `<target>` segment vs `<role>` segment overlap — when `target=workers` and `role=implementer`, the subject is `...workers.implementer`. When `target=swarm-alpha` and `role=worker`, the subject is `...swarm-alpha.worker`. Is `target` always one tier above `role` in the subject? Yes — codify that.

## Next Steps

1. Review and iterate on this proposal with the sesh core contributors.
2. Pick the initial verb set and document subscriber matrices for the six patterns.
3. Prototype subscription helpers in the sesh Go codebase and one reference agent (e.g., the grok or pi nats channel).
4. Update `coordination-patterns.md` and `swarm-workflow.md` with concrete wire examples using the new subjects.
5. Decide whether any of this should be proposed upstream to the Synadia Agent Protocol (likely not — this is sesh-specific coordination on top of the protocol).

---

This design gives sesh the rich, wildcard-friendly, scope-aware communication surface it needs for real multi-sesh work while preserving the cleanliness and interoperability of the Synadia Agent Protocol for direct agent prompting.