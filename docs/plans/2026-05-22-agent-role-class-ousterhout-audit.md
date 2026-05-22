# Ousterhout Audit — Agent Role & Class Plans + Proposal

**Date:** 2026-05-22
**Subject:**
- Proposal: `docs/proposals/2026-05-21-agent-role-registration.md`
- Plans: `docs/plans/2026-05-22-agent-role-class-phase{1,2,3}-*.md`

**Method:** *A Philosophy of Software Design* — minimize complexity, deep modules, information hiding, pull complexity downward, define errors out of existence, design it twice.

---

## Goal (one sentence)

Two-field extension to agent registration so coordination subjects can route by function and exclude observers.

---

## Red Flags

### P0 — Change amplification: validation & defaults duplicated across 3+ Go layers and 5+ TS adapter repos

The plans inline `validateRole` / `validateClass` and the `"worker"` / `"active"` defaults at:

1. `internal/refagent/agent.go` (Phase 1 Task 1) — constants + validators + env reads
2. `cli/agent_watcher.go` (Phase 1 Task 4) — *hardcoded copies* of `"worker"` / `"active"` defaults
3. `claude-nats-channel/src/config.ts` (Phase 2 Task 1) — TS port of validators + defaults
4. `pi-nats-channel`, `omp-nats-channel`, `gemini-nats-channel`, `grok-nats-channel` — copies of #3

Phase 1 plan itself flags the bug: *"The defaults are hardcoded here (rather than imported from internal/refagent) because cli should not depend on internal/refagent. Keep them in sync."*

> *"Keep them in sync"* is an Ousterhout red flag. One semantic change (e.g., role length 63→127) requires edits in 6+ files. **Change amplification + obscure dependencies.**

**Fix (Phase 1):** Introduce `internal/agentmeta/` (new Go package) containing:

```go
package agentmeta

type AgentClass string

const (
    ClassActive   AgentClass = "active"
    ClassObserver AgentClass = "observer"

    DefaultRole  = "worker"
    DefaultClass = ClassActive
)

var roleTokenRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

func ValidateRole(role string) error  { /* ... */ }
func ValidateClass(c AgentClass) error { /* ... */ }
func DefaultedRole(s string) string    { if s == "" { return DefaultRole }; return s }
func DefaultedClass(s string) AgentClass { /* ... */ }
```

Both `internal/refagent` (emit side) and `cli/agent_watcher` (parse side) import it. One file, one source of truth. Adds zero interface complexity (`agentmeta.ValidateRole(...)` is no harder to call than a local `validateRole(...)`) and removes a class of bug (defaults drifting).

**Fix (Phase 2):** Skip a shared TS module for v1 (no monorepo today) but add the rules verbatim *in the proposal itself* as a versioned "Canonical role/class rules" section. Each adapter cites the proposal in a comment above its local validator. When the SDK proposal (open Q5) lands, all adapters drop their inline copies.

---

### P0 — Weak type safety on `Class` for no benefit

`Class` is modeled as `string` everywhere (Go) and `string` (TS). Only ever two legal values.

```go
// Today (per plan):
type Config struct {
    Class string  // any string compiles
}
cfg.Class = "passsive"  // typo compiles, fails at runtime
```

**Fix:** Use a defined type with constants (Go) and a literal-union (TS).

```go
// Go
type AgentClass string

const (
    ClassActive   AgentClass = "active"
    ClassObserver AgentClass = "observer"
)
```

```ts
// TS
export type AgentClass = "active" | "observer";
```

Deeper interface (consumer cannot misuse the field) at zero added cost. *Define errors out of existence.*

Applies to: Phase 1 Task 1 (`Config.Class`), Phase 1 Task 3 (`AgentRef.Class` — needs to remain `string` for JSON serialization compat, but the *internal* fields used in `agent_watcher` can be the typed form, converted only at the JSON boundary).

---

### P1 — Phase 1 Task 5 contains a soft placeholder

