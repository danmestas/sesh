# F7 + F8 — Rig polish (ANSI stripping + disk preconditions)

**Date:** 2026-05-22
**Status:** AFK-ready (cleanly coupled — both are rig-only docs/scripting tweaks)
**Severity:** P4 — UX polish
**Owner:** sesh rig (`test/integration/`)

## Findings being addressed

- **F7** — OMP's TUI emits ANSI escapes into stdout/logs. Logs need `col -b` or `ansi2txt` to be grep-able.
- **F8** — Docker build fails with "input/output error" when host disk is tight (<1 GB free).

Both are environmental / cosmetic. Bundling them into one PR keeps the rig-polish surface coherent.

## Root cause

### F7

OMP detects TTY at startup via `isatty(STDOUT_FILENO)` (per its TUI library — `@earendil-works/pi-coding-agent`). Under the rig's `script -qfc <cmd> /dev/null` wrapper (which the rig uses to give OMP a fake TTY for *startup* purposes), OMP keeps emitting ANSI escapes — the PTY wrapper makes it think it's on a terminal, so it formats accordingly. Net: `/var/log/omp.log` is full of `\e[...m` sequences.

This is unavoidable downstream of `script -qfc` (which is itself a workaround for OMP refusing to boot under non-TTY). The fix is post-processing: pipe OMP's output through `col -b` (which strips backspace + ANSI control sequences) before writing the log.

### F8

