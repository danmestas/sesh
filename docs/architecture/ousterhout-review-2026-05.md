# Ousterhout review — refactor campaign (sesh PRs #32, #33, #35, #36, #37, #38)

Date: 2026-05-14
Reviewer: applying *A Philosophy of Software Design* (Ousterhout, 2018)
Scope: 6 PRs landed on `main` at `b4307ce`. New modules: `bootstrap`, `hubguard`, `hubinfo`, `session`. Rewritten: `up.go`, `down.go`. Touched: `hub_serve.go`, `state.go`, `scope.go`.

---

## TL;DR

The four new modules (`hubguard`, `hubinfo`, `session`, and `bootstrap`'s `MakePlan`) are **deep** — small interfaces hiding non-trivial machinery — and ship clean atomicity/lifecycle stories. The campaign earned its keep on those modules.

`up.go`'s `Run()` is the regression. The orchestration that used to live in one tangled function is now five named modules sequenced inside one tangled function. Run's depth went *down* during the campaign — every previously-tangled step is now a visible call at the same level of abstraction. The next refactor is to pull a `Starter` over Run.

A handful of smaller smells are listed below; only one (`Execute`'s two-phase split) is structural. The rest are local fixes.

---

## Depth scorecard

| Module | Interface | Impl depth | Verdict |
|---|---|---|---|
| `bootstrap.MakePlan` | pure, `(World) -> Plan` | substantive decision tree | **deep** |
| `bootstrap.Execute` | `(Plan, Deps)` but split into two phases at the call site | two phases | **shallow at call site** (see RF-2) |
| `hubguard.{AcquireOrReuse,RegisterDaemon,Lease}` | 3 entry points + opaque Lease | flock + O_EXCL state machine | **deep** |
| `hubinfo.{Write,Read,Clear,ProjectCode}` | 4 functions, one struct | atomic file I/O + libfossil SQL | **deep** |
| `session.{ClaimSession,ReadSession,Terminate,(*Session).Publish,Release}` | 5 entry points + opaque Session | O_EXCL + stale-PID reap + SIGINT-wait | **deep** |
| `down.DownCmd.Run` | thin wrapper over `Terminate` | trivial | **appropriately shallow** (depth pulled down into session) |
| `up.UpCmd.Run` | one method, ~170 lines | sequential pipeline of 5+ phases | **shallow** (see RF-1) |
| `hub_serve.HubServeCmd.Run` | one method | Ready-gate + URL publish + serve + auto-shutdown loop | medium |
| `state.go` | grab-bag of unexported path helpers + project-code pinning | small | OK; helpers are correctly hidden |
| `scope.go` | 2 path-building functions | trivial | **appropriately shallow**; deepening it would pull policy from sesh-ops |

---

## Red flags, ranked

### RF-1 — `up.go:Run()` is the new tangle (highest-leverage fix)

The bootstrap → hub-acquire → execute-post-hub → session-publish → serve sequence is visible in full at `up.go:Run`. Every step's error handling is at the same level of abstraction as the orchestration. The pipeline has no name.

This is the **shallow module / change amplification** smell at the function level: the function does too many concrete things at the same level. Adding a new pipeline step (e.g., goal-management bootstrap) requires editing Run rather than registering a new step.

**Principle violated:** *minimize cognitive load* + *pull complexity downward*.

**Fix:** extract a `Starter` (or `Session.Start`) that owns the sequence:

```go
func (c *UpCmd) Run() error {
    s, err := NewStarter(c)
    if err != nil { return err }
    return s.Start(ctx)
}
```

Each phase becomes a private method on `Starter`. The reader of `UpCmd.Run` learns one verb: "start a session." The reader who needs to modify a phase reads `Starter`.

Alternative considered: leave it. Run is the operator-visible entry point and "what `sesh up` does" arguably belongs visible. Rejected: the visibility is too coarse — the reader sees *every* atomic detail, including ones (flock acquisition, URL publication ordering) that belong inside `hubguard`/`hubinfo`.

### RF-2 — `bootstrap.Execute` runs in two phases that can't be understood independently

`Execute` is called twice: once before `hub.NewHub` to adopt the hub's project-code (`SourceHub`), once after the hub is up to seed from worktree (`SourceGitWorktree`). The order constraint lives in comments and `up.go`'s call sequence, not in types.

This is a **conjoined methods** smell — the two phases share an invariant (hub must exist between them OR plan source dictates which phase runs) that's enforced by reading the code, not the interface.

**Principle violated:** *information hiding* — `up.go` knows the order; `Execute` itself can't enforce it.

**Fix:** collapse to one ordered call that takes the hub as a parameter:

```go
func Apply(ctx context.Context, plan Plan, hub *hub.Hub, deps Deps) error
```

The plan's `Source` field tells `Apply` which path to run; the hub presence is enforced by the signature. Up.go's Run shrinks.

### RF-3 — `ProbeHub` swallows errors as "no hub"

`ProbeHub` returns `("", "", nil)` on any failure: missing `hub.repo`, missing URL files, libfossil read error, malformed config. Caller (`up.go:111-114`) logs "falling back to seed-from-cwd" without distinguishing absent-hub from probe-failure.

This is the *wrong direction* on **define errors out of existence**. Errors should be removed by making them impossible, not by hiding them in degradation paths. A genuine probe failure (e.g., corrupt `hub.repo`) silently degrades to a fresh seed — potentially overwriting state the operator wanted preserved.

**Fix:** sum-type return:

```go
type HubProbe struct {
    Present     bool   // true iff hub.repo exists AND probe succeeded
    FossilURL   string
    ProjectCode string
}
func ProbeHub() (HubProbe, error) // error reserved for real failures
```

Caller distinguishes `Present=false, err=nil` (fresh) from `err != nil` (unexpected). The current behavior of degrading on real errors becomes opt-in by the caller, not the default.

### RF-4 — Four names for one concept

`ProjectCode` (hubinfo), `projectCode` (up.go local), `SESH_PROJECT_CODE` (hub_serve env var), `deriveProjectCode` (state.go). Reading the campaign requires cross-referencing four spellings.

**Principle violated:** *minimize cognitive load*.

**Fix:** rename for consistency. The env var stays (`SESH_PROJECT_CODE` is operator-facing), but internal callers/locals should align on `projectCode` / `ProjectCode`. `deriveProjectCode` is fine — it's a verb-noun. Run a grep + audit.

### RF-5 — Path-building scattered

`scope.repoPathFor` and `scope.storeDirFor` exist, but `session.sessionFilePath`, `state.projectCodePath`, `state.hubRepoPath`, etc. each build their own paths inline. Adding a new scope mode or moving a state file requires editing N files.

**Smell:** mild **change amplification**. Not blocking — most paths are stable — but worth a consolidated `paths` package or unexported map.

**Fix (cheap):** keep file as-is; grep `filepath.Join` in `cli/` and ensure every path-construction is behind a named helper. No package needed.

**Fix (full):** new `cli/paths.go` with one function per logical path (`Repo(scope, label)`, `SessionState(label)`, `HubFossilURL()`, etc.). Other files import from it.

### RF-6 — `WriteHubInfo` silently skips `PrimaryURL`

`hubinfo.go:48-62` deliberately ignores `info.PrimaryURL` (comment at line 47 explains the daemon owns it via `Lease.Publish`). Callers can pass a full `HubInfo` and the no-op is invisible.

**Smell:** **obscure dependency**. A future caller will pass `PrimaryURL` and wonder why it's not written.

**Fix:** drop the field from the `HubInfo` struct entirely if it's never written through this path. Have `Lease.Publish` accept a primary URL string. Two channels, two concerns. Less surface, more honesty.

### RF-7 — `autoShutdownLoop` has no idle timeout

Polls every 500ms. `hadLeaf` latches true on first connect; shutdown fires only when leaves go to zero *after* that. If a leaf flap-cycles forever (connects, disconnects, reconnects), the hub never exits.

**Smell:** **unhandled hypothetical**. Likely fine in practice (sesh sessions are long-lived). But the loop should document the invariant or add a max-idle-since-first-leaf timeout.

**Fix (minimal):** doc comment explaining the design choice. **Fix (full):** add `MaxIdleTime` field to `HubServeCmd`; if set and hub is empty for that long, exit.

### RF-8 — Session state split between `state.go` and `session.go`

`SessionState` struct lives in `state.go:34-46`. `Session` lifecycle in `session.go`. Reading "what is a session" requires two files.

**Smell:** mild **information scatter**. Could be addressed by moving `SessionState` into `session.go` or renaming `state.go` to `paths.go` (since path helpers are most of its remaining content).

**Fix:** move `SessionState` to `session.go`. `state.go` becomes path/project-code helpers only — rename to `paths.go` if you go that route.

---

## Cross-cutting observations

- **The campaign extracted four deep modules and one shallow orchestrator.** `up.go:Run` *absorbed* the responsibilities the other modules surfaced. It's now the single point of "the order of operations is encoded here." Future module extractions should sharpen this — RF-1's `Starter` is the natural next step.

- **Test depth tracks module depth.** `bootstrap_test.go`, `hubguard_test.go`, `hubinfo_test.go`, `session_test.go` are table-driven against the public interface. `multi_session_integration_test.go` does the cross-module integration. The boundary is clean: unit tests don't reach into internals; integration tests don't duplicate unit coverage. Keep this discipline going.

- **Naming improved on three concepts and regressed on one.** Improved: `HubInfo` (was three loose `os.ReadFile(hubXURLPath())` patterns), `HubGuard.Lease` (was raw flock fd plus O_EXCL goo), `Session` (was three free functions). Regressed: `ProjectCode` got four spellings during the campaign (see RF-4).

- **Tier-1 safety held.** No code path in cli/ touches `~/.sesh/messaging/` or `.sesh/sessions/<label>.messaging/`. All `os.Remove` calls hit URL pointer files, session-state JSON, or atomic-write temp files. Memorized; respect it on future PRs.

---

## Wins

Places to copy-paste the pattern from:

- **`hubguard.Lease` state machine** (`hubguard.go`). `leaseNone`/`leaseSpawner`/`leaseDaemon` with runtime-panic on misuse. Caller can't accidentally call `Publish` on a non-daemon lease. Good *define errors out of existence*.

- **`hubinfo.WriteHubInfo` atomicity** (`hubinfo.go`). Temp-rename per file. Partial-publication is well-tested. The Read side tolerates missing files (zero strings, not errors). Clean.

- **`session.Terminate` graceful degradation** (`session.go`). Missing file → no-op. Dead PID → reap. Live PID → SIGINT + wait + timeout. The caller (`down.go`) is trivial because the depth got pulled down. Textbook *pull complexity downward*.

- **`MakePlan` purity** (`bootstrap.go`). No I/O. Tested in isolation. The Execute side stays where the I/O is. The shape is right; the only issue is Execute's split, not the Plan/Execute partition itself.

---

## Recommended follow-up issues (ranked)

1. **Extract `Starter` over `up.go:Run`** — addresses RF-1. Highest leverage. Future pipeline additions (goal-management, agent attach, etc.) plug in here.
2. **Collapse `bootstrap.Execute`'s two phases into one `Apply(plan, hub, deps)`** — addresses RF-2. Side-effect of (1), might be done in the same PR.
3. **Tighten `ProbeHub` error semantics** — addresses RF-3. Small PR; sum-type or three-return shape; caller chooses degradation explicitly.
4. **Unify `ProjectCode` naming** — addresses RF-4. Pure rename; 5-10 lines.
5. **Add `paths.go` (or audit `filepath.Join` callsites)** — addresses RF-5. Defer until a new scope is added or a path moves; not urgent.
6. **Drop `HubInfo.PrimaryURL` field** — addresses RF-6. Two-line struct change + small caller cleanup.
7. **Doc or add max-idle-after-first-leaf to `autoShutdownLoop`** — addresses RF-7. One-comment fix or one-flag feature.
8. **Move `SessionState` into `session.go`; rename `state.go` → `paths.go`** — addresses RF-8. Cosmetic but improves "where do I look" answer.

(1) and (2) are the only ones that touch the architecture meaningfully. The rest are local hygiene.