Plan text: *"`newTestSession(t, dir, "e2e-label", url)` — helper that constructs `*Session` for tests — see existing test file for the pattern"* and *"adjust to match Session's actual path"*.

The writing-plans skill explicitly forbids this pattern: *"References to types, functions, or methods not defined in any task."*

**Fix (one of):**

- (Preferred) Inline the actual session-construction code Task 5 needs. If the existing `cli/session_agents_test.go` already has a `t.TempDir()` + `Session{...}` literal pattern (line 252-260 visible in the scout report), copy that verbatim into Task 5.
- (Alternative) Merge Task 5 into Task 4 — Task 4 already proves the watcher populates AgentRef correctly; adding a `sess.UpdateAgents(agents)` step + a `os.ReadFile` of the session JSON path covers the same surface in 5 fewer lines.

The second option is the more Ousterhout-aligned answer: **the existing Task 4 already covers the load-bearing assertion**. Task 5 adds reading effort to the plan without adding meaningful confidence.

---

### P1 — Phase 3 Task 1 is "reconnaissance"

Task 1 is pure investigation: *"Locate the existing ORCH_ROLE write site … identify how `role` is derived"*. No code, no test, no commit.

Including a reconnaissance step means **the plan author didn't do the research**. Per the writing-plans skill: *"document everything they need to know: which files to touch for each task, code, testing"*.

**Fix:** Either

- (Preferred) Dispatch an Explore subagent at `~/projects/orch/` *before* this plan is handed off, and rewrite Task 1 with concrete `cmd/orch-spawn/main.go:NN` references.
- (Alternative) Mark Phase 3 explicitly as a *scaffolding* plan: "Phase 3 Task 1 is a manual scouting step that must be replaced with concrete file references before Tasks 2-4 can execute as written." This is honest and Ousterhout-aligned (no hidden incompleteness).

---

### P1 — Outfit→class mapping is hardcoded magic string

Phase 3 Task 4 hardcodes `outfit == "spy"` as the trigger for `class=observer`:

```go
if !classFlagExplicit && (spec.Outfit == "spy") {
    spec.Class = "observer"
}
```

**Obscure dependency:** the operator running `orch-spawn --outfit spy` won't know `SESH_CLASS` was implicitly set unless they read the docs *and* remember. A second observer-class outfit (`auditor`, `watcher`) requires editing this `if` statement.

The plan acknowledges this (*"start with just `spy`, expand as needed"*) but ships the tactical form first. Pure tactical programming.

**Fix:** Outfit definitions should carry their class explicitly. In whatever data structure orch-spawn uses to define outfits:

```yaml
# outfits/spy.yaml (or whatever the format is)
name: spy
default_class: observer
default_role: spy
```

Code becomes:

```go
outfit := loadOutfit(spec.OutfitName)
if !classFlagExplicit {
    spec.Class = outfit.DefaultClass
}
```

Now adding an `auditor` observer-class outfit is a config-file change, not a code change. *Pull complexity downward* into the outfit-definition layer.

If today's orch-spawn outfit registry doesn't carry per-outfit class metadata, that's a Phase-3 scope question — call it out explicitly: "Phase 3 acceptance includes adding `default_class` to the outfit schema, or accepting the hardcoded `outfit == 'spy'` form as a known shortcut."

---

### P2 — Free-form `role` pushes complexity to every consumer

Proposal Open Question 1: "*Should `role` be enumerated or free-form?*" — recommends free-form. Plans inherit that choice.

Ousterhout cost: every consumer (operators, dashboards, queue-group routers, subject builders) must independently know which roles exist. Typos (`implementor` vs `implementer`) silently mis-route. *Cognitive load* increases linearly with the number of consumers.

The proposal's rationale: *"loses discoverability (no enum = no autocomplete, typos cause silent mis-routing). Recommend free-form for now; revisit if typos become a real failure mode."*