`oven/bun:1.3.14-debian` is ~600 MB. The sesh-builder stage and sesh-channels copy stages add another ~200-300 MB combined. Docker Desktop's containerd ingest fails with cryptic "input/output error" when the host's `/var/lib/docker` (or Docker Desktop's Lima VM disk) is under ~1 GB free. The rig itself doesn't cause this; it's a Docker Desktop bug surfacing under disk pressure.

No code fix is possible. Document the precondition.

## Alternatives considered

### Option A — Apply both fixes (pipe OMP through `col -b` + document disk preconditions)

**Interface complexity:** trivial — one pipe + a README section.
**Blast radius:** rig only.
**Reversibility:** trivial.

### Option B — Force OMP to non-color mode via `NO_COLOR=1`

Set `NO_COLOR=1` in OMP's env. This is a widely-honored convention (no-color.org) that many TUI libraries respect.

**Interface complexity:** one env-var export.
**Blast radius:** OMP only.
**Risk:** OMP's TUI library may not honor `NO_COLOR`. Worth trying as a belt-and-braces alongside `col -b` — `col -b` strips what NO_COLOR doesn't suppress, and NO_COLOR suppresses what `col -b` doesn't strip.

### Chosen approach — Option A + Option B

Both layered. `NO_COLOR=1` at the source; `col -b` post-processor as a safety net.

## Operator decisions deferred

None. Both findings are rig-only and have no 4-axes implications.

## AFK-ready plan

### Task 1 — F7: Pipe OMP's stdout through `col -b` + set `NO_COLOR=1`

**File:** `/Users/dmestas/projects/sesh/test/integration/entrypoint.sh`

Locate the OMP launcher block (`launch-agents.sh` here-doc, currently lines 136-149). Replace the OMP-side subshell:

```bash
(
  export SESH_ROLE=planner
  export SESH_CLASS=active
  # Suppress ANSI color codes at the source (NO_COLOR is the
  # no-color.org convention) and pipe the remaining output through
  # `col -b` to strip any escape sequences that slip through. Without
  # both, /var/log/omp.log is unreadable without an ANSI-stripping
  # post-processor. (F7)
  export NO_COLOR=1
  export TERM=dumb
  # omp-nats-channel/extensions/nats-channel.ts only reads
  # NATS_SESSION_NAME for the session token — it does NOT consult
  # SESH_SESSION like claude-nats-channel/server.ts does. Work around
  # by setting NATS_SESSION_NAME explicitly. (F2 workaround — remove
  # after omp-nats-channel adopts the SDK's readSessionLabel helper.)
  export NATS_SESSION_NAME="${SESH_SESSION:-}"
  echo "[omp-side] SESH_SESSION=$SESH_SESSION NATS_SESSION_NAME=$NATS_SESSION_NAME SESH_ROLE=$SESH_ROLE SESH_CLASS=$SESH_CLASS HOME=$HOME PATH=$PATH NATS_URL=$NATS_URL" >&2
  exec script -qfc "omp" /dev/null < /tmp/omp.fifo
) 2>&1 | col -b > /var/log/omp.log &
OMP=$!
echo "[exec-wrapper] omp pid=$OMP" >&2
```

Note the `2>&1 | col -b` redirect — merges stderr into stdout, then strips, then writes the file.

**Dockerfile already includes `util-linux`** (which provides `col`) per line 46. No new dependency.

### Task 2 — F8: Disk precondition section in the rig README

**File:** `/Users/dmestas/projects/sesh/test/integration/README.md` — prepend a "Preconditions" section to the top (or insert above "Quick start" — whichever placement matches the README's current flow):

```markdown
## Preconditions

Before building the rig, verify:

1. **Docker Desktop running.** macOS: confirm via `docker info`. The rig
   uses BuildKit (`# syntax=docker/dockerfile:1.6`) and multi-stage builds;
   older Docker daemons may not support all features.

2. **At least 2 GB of free disk space** in Docker Desktop's storage volume.
   The rig's image weighs ~900 MB; intermediate build layers + node_modules
   for the harness easily push the working set over 1 GB.

   On a tight disk Docker's containerd ingest fails with a cryptic
   `input/output error` during the build (F8 in the rig findings doc). To
   reclaim space:

   ```bash
   docker system prune -a --volumes
   docker buildx prune -a
   ```

   If you have shared a project's `node_modules` into a Docker bind mount,
   that node_modules may have been silently growing in the Docker Desktop
   VM — `docker run --rm -v $(pwd):/host alpine du -sh /host/node_modules`
   can surface unexpected hot spots.

3. **Operator credentials staged.** Run `scripts/stage-creds.sh` (or the
   equivalent host-side helper) before `docker compose up`. The script
   reads the operator's macOS keychain for Claude OAuth and copies OMP's
   `~/.omp/agent/agent.db` into a tmpdir for bind-mounting.

4. **`tmpfs /tmp`** — the rig uses `/tmp/launch-agents.sh` and named FIFOs
   under `/tmp/`. Default Docker / overlay2 / tmpfs configurations all
   suffice; no special tuning.
```

### Task 3 — F7: README note about ANSI stripping

**File:** `/Users/dmestas/projects/sesh/test/integration/README.md` — append (after the existing log-inspection notes, if any):

```markdown
## Log readability (F7)

OMP's TUI emits ANSI escape sequences even when run under `script -qfc`
(the PTY wrapper the rig uses to satisfy OMP's TTY startup check). The
entrypoint exports `NO_COLOR=1 TERM=dumb` (which most TUI libraries
honor as a request to skip color output) AND pipes OMP's output through
`col -b` (which strips backspace + escape sequences from the remaining
output). Either alone is insufficient on its own — the pair is
belt-and-braces.

If you ever need to inspect OMP's log without stripping (e.g., debugging
the TUI itself), comment out the `| col -b` in `entrypoint.sh` and
unset `NO_COLOR` / `TERM`.
```

### Task 4 — Commit + PR

```bash
cd /Users/dmestas/projects/sesh
git checkout -b chore/integration-rig-f7-f8-polish
git add test/integration/entrypoint.sh test/integration/README.md
git commit -m "chore(test/integration): strip ANSI from OMP logs + document disk preconditions (closes F7, F8)"
```

Open a PR. Don't push to main directly.

## Dependencies

- **F7 / F8 lands independently of F1-F6.** Conflicts: F2's workaround removal step touches the same OMP-side block as F7's `col -b` addition. Land F7 first; F2's later rebase removes the `NATS_SESSION_NAME` export but keeps F7's `NO_COLOR`/`col -b`. Resolution is mechanical.

## Optional follow-ups

- File an upstream issue at oh-my-pi/pi-coding-agent (or the underlying TUI library) requesting `NO_COLOR` honoring. **Per CLAUDE.md no-third-party-filing, do NOT file. Surface as an operator note.**
- Add a CI check that fails the build if `du -sh /var/lib/docker` (or the equivalent on Docker Desktop) drops below a threshold during the rig's own build step. Out of scope for F8; rig docs cover the manual check.
