# F3 — `sesh up` claims the session label exclusively

**Date:** 2026-05-22
**Status:** AFK-ready (one operator decision deferred — see bottom)
**Severity:** P1 — design clarification, possibly correct-as-implemented
**Owner:** sesh (docs + optional flag)

## Root cause

`/Users/dmestas/projects/sesh/cli/session.go:79-105 ClaimSession` opens the session state file with `O_CREATE|O_EXCL|O_WRONLY`. If a live PID already owns the slot it returns an error; if the existing claim's PID is dead, the file is reaped first and the claim proceeds. This means **one and only one `sesh up` can hold a given session label** at any time.

This is intentional design (the SessionState comment at `session.go:35-54` is unambiguous about it). The integration rig's original plan assumed two parallel `sesh up`s could attach to the same hub under the same label — the plan's "two parallel sesh up" pattern. That doesn't match the implementation.

The rig already worked around it by using a single `sesh up --exec=/tmp/launch-agents.sh` where the exec is a bash wrapper that fan-outs to claude + omp. That pattern is fine and matches what the docs (when written) should recommend.

## Alternatives considered

### Option A — Document the single-wrapper model

Add a section to `~/projects/sesh/docs/synadia-agents-on-sesh.md` and `cli/up.go`'s flag help text explaining that a session label has at most one owner, and how to run multiple adapters via a single exec.

**Interface complexity:** none — docs only.
**Blast radius:** docs only.
**Reversibility:** trivial.
**Risk:** none.

### Option B — Add a `--join` / `--secondary` flag to `sesh up` that attaches to an existing claim

Operators with this need would invoke `sesh up --session=foo --join --exec=...` to add a second adapter under the existing claim. This requires:

1. New flag and parsing in `cli/up.go`.
2. New mode in `Session` that opens the slot for read-only access without re-claiming.
3. Teardown semantics: when does the secondary process die? Tied to primary's lifetime? Independent? Both?
4. Race semantics: if the primary exits while the secondary is still running, what happens to the secondary's children?

**Interface complexity:** moderate-to-high.
**Blast radius:** `cli/up.go`, `cli/session.go`, plus tests, plus docs.
**Reversibility:** medium — flag can be removed, but if users adopt it the removal is breaking.
**Risk:** the teardown / race semantics need careful design (Ousterhout territory: introducing a primary/secondary split where the model used to be "one claim, one process tree" creates new ambiguity).

### Option C — Change the model so multiple `sesh up`s coexist under the same label

Replace the O_EXCL slot with reference-counted state. First `sesh up --session=foo` initializes; subsequent ones increment; release decrements; last one out wipes the state file.

**Interface complexity:** high — the SessionState contract changes (no longer a single owning PID).
**Blast radius:** `cli/session.go`, watchers, every downstream consumer of `SessionState.PID`.
**Reversibility:** poor — once shipped, callers depend on the new semantics.
**Risk:** invasive. Could regress the "live PID = live session" invariant defended in the session.go comments.

### Chosen approach — Option A (docs only)

The current model is correct for the design center: a sesh session is a coherent unit of "this work-in-progress on this label", and the operator should have one process owning it. Multiple adapters in one session are spawned via a single `--exec` that fan-outs (the integration rig already validates this works fine).

The cost of formalizing this in docs is small; the cost of B or C is large; the benefit of allowing parallel `sesh up`s isn't demonstrated by any user-visible workflow yet.

If Option B becomes desirable later, it should land as its own RFC — not bundled into the integration-rig fixes.

## Operator decisions deferred

**Decision F3.1 — Stop at docs (Option A), or also add `sesh up --join` (Option B)?**

**Axis: architecture.** Adding `--join` locks in primary/secondary semantics across the session/up/watcher modules and is expensive to undo. Pick before AFK dispatch.

Plan ships with Option A selected. If the operator wants Option B, this plan needs an additional Task 3-N covering the `--join` implementation + tests; flag in the index README.

## AFK-ready plan (Option A — docs only)

### Task 1 — Add a "session ownership" section to the existing sesh docs

**File:** `/Users/dmestas/projects/sesh/docs/synadia-agents-on-sesh.md`

