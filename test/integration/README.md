# sesh integration rig

End-to-end Docker-based integration test for **sesh + sesh-channels + Claude Code + Oh My Pi**, exercising the recent role/class + `sesh up --exec` work in one self-contained run.

## What it does

Spins up a container that:

1. Builds sesh from the in-tree source (no remote clone — the host's working copy is the truth).
2. Installs Claude Code (`@anthropic-ai/claude-code`) and Oh My Pi (`@oh-my-pi/pi-coding-agent`).
3. Mounts the operator's local OAuth state from macOS keychain (Claude) and `~/.omp/agent` (OMP) so no API keys go through env vars.
4. Runs two parallel `sesh up --exec` invocations against a single session `smoke-test`:
   - `--role=implementer --class=active` spawns `claude` with the `claude-nats-channel` MCP server
   - `--role=planner --class=active` spawns `omp` with the `omp-nats-channel` extension
5. Executes a TypeScript harness with 8 ordered cases — registration, heartbeats, prompt/reply on each agent, attachment round-trip, cross-adapter chatter, session JSON shape, and a steady-state stability window.

Findings land in `artifacts/results.txt` + `artifacts/session-smoke-test.json` + per-agent logs.

## Prerequisites

- macOS host (the cred-staging script uses `security find-generic-password`)
- Claude Code signed in locally (`security find-generic-password -s "Claude Code-credentials" -w` must succeed)
- `omp` installed locally and signed in (`~/.omp/agent/agent.db` must exist with non-empty `auth_credentials`)
- Docker Desktop with `compose v2`
- `sesh-channels` repo cloned at `~/projects/sesh-channels` (override with `$SESH_CHANNELS_DIR`)

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

## Run

```bash
cd test/integration
bash scripts/run.sh
```

`run.sh` calls `scripts/stage-creds.sh` (copies your OAuth state into a tmpdir, writes `.env`) then `docker compose up --build --abort-on-container-exit --exit-code-from sesh-integ`. Exit code = harness exit code (0 if all 8 cases pass, 1 if any fail, 2-3 for rig bootstrap errors).

## Inspecting results

After a run, `test/integration/artifacts/` contains:

- `results.txt` — full harness stdout (markdown PASS/FAIL table + JSON dump)
- `session-smoke-test.json` — sesh session manifest after the run
- `claude.log` / `omp.log` — per-agent sesh+adapter stderr
- `hub.log` — sesh hub serve log
- `hub-state.txt` — final URL files + session JSON snapshot

## Architecture notes

- **Single container, one process tree.** PID 1 is tini; entrypoint spawns two `sesh up` wrappers, each of which spawns claude/omp under `script -qfc` for a real PTY (both agents misbehave without one). The harness runs as a fourth child once the bus has settled.
- **Bus discovery.** `sesh up` writes `~/.sesh/hub.nats.url` after the first invocation binds NATS; the second `sesh up` reuses that hub via sesh's lease guard rather than spawning its own. Adapters and harness all read the same file.
- **No env-var keys.** Claude OAuth comes from `~/.claude/.credentials.json` (mounted RO from a tmpdir copy of the macOS Keychain). OMP credentials come from the SQLite `auth_credentials` table in a writable copy of `~/.omp/agent/agent.db`.
- **`project-id` baked at build.** `coordinateLoop` in the sesh refagent skips coordinate subjects when `.sesh/project-id` is missing; we generate a 40-hex value at image-build time.
- **`/etc/machine-id` via dbus-uuidgen.** Without it, the `coord` package returns its `MachineLocal` sentinel; baking a uuid keeps the machine token deterministic per image.

## Cases

| # | Name | What it checks |
|---|------|----------------|
| 01 | registration | Both adapters appear on `$SRV.INFO.agents` with correct role/class/session metadata |
| 02 | heartbeats | At least 2 heartbeats per agent in 20s on 5-token `agents.hb.*` subjects |
| 03 | prompt-claude | Stream + reply via Synadia SDK contains "SUCCESS" |
| 04 | prompt-omp | Same shape against OMP (longer budget) |
| 05 | attachment | 10-byte attachment delivered to claude; response cites length 10 |
| 06 | cross-adapter | Caller-mediated A→harness→B chatter (v1 interpretation per plan) |
| 07 | session-json | `agents[]` in session JSON has both agents with correct subject + role/class |
| 08 | steady-state | 60s window — heartbeat count grows monotonically, agent set stable |

## Re-running with edits

The Dockerfile is structured to cache: changes to `harness/`, `entrypoint.sh`, or `config/` rebuild fast; changes to `cmd/sesh/` invalidate stage 1.

```bash
bash scripts/run.sh           # full path
docker compose build sesh-integ  # rebuild without running
docker compose run --rm sesh-integ /bin/bash  # interactive shell
```

## Known limitations

- macOS-only cred staging (the rig itself runs Linux containers; only the host script is mac-specific).
- 60s steady-state window is short; raise `WINDOW_MS` in `08-steady-state.ts` if you need a longer soak.
- Cross-adapter case is caller-mediated only — no claude→omp tool surface today (tracked as an enhancement note in `FINDINGS.md`).

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
