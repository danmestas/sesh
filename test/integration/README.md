# sesh integration rig

End-to-end Docker-based integration test for **sesh + sesh-channels + Claude Code + Oh My Pi**, exercising the recent role/class + `sesh up --exec` work in one self-contained run.

## What it does

Spins up a container that:

1. Builds sesh from the in-tree source (no remote clone — the host's working copy is the truth).
2. Installs Claude Code (`@anthropic-ai/claude-code`) and Oh My Pi (`@oh-my-pi/pi-coding-agent`).
3. Mounts the operator's local OAuth state from macOS keychain (Claude) and `~/.omp/agent` (OMP) so no API keys go through env vars.
4. Runs two parallel `sesh up --exec` invocations against a single session `smoke-test`:
   - `--role=implementer --class=active` spawns `claude` in **plugin-mode**, with the `nats-channel@sesh-channels` plugin pre-installed at image-build time (mirrors the operator's production invocation)
   - `--role=planner --class=active` spawns `omp` with the `omp-nats-channel` extension
5. Executes a TypeScript harness with 8 ordered cases — registration, heartbeats, prompt/reply on each agent, attachment round-trip, multi-round cross-adapter conversation (3 alternating rounds, claude ↔ omp), session JSON shape, and a steady-state stability window.

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
| 06 | cross-adapter | 3-round chained-arithmetic conversation: claude → omp → claude, harness-mediated relay |
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
- Cross-adapter case is caller-mediated only — the harness brokers each round, no direct claude→omp tool surface (tracked as an enhancement note in `FINDINGS.md`).

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

## NATS URL caching (F6)

`~/.sesh/hub.nats.url` is alive iff the hub daemon is alive. The hub auto-
shuts-down when its last leaf disconnects, so the file vanishes when
`sesh up` exits — *before* the harness has finished snapshotting artifacts.

The entrypoint caches the URL on first sighting:

```bash
cp -f ~/.sesh/hub.nats.url /var/artifacts/hub.nats.url
```

Downstream tools (the harness, post-run inspection scripts) read from
`/var/artifacts/hub.nats.url`, not from `~/.sesh/hub.nats.url`. This
matches the documented lifecycle in
[`docs/synadia-agents-on-sesh.md` § 2.1](../../docs/synadia-agents-on-sesh.md#21-nats-url-discovery-and-lifecycle).

For per-session URL discovery (which session owns which hub), prefer
`<cwd>/.sesh/sessions/<label>.json#nats_url` over `hub.nats.url` — the
session JSON's `nats_url` field is written at `sesh up` boot and is the
canonical per-session reference.

## OMP + claude co-location MCP discovery hazard (F5)

OMP's config has `mcp.discoveryMode: true` (intentional, upstream behavior —
see `omp-config.yml` and Oh My Pi's docs). When OMP and claude run in the
same workspace and a `.mcp.json` is present, OMP autoloads every entry —
including ones intended for claude only — spawning duplicate channel
instances. The `claude-nats-channel/server.ts`'s `resolveSessionName`
auto-suffixes name collisions (`<name>-2`, etc.), so the phantom OMP-
spawned claude-channel registers silently as `cc.<owner>.<name>-2` rather
than crashing.

The rig avoids this by:

1. Not baking any `.mcp.json` into `/workspace`. claude is launched in
   **plugin-mode** — the `nats-channel@sesh-channels` plugin (pre-installed
   into `~/.claude/plugins/` at image-build time) provides the MCP server,
   so no project-level config file is needed at all.
2. Setting `NATS_CHANNEL_STRICT=1` in the plugin's MCP server env (declared
   in `claude-nats-channel/plugin.json` / the plugin manifest, not in the
   rig). If a duplicate registration ever does happen — operator
   misconfiguration, future regression — the second instance fails loudly
   with exit code 2 rather than registering as a phantom `cc-2`.

When co-locating claude + OMP in a real workspace, apply the same two
rules: prefer plugin-mode (or `--mcp-config` outside the workspace) for
the claude MCP config, and keep strict-mode on. The strict-mode flag is
documented in `sesh-channels/claude-nats-channel/README.md#strict-mode`.
For operator-facing notes on the underlying OMP feature gap (no exclusion
mechanism for `mcp.discoveryMode`), see
[`docs/upstream-notes-omp-mcp-discovery.md`](../../docs/upstream-notes-omp-mcp-discovery.md).

## Claude Code channel-enablement (F1 workaround)

The rig launches `claude` with `--dangerously-load-development-channels
plugin:nats-channel@sesh-channels`. The `plugin:<name>@<marketplace>` form
opts the plugin's MCP server into Claude Code's *channel* gate (inbound-push
enabled) instead of treating it as a plain tool provider. Without the
channel flag, `notifications/claude/channel` from the channel server are
silently dropped by claude-code, and case 03/05/06 hang.

This is the exact production invocation the operator runs:

```sh
sesh up --session=<label> --role=<role> \
  --exec 'claude --dangerously-skip-permissions \
                 --dangerously-load-development-channels plugin:nats-channel@sesh-channels'
```

The plugin itself is pre-registered into `~/.claude/plugins/` at image-build
time. See **Plugin-mode install** below for the on-disk shape.

The rig auto-feeds one `2\n` input over the claude FIFO to dismiss the
Loading-Development-Channels warning dialog (~10 s after launch). The
older Bypass-Permissions warning dialog is dismissed at the source via
`skipDangerousModePermissionPrompt: true` in
`/home/integ/.claude/settings.json` (see F4.3 below).

To debug the channel gate, set `RIG_DEBUG_MCP=1` in the rig's environment;
`--debug=mcp` is then appended to the claude invocation and the relevant
gate decisions appear in `/var/log/claude.log`.

## Claude Code unattended-mode workarounds (F4)

The rig works around four real Claude Code 2.1.126 ergonomic gaps for
containerized / unattended runs. Each is documented here so future operators
don't rediscover them.

### F4.1 — Non-root user required

`claude --dangerously-skip-permissions` refuses to run when `geteuid() == 0`.
The Dockerfile creates a non-root `integ` user (uid 1500) and runs the
entrypoint as that user. See `Dockerfile` (`useradd -ms /bin/bash -u 1500 integ`).

### F4.2 — `.mcp.json` auto-discovery dialog

A project-local `.mcp.json` triggers a "New MCP server found" 1/2/3 trust
dialog at first sight, even with `--dangerously-skip-permissions`. The rig
sidesteps this entirely by using **plugin-mode** — the MCP server is
declared inside the `nats-channel@sesh-channels` plugin manifest and
loaded by Claude Code as part of plugin activation. No project `.mcp.json`,
no `--mcp-config` flag. See **Plugin-mode install** below.

### F4.3 — Bypass-Permissions warning dialog

claude-code renders `WARNING: Claude Code running in Bypass Permissions mode`
at first run. The managed-settings key
`skipDangerousModePermissionPrompt: true` dismisses it at the source.
The rig copies `test/integration/config/claude-settings.json` to
`/home/integ/.claude/settings.json` to enable this. Prior to F4 the rig
fed a timed `2\n` into the claude FIFO ~6 s after startup; the
managed-setting removes the dialog and the timed feed.

### F4.4 — Non-TTY stdin behavior

claude under non-TTY stdin (containerized stdout redirect) behaves
inconsistently with `--print` + `--dangerously-skip-permissions`. The rig
wraps claude with `script -qfc <cmd> /dev/null` (Linux PTY wrapper) so
claude thinks it's running on a TTY. See the `claude` launch in
`entrypoint.sh`.

### Dev-channels warning dialog (covered by F1, not F4)

`--dangerously-load-development-channels` (required by F1) raises a
separate "Loading development channels" dialog. The rig auto-feeds `2\n`
to dismiss it ~10 s after startup. See F1's plan for context. There is
currently no managed-settings equivalent to dismiss this dialog at the
source; the timed feed is the only known mechanism.

## Plugin-mode install

The rig matches the operator's production invocation shape:

```sh
claude --dangerously-skip-permissions \
       --dangerously-load-development-channels plugin:nats-channel@sesh-channels
```

For `plugin:nats-channel@sesh-channels` to resolve, two pieces of state must
exist on disk before `claude` launches:

1. **A `directory`-source marketplace** named `sesh-channels`, pointing at
   the sesh-channels checkout (the `.claude-plugin/marketplace.json` inside
   that directory declares the `nats-channel` plugin).
2. **An installed plugin entry** for `nats-channel@sesh-channels`, user-
   scoped, pointing at the plugin's source directory.

Operators register these by running:

```sh
claude plugin marketplace add ~/projects/sesh-channels
claude plugin install nats-channel@sesh-channels -s user
```

Both commands prompt for confirmation today, so they can't run unattended
inside a Dockerfile build. The rig instead **pre-bakes the equivalent
on-disk state** by COPYing three JSONs into `/home/integ/.claude/plugins/`:

- `known_marketplaces.json` — declares the `sesh-channels` marketplace,
  source `directory: /opt/sesh-channels`.
- `installed_plugins.json` — declares the `nats-channel@sesh-channels`
  install, user-scoped, `installPath: /opt/sesh-channels/claude-nats-channel`.
- `config.json` — the (empty) `repositories` map, present for completeness
  so claude doesn't re-write it on first launch.

These files are the on-disk surface claude-code reads at startup; the
shape mirrors the operator's `~/.claude/plugins/` exactly (verified by
direct inspection). The bake happens during `docker build` so the image
is self-contained — no install at runtime.

If a future claude-code release adds a non-interactive flag to
`claude plugin install` (`-y`, `--yes`, etc.), the rig can switch to the
canonical CLI path — just add the install command to the Dockerfile and
drop the JSON COPYs. Until then, the JSON-bake is the only deterministic
path.

The Dockerfile copies `/opt/sesh-channels/.claude-plugin/marketplace.json`
in alongside the adapter source so the marketplace pointer resolves. See
`Dockerfile` block "Plugin-mode install for claude-nats-channel" for the
exact COPY ordering.