Append (or insert before the existing "Discovery" section, whichever placement fits the doc's current TOC):

```markdown
## Session ownership

A sesh session label (e.g., `smoke-test`) is owned by **exactly one** `sesh up`
process at a time. The state file at
`<cwd-up-walk>/.sesh/sessions/<label>.json` is created with `O_CREATE|O_EXCL`
by `ClaimSession` (cli/session.go:79) — a second `sesh up --session=<label>`
in another shell will fail with "session %q already held by pid %d".

This is intentional. A session has one canonical owner (its `pid` field is
read by `sesh down`, `sesh status`, the agent watcher, and downstream
tools); a single owner is what makes the lifecycle deterministic.

### Running multiple adapters in one session

Spawn them all under a single `sesh up --exec=<wrapper>`. The wrapper is a
small shell script (or any executable) that fans out and waits — the
integration rig at `test/integration/entrypoint.sh` is a working example:

```bash
sesh up --session=my-session --exec=/path/to/launch-agents.sh
```

Where `/path/to/launch-agents.sh` is:

```bash
#!/usr/bin/env bash
set -o pipefail
# Per-process role overrides (the outer `sesh up` sets a neutral role; each
# child can override SESH_ROLE for its own metadata).
(
  export SESH_ROLE=implementer
  exec claude --dangerously-skip-permissions --mcp-config /path/to/mcp.json
) > /var/log/claude.log 2>&1 &
CLAUDE=$!
(
  export SESH_ROLE=planner
  exec omp
) > /var/log/omp.log 2>&1 &
OMP=$!
wait -n $CLAUDE $OMP
EC=$?
kill $CLAUDE $OMP 2>/dev/null || true
wait
exit $EC
```

This pattern preserves the one-owner invariant: a single `sesh up` is the
canonical owner; the wrapper's children inherit `SESH_SESSION`, `NATS_URL`,
etc. and register on the bus under that one session's label.

### What about `sesh up --session=foo` from a second shell?

It fails with the "already held" error. If you want a second, parallel
session, pick a different label:

```bash
sesh up --session=foo &
sesh up --session=bar &
```

Each gets its own `.sesh/sessions/<label>.json`, its own state, its own
agent set on the bus.
```

### Task 2 — Update `sesh up --help` text

**File:** `/Users/dmestas/projects/sesh/cli/up.go`

Find the `--session` flag's `help` tag (search for `Session` + `help:"`) and replace with text that mentions exclusivity. Approximate sentence to splice in (exact phrasing matches the help-text style of neighboring flags):

```
Session label. Held exclusively by this sesh up — a second `sesh up --session=<same-label>` in another shell will fail. Run multiple adapters in one session by passing a multiplex wrapper to --exec.
```

### Task 3 — Cross-link from `cli/session.go`

The `Session` doc comment at lines 56-66 is already accurate. Add one line linking out to the new doc section so future readers find the why:

```diff
-// Session owns a project-local state file at <stateDir>/<label>.json. It
-// represents the lifecycle of a single `sesh up` between claim and release:
+// Session owns a project-local state file at <stateDir>/<label>.json. It
+// represents the lifecycle of a single `sesh up` between claim and release.
+// One owner per label is enforced; see docs/synadia-agents-on-sesh.md
+// "Session ownership" for the rationale and the wrapper-exec pattern for
+// running multiple adapters in one session.
```

### Task 4 — Failing test? (none — docs only)

This finding has no test surface. The behavior is correct; we are documenting it. If the operator wants to harden against a future regression of the O_EXCL semantics, add a unit test in `cli/session_test.go`:

```go
// TestClaimSessionIsExclusive verifies that a second claim against a live
// PID's session label fails. Guards against accidental relaxation of the
// O_EXCL semantics — see docs/synadia-agents-on-sesh.md.
func TestClaimSessionIsExclusive(t *testing.T) {
    dir := t.TempDir()
    s1, err := ClaimSession(dir, "shared-label")
    if err != nil {
        t.Fatalf("first claim: %v", err)
    }
    defer s1.Release()

    if _, err := ClaimSession(dir, "shared-label"); err == nil {
        t.Fatalf("second claim should have failed; got nil error")
    }
}
```

Optional — include if the operator wants the regression-guard, skip if pure docs is enough.

### Task 5 — Commit + PR

```bash
cd /Users/dmestas/projects/sesh
git checkout -b docs/session-ownership-clarification
git add docs/synadia-agents-on-sesh.md cli/up.go cli/session.go cli/session_test.go
git commit -m "docs(session): clarify single-owner-per-label semantics + wrapper-exec pattern (closes F3)"
```

Open a PR. Do not push to main directly.

## Dependencies

None. F3 is independent of all other findings.

## Optional follow-ups

- If multiple-adapters-per-session is a recurring operator pattern, propose a `sesh up --multiplex` shortcut that takes multiple `--exec` flags and wraps them under the hood (eliminating the need for hand-written launcher scripts). **This is Option B in a smaller form — surface to operator as a separate decision, do not implement here.**