**Audit position:** the recommendation is defensible — flexibility-vs-discoverability is a real tradeoff — but the cost is not zero. Two cheap mitigations the plans could add:

1. Ship a `KnownRoles` slice in `internal/agentmeta/` containing the canonical set the team currently uses (`worker, implementer, verifier, planner, coordinator, spy`). Adapters and dashboards can iterate it for autocomplete without rejecting unknown roles.
2. The coordination-subject SDK (when built) should *warn* on subjects referencing unknown roles, not reject. Same direction as #1.

Neither is required for Phase 1 to ship. Note as future-work.

---

### P2 — Acceptance criteria duplication (Phase 1 Task 5 ≈ Task 4)

Task 4 (`TestAgentWatcher_PopulatesRoleAndClassFromMetadata`) and Task 5 (`TestSessionJSON_PopulatesRoleAndClassFromRefAgent`) both:

- Start an in-process NATS server
- Register a synthetic agent with `metadata.role` / `metadata.class`
- Assert the watcher picks up the fields

Task 5's only delta is `sess.UpdateAgents(...)` + `os.ReadFile(...)`. If Task 4 passes, Task 5 passes by construction.

**Fix:** Either delete Task 5 (Task 4 + the existing `session_agents_test.go:312` UpdateAgents round-trip already covers the JSON write path) or rewrite Task 5 to cover something Task 4 doesn't (e.g., test that the session file is atomically rewritten when role/class *change* — that's a distinct code path in `UpdateAgents`).

---

### P2 — Two-place defaulting policy creates a subtle observable equivalence

When a vanilla (non-sesh) Synadia agent registers without `metadata.role`/`class`:
- It appears in session JSON as `role=worker class=active` (watcher default).

When an operator sets `SESH_ROLE=worker` (the default) on a sesh adapter:
- It appears identically as `role=worker class=active`.

Observable equivalence between *"agent did not opt in to role/class"* and *"operator explicitly chose worker"*. Probably fine in practice but worth documenting: dashboards can't distinguish the two.

**Fix (lightweight):** None required. Just document this in the proposal's "Migration" section — the back-compat default is *also* the most common explicit value, and that's by design.

---

### P2 — Phase 2 integration test hits a real `nats-server`

Plan: *"Assumes nats-server is available at $NATS_URL or nats://localhost:4222"*.

Phase 1's Go tests use an in-process `nats-server` (`startTestNATSServer`). Phase 2's TS tests don't — they assume an external server is running. CI will skip/fail unpredictably.

**Fix:** Use `@nats-io/nats-server` (or spawn the server binary as a child process at test setup) to mirror Phase 1's in-process approach. Or, accept the dependency and gate the integration test on an env var (`NATS_TEST_URL`).

---

### Strategic vs Tactical

The proposal itself is *strategic* — it carries Why/Migration/Open-Questions sections, considers Synadia compatibility, defers Phase 4 honestly. Good.

The Phase 1 plan is strategic in shape (TDD, real code blocks) but inherits the **structural weakness of inlining validation/defaults across packages** (P0 above). The Phase 3 plan is *tactical* in two places (reconnaissance Task 1, hardcoded `outfit == "spy"`).

Overall: the proposal sets a reasonable course; the plans need ~2 hours of patching (centralize Go validators + defaults; replace the soft placeholder; either scout or annotate Phase 3 Task 1) before they're ready to execute end-to-end.

---

## Design It Twice — Centralizing the Go side

### Alternative A (what the plans implement)

`Config` carries `Role` / `Class` as plain `string`. `internal/refagent/agent.go` defines validators + defaults. `cli/agent_watcher.go` hardcodes default strings to avoid an `internal/refagent` import.

- **Pros:** mechanical, matches the proposal verbatim. Two files touched.
- **Cons:** validators + defaults duplicated; type system can't catch class typos; new consumer (e.g., a future `cli/agents.go` lister) has to either re-duplicate defaults or import `internal/refagent` (which would pull in the whole refagent runtime — heavy).

