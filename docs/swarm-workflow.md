# Fossil-as-trunk swarm workflow

> **Tracking:** [sesh#64](https://github.com/danmestas/sesh/issues/64)
> **Last update:** 2026-05-17
> **Slices covered:** 1–8 (EdgeSync#181, sesh#65, sesh#67, wardrobe#132,
> sesh#68 + orch#139, wardrobe#134, sesh#70, this doc)

Operator runbook for running multi-worker missions where workers iterate
against a shared **fossil trunk** instead of pushing to git branches.
Git remains the maintainer's review surface; fossil is the workers'
coordination substrate.

## When to use this

Use the swarm workflow when:

- a mission has **two or more concurrent workers** (an implementer and
  a verifier; two implementers on independent parts; etc.);
- the workers need to **see each other's commits as they land**, not at
  push-time;
- you want a **single PR** at the end, not one PR per worker.

Skip it for single-developer single-task work. The git-branch-and-PR
path is simpler when there is no second worker on the same code.

## Shape

Three layers sit between the operator and the merge:

```
   workers           — implementer + verifier, each in their own checkout
       |
       v   (fossil commit; autosync over NATS)
   fossil trunk      — shared trunk, per-session repo or shared project repo
       |
       v   (sesh materialize → git worktree)
   git worktree      — operator's review surface; one PR to upstream
```

Workers commit to fossil; commits propagate to peer checkouts via NATS
autosync. When the mission is complete the operator runs
`sesh materialize <label>` to overlay the fossil trunk HEAD into the
git worktree, then opens a PR the normal way.

## The mission loop

Concrete end-to-end recipe. Substitute `alpha` for your own label.

```bash
# 1. Bring up a session (use --scope=project for multi-worker missions)
sesh up --session=alpha --scope=project &

# 2. Provision the worker's fossil checkout
sesh worktree alpha
# stdout: <project>/.sesh/checkouts/alpha

# 3. Spawn the implementer into that checkout
orch-spawn claude --sesh-session alpha --outfit backend --cut executing \
  --accessory fossil-worker --accessory pr-policy
# Implementer lands cwd=<project>/.sesh/checkouts/alpha/

# 4. (Implementer iterates; commits to fossil trunk; autosync propagates)

# 5. Spawn the verifier once the implementer signals mission complete
orch-spawn claude --sesh-session alpha --outfit engineer --cut reviewing \
  --accessory fossil-verifier
# Verifier reports GO / NO-GO / INCONCLUSIVE per the template

# 6. On GO: materialize fossil trunk → git, then open the PR
sesh materialize alpha --scope=project --git-add
git commit -m "mission: <description>"
gh pr create

# 7. Tear down
sesh down --session=alpha
```

`sesh up` runs in the foreground. Either background it with `&` (as
above) or run it in its own terminal/pane. `sesh down --session=alpha`

`sesh up` also registers the operator's `$USER` in the seeded fossil
repo's user table with check-in capability (`i`). Workers spawned
under the operator's UID can `fossil commit` from a checkout without
the operator manually running `fossil user new` — the registration is
idempotent across re-runs.
sends SIGINT and waits for the foreground process to exit.

## Scope choice: `session` vs `project`

`sesh up --scope=<mode>` controls where the session's fossil repo lives.

| Mode               | Backing repo                              | Convergence                                                                          |
| ------------------ | ----------------------------------------- | ------------------------------------------------------------------------------------ |
| `session` (default)| `.sesh/sessions/<label>.repo`             | Cross-session commits propagate **eventually** via NATS autosync                     |
| `project` (opt-in) | `.sesh/project.repo` (shared file)        | Cross-session commits **synchronous** via shared SQLite WAL; autosync adds NATS fan-out |

Worked example, `session` (solo mission):

```bash
sesh up --session=solo &           # writes .sesh/sessions/solo.repo
sesh worktree solo                 # checkout backed by solo.repo
# spawn one worker, iterate, materialize, PR
```

Worked example, `project` (two workers on the same mission, same machine):

```bash
sesh up --session=alpha --scope=project &
sesh up --session=beta  --scope=project &
sesh worktree alpha                # backed by .sesh/project.repo
sesh worktree beta                 # backed by .sesh/project.repo (same file)
# spawn implementer into alpha's checkout, verifier into beta's
# both see the same trunk synchronously
```

**Rule of thumb:** if two `sesh up` processes need to share fossil
state on the same machine, use `--scope=project`. If a worker on a
remote machine needs to share state, both ends still use their own
local repo (`session` mode) and autosync propagates via the hub's
fossil-sync subject.

See [scoped-memory § fossil scope](./scoped-memory.md#storage-selection)
for the full convergence semantics. Mixing `session` and `project`
within one mission works, but the cross-mode dance is described in the
[README](../README.md#fossil-scope-session-vs-project) — prefer to
keep one mission on one mode.

## Implementer protocol

The `fossil-worker` accessory carries the load-bearing rules. Operator-
facing concerns only:

**Recognising "implementer needs an operator decision."** The
implementer surfaces using the template in
`accessories/fossil-worker/accessory.md` — a fixed shape with the file,
the hunks, intent, and a recommended resolution. If the implementer
asks an open-ended "what should I do?" without that shape, redirect:
the surfacing template is the protocol.

**Re-spawn vs accept partial work.** If the implementer says "mission
complete with caveats," read the caveats. Either:

- re-spawn the implementer with a directive that names the caveat
  ("address the slow-test caveat from your last report"); or
- accept and pass to the verifier — the verifier will catch real
  defects and the caveat becomes documented context for the PR review.

The implementer never opens PRs from inside the checkout. PRs are the
operator's gatekeep.

## Verifier protocol

The `fossil-verifier` accessory carries the load-bearing rules. The
verifier produces a fixed-template verdict:

```
Verdict: GO | NO-GO | INCONCLUSIVE
```

| Verdict        | Operator action                                                                       |
| -------------- | ------------------------------------------------------------------------------------- |
| `GO`           | Materialize and open the PR.                                                          |
| `NO-GO`        | Either re-spawn the implementer with the findings, or materialize-with-caveats — operator's call. |
| `INCONCLUSIVE` | Verifier couldn't audit (missing infra, ambiguous criteria). Resolve the blocker, then re-spawn the verifier OR ship with explicit reviewer attention. |

A `NO-GO` is never "merge anyway because the issue is minor" — the
verifier never makes that call unilaterally and neither should the
operator without owning the trade-off explicitly in the PR body.

## Conflict resolution

Two workers committing simultaneously:

- **Independent lines (trivial case).** Fossil's text-merge auto-
  resolves both commits. Both land on trunk transparently; no operator
  intervention.
- **Same line (judgment-required case).** The second commit errors.
  The worker surfaces to the operator using the `fossil-worker`
  surfacing template — file path, hunks, intent, recommendation.
  Operator decides the resolution OR which worker takes priority and
  re-spawns the loser with the merged context.

Worker-side discipline (pull before non-trivial work, surface on
conflict, never `--force`) is tracked under `fossil-worker` —
operators should not see frequent surfacings if the accessory is
loaded correctly. If a worker is spamming surfaces, audit that the
accessory actually loaded.

## Recovery procedures

**Abort an in-flight mission.** Terminate workers (`orch-spawn`'s
companion `orch-kill`, or kill the tmux pane), then:

```bash
sesh down --session=alpha
```

Decide whether to keep the fossil checkout — it's at
`.sesh/checkouts/alpha/` and is throwaway in tier-1 terms (the trunk
itself lives in `.sesh/sessions/alpha.repo` or `.sesh/project.repo`).

**Force-recreate a corrupt checkout.** Safe; only touches the checkout
directory:

```bash
sesh worktree alpha --force-recreate
```

`.sesh/sessions/` and `.sesh/messaging/` are NOT touched.

**Recover an interrupted materialize.** Materialize is idempotent.
Just re-run:

```bash
sesh materialize alpha --scope=project --git-add
```

Materialize is overlay-only — files in the output dir that are absent
from the fossil trunk are LEFT ALONE. The git diff after materialize
shows what fossil contributed; you decide what to commit.

**Hub diagnostics.**

```bash
cat ~/.sesh/hub.log                     # auto-spawned hub's stderr/stdout
nats sub 'fossil.>' --server=$(cat ~/.sesh/hub.nats.url)
fossil timeline                         # run inside a .sesh/checkouts/<label>/
```

The `fossil.<project-code>.commit` subject carries the propagation
announces; if you see commits land on the subject but not in a peer
checkout, the propagation gap is on the receive side (peer's fossil
isn't pulling) not the announce side.

## Tier-1 safety

Three precious-ness tiers in a sesh project. Memorise this:

| Tier | Path                                                                | Treatment                                                                       |
| ---- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| 1    | `~/.sesh/messaging/`, `<project>/.sesh/sessions/<label>.messaging/` | **Irreplaceable.** JetStream lives here. NEVER `rm -rf`. NEVER any destructive op. |
| 2    | `~/.sesh/hub.repo`, `.sesh/sessions/<label>.repo`, `.sesh/project.repo` | Fossil repos. Recoverable from the hub if lost but valuable. Archive, don't delete. |
| 3    | `~/.sesh/hub.url`, `hub.nats.url`, `hub.fossil.url`, session JSONs   | Throwaway. Regenerate on next `sesh up`.                                        |

`sesh worktree --force-recreate` and `sesh materialize` are explicitly
scoped to tier-3 / tier-3-adjacent paths. They will not touch tier-1
or tier-2. Don't manually `rm -rf` inside `.sesh/` unless you know
exactly which tier you're touching.

## Troubleshooting checklist

**"Worker can't see peer's commits."**

1. Check the worker is in the right checkout: `pwd` should be
   `<project>/.sesh/checkouts/<label>`.
2. Confirm the announce is on the wire from a peer session:
   `nats sub 'fossil.>' --server=$(cat ~/.sesh/hub.nats.url)` — you
   should see a `fossil.<project-code>.commit` message per peer commit.
3. Inside the worker's checkout: `fossil timeline` should show the
   trunk. If autosync is on (it is, by default after `sesh worktree`),
   running any fossil command will pull. Try `fossil pull` explicitly.

**"`sesh up` fails to bind hub."** The hub is a singleton; only one
`sesh hub serve` per user. Check:

```bash
cat ~/.sesh/hub.url       # who currently holds it
cat ~/.sesh/hub.log       # boot errors
```

If `hub.url` points at a dead PID, the next `sesh up` performs a stale
takeover automatically. If the port is genuinely bound by another
process, you'll see a bind error in `hub.log`.

**"Materialize refuses dirty worktree."** This is intentional. Your
uncommitted git work is being protected.

```bash
git stash                                 # save your work
sesh materialize alpha --git-add
git stash pop
```

Or override explicitly:

```bash
sesh materialize alpha --allow-dirty --git-add
```

**"Validator rejects label."** Labels must match
`^[A-Za-z0-9._-]+$`, can't be `.` or `..`, can't contain `..`, can't
have path separators or NUL bytes, max 128 bytes. The validator is the
first call in every `sesh worktree`, `sesh materialize`, and
`sesh worker-cwd` — a hostile label is rejected before any path math
runs. See sesh#67 + sesh#68 for the gate.

## Testing the swarm

Two e2e test variants live in `cli/`:

- **Mock variant** (`cli/e2e_test.go`, default build tag). The "worker"
  and "verifier" are the test code itself driving the project repo via
  the libfossil Go API. Runs in normal CI; covers the load-bearing
  swarm properties through sesh's leaf-link surface.

  ```bash
  go test ./cli/ -run TestE2E -count=1 -timeout 360s -v
  ```

- **Orch variant** (`cli/e2e_orch_test.go`, build tag `orch_e2e`). Drives
  real `orch-spawn claude --sesh-session <label>` subprocesses through
  recipe fixtures executed by a stub-claude. Validates the same
  properties through the real subprocess chain (orch-spawn -> tmux ->
  claude).

  ```bash
  go test -tags=orch_e2e ./cli/ -run TestE2E -count=1 -timeout 360s -v
  ```

  The orch variant auto-wires to stubs in `cli/testdata/orch_e2e/`
  with no operator setup required, provided `orch-spawn`, `tmux`, and
  `fossil` are on PATH. The tests SKIP cleanly when any prerequisite
  is missing. To point at a real `claude` binary or custom recipes,
  override with `SESH_E2E_CLAUDE=/path/to/claude` and/or
  `SESH_E2E_RECIPE_DIR=/path/to/recipes`. See
  `cli/testdata/orch_e2e/README.md` for the fixture wiring.

## Where to learn more

- [sesh#64](https://github.com/danmestas/sesh/issues/64) — the
  tracking issue with slice history and design notes.
- `accessories/fossil-worker/accessory.md` (in wardrobe) — implementer-
  side discipline (command discipline, surfacing template, mission-
  complete signal).
- `accessories/fossil-verifier/accessory.md` (in wardrobe) — verifier-
  side discipline (verdict template, allowed inspection commands).
- [scoped-memory.md](./scoped-memory.md) — the scope conventions the
  swarm workflow shares with other sesh state.
- [`cli/swarm_tbd_integration_test.go`](../cli/swarm_tbd_integration_test.go)
  — `TestSwarmTBD_TwoWorkers_ConvergeOnSharedTrunk`. Empirical proof of
  the end-to-end loop; the runbook commands above are the operator
  surface of what that test asserts mechanically.