### Alternative B (recommended)

New package `internal/agentmeta/`:

```
internal/agentmeta/
├── agentmeta.go         # AgentClass type + constants + DefaultRole/DefaultClass
├── validate.go          # ValidateRole, ValidateClass, Defaulted{Role,Class}
└── agentmeta_test.go    # the table-driven tests Phase 1 Task 1 defines
```

`internal/refagent` imports `agentmeta` to populate `Config`. `cli/agent_watcher` imports `agentmeta` to default + validate parsed metadata. No `cli ↔ refagent` dependency edge.

- **Pros:** one validator, one default. Future consumers add one import. Type system catches `Class` typos. The `internal/agentmeta` package is *deep* — small interface (`ValidateRole`, `DefaultedClass`, ~4 symbols total) hiding all the rules.
- **Cons:** one extra package (~80 LoC across 3 files). Phase 1 Task 1 grows by one file.

**Pick B.** The cost is one ~80-LoC package. The benefit is eliminating a known-bug-shape (drift across files) before it ever lands.

---

## Concrete Patches to Apply Before Execution

| # | Plan file | Section | Patch |
|---|-----------|---------|-------|
| 1 | phase1-sesh.md | New "Task 0: Centralize role/class types in `internal/agentmeta/`" | Move `validateRole`/`validateClass`/defaults/`AgentClass` type into a new package. Tasks 1, 4 import from it. |
| 2 | phase1-sesh.md | Task 1 | `Config.Class` becomes `agentmeta.AgentClass` (typed). Validators called via `agentmeta.ValidateRole(...)`. |
| 3 | phase1-sesh.md | Task 4 | `agent_watcher.go` calls `agentmeta.DefaultedRole(info.Metadata["role"])` instead of hardcoded `"worker"`. |
| 4 | phase1-sesh.md | Task 5 | Either delete (Task 4 covers it) or replace with a *distinct* test (e.g., "session JSON atomic rewrite on role/class change"). |
| 5 | phase2-adapters.md | Task 1 | Add a `// SOURCE: docs/proposals/2026-05-21-agent-role-registration.md` comment above the validators citing the proposal as canonical. |
| 6 | phase2-adapters.md | Task 2 | Replace the "assumes nats-server available" dependency with an in-process server (`@nats-io/nats-server` or child-process spawn). |
| 7 | phase3-orch-spawn.md | Task 1 | Either scout orch-spawn now and replace with concrete file:line refs, or label the task as "scouting required before Tasks 2-4 execute". |
| 8 | phase3-orch-spawn.md | Task 4 | Reframe as "outfit registry carries `default_class`" rather than `outfit == "spy"` hardcode. If outfit schema doesn't allow that today, add a sub-task to extend the schema. |
| 9 | proposal | New "Canonical role/class rules" subsection | Add the validation rules verbatim so TS adapters can cite a single source. |

---

## Net Recommendation

- **Proposal:** ship as-is plus the Canonical-rules subsection (Patch #9). The decisions are sound; documenting the rules in one place removes downstream confusion.
- **Phase 1 plan:** apply Patches #1-4 before execution. Adds one package, removes a class of drift bug, tightens types. ~30 min of planning effort.
- **Phase 2 plan:** apply Patches #5-6. Minor.
- **Phase 3 plan:** apply Patches #7-8. The bigger of the two is #7 — Phase 3 isn't ready to dispatch without an orch-spawn scout pass.

**Open question worth surfacing:** the proposal ships role/class to support a coordination-subject hierarchy (`docs/proposals/2026-05-20-sesh-parallel-coordination-subjects.md`) that itself is unbuilt. Phases 1-3 will ship a *capability* (the fields land in metadata + session JSON) with no current *consumer*. That's defensible — the fields are cheap and the future consumer is committed — but the operator should be aware: the *value* of this work is realized only when the coordination SDK lands.
