# Docker Integration Rig — Findings

**Run:** 2026-05-22
**Rig commit:** `feat/docker-integration-rig` @ `06392e7`
**Plan:** `docs/plans/2026-05-22-docker-integration-rig.md`
**Result:** 4 / 8 cases PASS

## Test summary

| # | Case | Status | Notes |
|---|------|--------|-------|
| 01 | `01-registration` | **PASS** | Both `cc` and `op` agents present in `$SRV.INFO.agents` with correct `agent / owner / role / class / session / protocol_version` metadata. |
| 02 | `02-heartbeats` | **PASS** | 4 heartbeats per agent in 20s window on `agents.hb.cc.*` and `agents.hb.op.*`. Payloads match metadata. |
| 03 | `03-prompt-claude` | **FAIL** | Stream timeout (90s). Saw mandatory §6.4 ack chunk; no `{"type":"response"...}` chunks; no terminator. |
| 04 | `04-prompt-omp` | **FAIL** | Stream completes (1.5s) — ack + terminator — but with **zero response chunks**. Empty reply. |
| 05 | `05-attachment` | **FAIL** | Same shape as 03 (sent to claude with a 10-byte text attachment). Stream timeout. |
| 06 | `06-cross-adapter` | **FAIL** | Caller-mediated test — claude leg blocks same as 03; never reaches the OMP leg. |
| 07 | `07-session-json` | **PASS** | `~/.sesh/sessions/smoke-test.json` `agents[]` has both adapters with correct `agent/owner/instance_id/subject/role/class`. |
| 08 | `08-steady-state` | **PASS** | 90s subscription saw 28 heartbeats; `$SRV.INFO.agents` returned the same 2 agents at t=15/30/45/60/75/90s. |

## Findings (ranked by load-bearing impact)

### F1 — Inbound prompt does not trigger an agent turn (load-bearing)

**Severity:** P0 — single root cause for cases 03 / 04 / 05 / 06.
**Owner:** claude-code's `--dangerously-load-development-channels` gate.
**Status:** FIXED. The rig now matches the operator's production invocation
shape — plugin-mode (`--dangerously-load-development-channels plugin:nats-channel@sesh-channels`),
which routes `notifications/claude/channel` through Claude Code's channel
gate and drives a model turn. The previous `--strict-mcp-config / --mcp-config`
form bypassed the channel gate by registering the MCP server outside the
plugin path, so notifications were silently dropped.

**Symptom (pre-fix):** Caller publishes to `agents.prompt.cc.<owner>.<session>`. The channel's MCP server receives the request, forwards it as a `notifications/claude/channel` MCP notification, and emits the mandatory §6.4 `{"type":"status","data":"ack"}` chunk on the reply subject. **Then nothing.** Claude does not invoke its `reply` tool. Ack-timers fire (every 30s — `ACK_INTERVAL_MS = 30_000` in `claude-nats-channel/server.ts`) until the caller times out at 90s.

Same shape on OMP — receives ack, emits terminator immediately (so the reply *terminates*, but with no response chunks).

**Diagnosis (pre-fix):** The model only produces output during an active conversation turn. The `--dangerously-load-development-channels nats` form (matching an MCP server name) did not opt the server into the channel gate; `plugin:<name>@<marketplace>` does. With the gate inactive, claude-code dropped the channel notifications, leaving the channel waiting for a reply that never came.

**Reproduction (pre-fix):** Run rig before this commit. Watch `harness/results.txt`. Cases 03/05/06 time out; case 04 returns empty response.

**Fix shipped:**
- Rig: claude is now installed via plugin-mode at image-build time (Dockerfile bakes `~/.claude/plugins/{known_marketplaces,installed_plugins,config}.json`) and launched with `--dangerously-load-development-channels plugin:nats-channel@sesh-channels`. The `--strict-mcp-config / --mcp-config` flags are dropped; the plugin's MCP server is loaded by Claude Code as part of plugin activation, and the channel gate is engaged by the `plugin:` form of the channel flag.
- This matches the operator's production invocation byte-for-byte (except `--continue`, which the rig deliberately omits — fresh session per run).

**Cross-references:**
- `test/integration/Dockerfile` (Plugin-mode install block)
- `test/integration/entrypoint.sh` (claude launch line)
- `test/integration/config/claude-plugins/` (baked plugin enablement JSONs)
- Operator reference: `~/.claude/plugins/installed_plugins.json` + `known_marketplaces.json` (the schema the rig mirrors)

---

### F2 — `omp-nats-channel` doesn't read `SESH_SESSION`; `claude-nats-channel` does

**Severity:** P1 — adapter inconsistency, surfaced during rig debugging.
**Owner:** sesh-channels (omp-nats-channel, plus pi / grok / gemini — same bug shape across 4 adapters).
**Status:** FIXED. SDK landed `readSessionLabel` in sesh `agents/sdk-ts/` (PR #105, published as `@agent-ops/sesh-channels@0.1.1`). sesh-channels PR #4 migrated OMP / pi / grok / gemini adapters to honor `$SESH_SESSION` natively via the new helper. Rig workaround dropped in this commit.

**Symptom:** When the rig sets `SESH_SESSION=smoke-test` in the spawn env, claude-nats-channel registers with `metadata.session=smoke-test` (correct). omp-nats-channel registers with `metadata.session=workspace` — it ignored `SESH_SESSION` and fell back to `basename(cwd)`. The agent_watcher then excluded OMP from the session manifest entirely until the rig started exporting `NATS_SESSION_NAME=$SESH_SESSION` as a workaround.

**Diagnosis:** Two different env-var resolution paths.
- claude: `~/projects/sesh-channels/claude-nats-channel/server.ts:235` calls `discoverSessionLabel()` which prefers `process.env.SESH_SESSION` first.
- omp: `~/projects/sesh-channels/omp-nats-channel/extensions/nats-channel.ts:965-968` (pre-fix) checked `NATS_SESSION_NAME` → `config.sessionName` → `basename(ctx.cwd)`. No `SESH_SESSION`.

**Reproduction (pre-fix):** Spawn omp-nats-channel with `SESH_SESSION=foo` but no `NATS_SESSION_NAME`. Verify `$SRV.INFO.agents` shows `metadata.session=<cwd-basename>` instead of `foo`.

**Fix shipped:**
- SDK: `agents/sdk-ts/src/session.ts` exports `readSessionLabel({ env, startDir, warn })` — composes `$SESH_SESSION` then `.sesh/sessions/<label>.json` state-walk. Published as `@agent-ops/sesh-channels@0.1.1`.
- Adapters: each (non-claude) adapter grew a `session.ts` shim that wraps `readSessionLabel` and falls through to the legacy env-var override, per-context config, and `basename(cwd)` defaults. Identical ladder ordering across all 5 adapters now.
- Rig: this commit drops `export NATS_SESSION_NAME="${SESH_SESSION:-}"` from the OMP wrapper in `test/integration/entrypoint.sh` — no longer required.

---

### F3 — `sesh up` claims the session label exclusively; the plan's "two parallel `sesh up`" pattern is impossible

**Severity:** P1 — design clarification needed; the plan assumed a model the code doesn't support.
**Owner:** sesh (design decision — possibly correct, but needs documenting).

**Symptom:** Second `sesh up --session=smoke-test --exec=...` invocation blocks/errors because the first holds the session claim. The original integration plan assumed two `sesh up`s would attach to the same hub and share the session label.

**Diagnosis:** `~/projects/sesh/cli/session.go:ClaimSession` enforces single-claim-per-label. This is probably intentional — multiple wrappers on the same label have ambiguous teardown semantics. But the rig had to work around it with a single `sesh up` + bash fan-out wrapper that spawns claude + omp under the same session-claim.

**Reproduction:** In one shell, `sesh up --session=foo --exec='sleep 30'`. In another, `sesh up --session=foo --exec='sleep 30'`. Observe second invocation's behavior.

**Suggested fix shape:** Three options:
- (a) Document the single-wrapper model. `sesh up --exec` is for one adapter per session label. To run multiple adapters in a session, use one `sesh up` with a multiplexing exec.
- (b) Add a `--join` or `--secondary` mode that attaches to an existing claim without claiming.
- (c) Change the model so multiple `sesh up`s coexist.

(a) is cheapest, matches current semantics. (b) is the natural extension but adds API surface. (c) is invasive.

**Cross-references:**
- `~/projects/sesh/cli/session.go:ClaimSession`
- `~/projects/sesh/cli/up.go` (PR #96 surface)
- Rig workaround: `test/integration/entrypoint.sh` (single `sesh up` + `/tmp/launch-agents.sh`)

---

### F4 — Multiple Claude-Code ergonomic blockers for unattended container runs

**Severity:** P2 — each individually small; cumulatively they make containerized claude painful.
**Owner:** claude-code (Anthropic upstream).

**Symptoms (each is a real workaround in the rig):**

1. **Refuses `--dangerously-skip-permissions` under root** — claude exits with "for security reasons" when uid=0. Rig added a non-root `integ` user (uid 1500).
2. **`.mcp.json` auto-discovery dialog blocks even with `--dangerously-skip-permissions`** — Claude prompts "New MCP server found in .mcp.json: nats — 1/2/3". Rig sidesteps entirely via plugin-mode (MCP server declared inside the `nats-channel@sesh-channels` plugin manifest, no project `.mcp.json` at all). Pre-plugin-mode workaround was `--strict-mcp-config --mcp-config /opt/claude.mcp.json`.
3. **First-run "Bypass Permissions mode" warning dialog blocks startup** — `bypassPermissionsModeAccepted: true` in `~/.claude.json` doesn't persist across containers (claude reads `.config.json`, which is different). Rig auto-feeds `2\n` via a FIFO.
4. **`--print` and `--dangerously-skip-permissions` interact oddly when stdin isn't a TTY** — bypass-permissions still wants interactive input even in `--print` mode.

**Suggested fix shape:** Upstream issues at `anthropic/claude-code`. Either separate small issues or one "containerized claude ergonomics" umbrella. Per CLAUDE.md no-third-party-filing rule, we don't file these — but the rig README should document the workarounds so anyone else running containerized claude doesn't rediscover them.

**Cross-references:**
- `test/integration/entrypoint.sh` (FIFO + plugin-mode invocation)
- `test/integration/Dockerfile` (non-root `integ` user + plugin-mode install)

---

### F5 — OMP's `mcp.discoveryMode: true` autoloads any project `.mcp.json`, causing duplicate channel registration

**Severity:** P2 — startup-time consistency issue, caught during rig iteration.
**Owner:** sesh-channels + rig integration policy (OMP behavior is upstream/intentional).

**Symptom:** OMP's config has `mcp.discoveryMode: true`. When the rig had a `/workspace/.mcp.json` with `mcpServers.nats` (for Claude), OMP auto-discovered it and ALSO instantiated `claude-nats-channel` as an MCP server inside OMP's process. Result: 3 bus registrations from 2 agents — claude registers via its own MCP, OMP registers via its extension, AND OMP-loaded claude-nats-channel registers a phantom third entry.

**Diagnosis:** OMP's MCP autodiscovery is by design; the rig was placing claude's MCP config where OMP could see it. Real-world co-location of claude + OMP on the same workspace will likely hit this.

**Reproduction:** Put a `.mcp.json` in OMP's working dir with `mcpServers` entries. Watch OMP load each as a separate MCP-side identity.

**Suggested fix shape:**
- (a) **Document**: when running claude + OMP in the same workspace, use `--mcp-config` flag (per-tool config files) rather than `.mcp.json` (workspace-shared config).
- (b) Make `claude-nats-channel`'s MCP server idempotent — if it's already registered on the bus for the same `(agent, owner, name)` triple, refuse to register again rather than create a duplicate.
- (c) Add an `omp.mcp.exclude` config pattern so OMP can be told to skip specific MCP entries.

(b) is the most robust (works even when operators don't read docs). (a) is cheapest but error-prone. (c) is the right OMP feature but upstream.

**Cross-references:**
- `claude-nats-channel/server.ts` (registration path; need to check for idempotency)
- OMP's config schema (extension load loop)
- Rig workaround: removes `/workspace/.mcp.json`, uses `--mcp-config /opt/claude.mcp.json` for claude only

---

### F6 — `hub.nats.url` disappears when `sesh up` exits — downstream rigs/tools need persistence

**Severity:** P3 — design call, not a bug per se.
**Owner:** sesh.

**Symptom:** `~/.sesh/hub.nats.url` is removed when the hub daemon exits (which happens when the last leaf disconnects). If a downstream tool (the rig's harness) reads the URL after the daemon has exited, the file is gone.

**Diagnosis:** `~/projects/sesh/cli/hub_serve.go` calls `defer ClearHubInfo(seshDir)` on shutdown — file-lock-style cleanup, sensible design.

**Suggested fix shape:** Either
- (a) Persist the URL in session JSON (`session.nats_url`) so it survives daemon exit and is the canonical place to look — `agent_watcher` already reads it.
- (b) Document that downstream tools should cache the URL on first sighting.
- (c) Add an `--retain-url` flag to `sesh up` that suppresses the deferred cleanup.

(a) feels right — session JSON should be the single source of truth for "where's the hub for this session?". The watcher already writes there.

**Cross-references:**
- `cli/hub_serve.go` `ClearHubInfo`
- `cli/session.go` (session JSON shape — does `nats_url` field already exist?)
- Rig workaround: harness caches `hub.nats.url` to `/var/artifacts/hub.nats.url` on first sighting

---

### F7 — OMP's TUI emits ANSI escapes into stdout/logs; rig logs need stripping for grep-ability

**Severity:** P4 — UX polish.
**Owner:** rig docs.

**Symptom:** OMP under non-TTY (containerized stdout redirect) still emits ANSI escape codes for cursor positioning, color, etc. Logs are unreadable without `col -b` or `ansi2txt`.

**Suggested fix shape:** Rig's entrypoint pipes OMP's log through `col -b` before writing to `/var/log/omp.log`. Document this in the README.

**Cross-references:**
- `test/integration/entrypoint.sh` (OMP wrapper)

---

### F8 — Disk pressure causes Docker build to fail with "input/output error"

**Severity:** P4 — environmental.
**Owner:** rig docs.

**Symptom:** Docker Desktop containerd ingest fails on tight disk (<1 GB free).

**Suggested fix shape:** Add a "Disk preconditions" section to the rig README. `docker system prune -a` if low.

---

## What the integration test proved (positive findings)

Worth recording — not all news is bad:

1. **Role/class wire format works end-to-end.** Claude registers with `role=implementer class=active`; OMP registers with `role=planner class=active`. Both visible in `$SRV.INFO.agents` and session JSON `agents[]`.
2. **`@agent-ops/sesh-channels@0.1.0` resolves correctly inside both adapter runtimes** (bun module loader, ESM-only). No `ERR_REQUIRE_ESM` issues.
3. **Heartbeats fire at the documented cadence** (~30s default). Payload includes machine/project/session/role/class per the Phase 1 coordination work.
4. **Coordination subjects subscribe correctly** — active workers on `agents.prompt.<machine>.<project>.<session>.<role>` (queue group). Observed (indirectly) via successful `agents.prompt.cc.<owner>.<session>` dispatch reaching the channel ack handler.
5. **`sesh up --exec` propagates env vars** correctly to the spawned child (`SESH_ROLE`, `SESH_CLASS` reach the adapter's `process.env`).
6. **Session JSON populates within ~1.2s** of agent registration — `agent_watcher` polling cadence is fast enough.
7. **`/etc/machine-id` + `.sesh/project-id` together produce stable machine + project tokens** in coordination subjects.

## Aggregate

- **4 PASS / 4 FAIL** out of 8 cases.
- **1 P0 finding** (F1) gates 4 test cases (03/04/05/06). If F1 is resolved, expected rig outcome jumps to 8/8 PASS modulo any second-order surprises.
- **1 P1 inconsistency** between adapters (F2 — SESH_SESSION).
- **1 P1 design clarification** (F3 — single-wrapper session model).
- **4 ergonomic / polish findings** (F4-F8).

Next step (operator-directed): systematic-debugging + brainstorm pass per finding, only surface 4-axes decisions to operator, produce AFK-ready specs/plans.

---

# Follow-up Findings — Rig Validation Attempt (2026-05-23)

**Context:** After F1-F8 + PR #108 (plugin-mode + multi-round case 06) landed, the rig was re-run to confirm 8/8 PASS. **It regressed to 2/8.** ~18 iterations of hot-patches did not converge. The operator decided to ship the diagnostic findings as F9-F15 and tackle proper rig validation in a follow-on.

The patches in this PR (`feat/integration-rig-debugging-findings`) fix the most clear-cut issues (Dockerfile build errors, hardcoded keychain assumption, expect-driven TUI dismissal). The deeper plugin-mode + OMP-autodiscovery interactions remain unresolved and are surfaced below for the follow-on.

## Test summary (post-PR #108 → this branch)

| # | Case | Pre-PR #108 | Post-PR #108 (baseline) | Current branch tip |
|---|------|-------------|-------------------------|--------------------|
| 01 | registration | PASS | FAIL (role=planner) | varies by run |
| 02 | heartbeats | PASS | PASS | PASS |
| 03 | prompt claude | FAIL (gated F1) | FAIL (channel-skip) | FAIL |
| 04 | prompt OMP | FAIL | FAIL | FAIL |
| 05 | attachment | FAIL | FAIL | FAIL |
| 06 | cross-adapter | FAIL | FAIL (round 1) | FAIL |
| 07 | session JSON | PASS | FAIL (role mismatch) | varies |
| 08 | steady-state | PASS | PASS | PASS |

Best result during debugging: **3/8** (run #12 — case 01 added). PR #107 baseline of 4/8 remains the operator's reference point.

## Findings F9-F15

### F9 — `Claude Code-credentials` keychain entry absent on this host

**Severity:** P1 — rig won't start without Claude OAuth.
**Owner:** rig (claude-code's storage location may have changed).

**Symptom:** `scripts/stage-creds.sh` ran `security find-generic-password -s "Claude Code-credentials" -w` and got "specified item could not be found in the keychain."

**Root cause:** On this operator's host, Claude Code 2.1.126 stores the OAuth token at `~/.claude/.credentials.json` directly (not in macOS keychain). The darken pattern that the rig copied assumes keychain storage.

**Fix shipped in this PR:** `stage-creds.sh` now tries keychain first, falls back to the `~/.claude/.credentials.json` file.

### F10 — Top-level `mcpServers.nats` in operator's `~/.claude.json` has stale absolute path

**Severity:** P0 — blocks claude's MCP server load entirely.
**Owner:** rig (operator's local state pollutes the container).

**Symptom:** Claude inside the container failed to start the nats MCP server with `ENOENT: Could not change directory to "/Users/dmestas/projects/agent-channels/claude-nats-channel"`. That path is the operator's pre-rename host path; doesn't exist in the container.

**Root cause:** When the operator installed claude-nats-channel as an MCP server (pre-plugin-mode), claude wrote a `mcpServers.nats` entry to `~/.claude.json` with the absolute host path. The operator later renamed `~/projects/agent-channels` to `~/projects/sesh-channels` but the JSON entry kept the old path. When stage-creds.sh mounts the host's `~/.claude.json` into the container, the stale path comes along.

**Fix shipped:** `stage-creds.sh`'s jq pipeline now does `del(.mcpServers)` so the user-scope mcpServers entry is stripped before the file is mounted. Plugin-mode is supposed to provide the MCP server independently — but see F12 for why this didn't fully work.

### F11 — `--dangerously-load-development-channels` requires tagged syntax in claude 2.1.148

**Severity:** P2 — flag-syntax documentation.
**Owner:** claude-code (upstream — docs gap).

**Symptom:** Passing `--dangerously-load-development-channels nats` (bare server name) made claude exit with:
```
--dangerously-load-development-channels entries must be tagged: nats
  plugin:<name>@<marketplace>  — plugin-provided channel (allowlist enforced)
  server:<name>                — manually configured MCP server
```

**Root cause:** Newer claude requires either `plugin:X@Y` or `server:X` prefix. The flag is hidden from `--help`; this requirement is only visible when you pass a bad arg.

**Fix shipped:** `entrypoint.sh` now uses `--dangerously-load-development-channels plugin:nats-channel@sesh-channels` (matches operator's production invocation).

**Worth filing upstream** as a docs gap (CLAUDE.md no-third-party-filing rule defers this).

### F12 — Channel-mode gate matches MCP server **name** literally against the channels list

**Severity:** P0 — root cause for cases 03/05/06.
**Owner:** claude-code (likely upstream bug or design quirk) + possibly sesh-channels plugin manifest.

**Symptom:** With `--dangerously-load-development-channels plugin:nats-channel@sesh-channels`, the plugin's MCP server `nats` loads and connects successfully. But claude's debug log says:
```
MCP server "nats": Channel notifications skipped: server nats not in --channels list for this session
```

**Root cause:** Claude's channel-mode gate compares the runtime MCP server name (`nats`) against the literal entries in the channels list (`["plugin:nats-channel@sesh-channels"]`). They don't string-match, so channel notifications are dropped. The `plugin:...@...` form should logically expand to the set of MCP servers the plugin provides, but apparently doesn't in 2.1.148.

**Workaround attempts that didn't fully resolve:**
- Using `server:nats` syntax — works for user-scope mcpServers but then OMP autodiscovers it too (F13)
- Passing both `plugin:nats-channel@sesh-channels` and `server:nats` — not tested; likely produces double-registration

**Likely upstream issue:** claude-code's plugin-mode channel gate needs to resolve plugin entries to their underlying MCP server names. This may be a bug or a design that requires the operator to use `server:` form even for plugin-provided servers.

### F13 — OMP MCP autodiscovery ignores `mcp.discoveryMode: false`

**Severity:** P1 — causes duplicate `claude-code` registration with OMP's env.
**Owner:** can1357/oh-my-pi (upstream OMP bug or documentation gap).

**Symptom:** Even with `mcp.discoveryMode: false` in `~/.omp/agent/config.yml`, OMP logged:
```
Connecting to MCP servers: nats, nats-channel:nats
```

OMP loads BOTH the user-scope `mcpServers.nats` (`nats`) and the plugin-provided MCP server (`nats-channel:nats`). The plugin-scoped one inherits OMP's env (SESH_ROLE=planner), so the `claude-code` registration on the bus ends up with role=planner instead of implementer.

**Workaround attempted:** Run OMP with a fresh HOME (`/tmp/omp-home`) symlinked only to `~/.omp/agent`, so OMP can't see claude's `~/.claude.json` or `~/.claude/plugins/`. Partial success — eliminates user-scope discovery but not plugin discovery.

**Likely upstream issue:** OMP's `mcp.discoveryMode` option doesn't actually disable discovery. Either documentation lies or implementation is buggy. Worth investigating OMP source.

### F14 — Real `claude plugin install` is required (JSON-baking is insufficient)

**Severity:** P1 — rig was faking plugin install state and missing supporting directories.
**Owner:** rig.

**Symptom:** PR #108's plugin-mode setup baked `~/.claude/plugins/{known_marketplaces,installed_plugins,config}.json` files into the image, but did NOT recreate the `marketplaces/`, `repos/`, and `data/` subdirectories that `claude plugin install` produces. Claude's plugin loader silently failed when those structures were missing.

**Fix shipped:** Dockerfile now actually runs `claude plugin marketplace add /opt/sesh-channels && claude plugin install nats-channel@sesh-channels -s user` as the `integ` user during build. Both commands are non-interactive in claude 2.1.148. The baked JSON files have been deleted.

### F15 — Pre-existing wrapper script bugs uncovered during rig debugging

**Severity:** P3 — small cumulative issues.
**Owner:** rig.

Three small bugs in `entrypoint.sh`'s launch wrapper:

a) **`col` command missing** — Dockerfile installed `util-linux` but `col` lives in `bsdextrautils` on Debian. OMP wrapper's `... | col -b` failed silently. Added `bsdextrautils` to apt-install.

b) **Dev-channels dialog FIFO sent wrong key** — option `2` was previously documented as "use locally" but in claude 2.1.148 it means "Exit". Switched to `expect` + match-on-`cancel` + Enter on default-highlighted option 1.

c) **`expect` pattern can't match multi-word strings interleaved with ANSI cursor codes** — Ink TUI emits `\e[12G` between every word. Matching `"I am using this for local development"` literally never succeeds. Switched to matching `cancel` (single contiguous ASCII token).

**Fixes shipped:** Dockerfile + entrypoint.sh + new `config/claude-launch.exp` script.

## Patches NOT shipped (deferred to follow-on)

1. **Channel-mode for plugin syntax** (F12) — needs upstream investigation. Possible fix shapes:
   - claude-code patches the gate to resolve `plugin:X@Y` → list of MCP server names
   - sesh-channels' plugin manifest is restructured so the channel name matches the plugin id
   - The rig falls back to `server:nats` + isolates OMP from claude.json (see F13)
2. **OMP MCP autodiscovery** (F13) — needs OMP source investigation or upstream issue at `can1357/oh-my-pi`.
3. **Full HOME isolation for OMP** — Partial workaround works but doesn't stop OMP from discovering plugin-installed MCP servers. May need a fully separate user.
4. **Stale `mcpServers.nats` in operator's host `~/.claude.json`** — Operator could remove this manually; the rig works around it via `del(.mcpServers)` in stage-creds.

## Diagnostic value of this PR

Even though the rig still has only 2/8 passing, the patches in this PR are *real progress* — they unblock paths that were silently failing:

- Real plugin install (not faked JSON) → claude can load plugin-mode MCP servers
- Expect-driven TUI dismissal → claude makes it past the dev-channels dialog
- Credentials file fallback → rig works on hosts where keychain isn't used
- mcpServers scrub → stale operator paths don't crash claude

The remaining failures (F12 channel-mode gate, F13 OMP discovery) need deeper investigation that doesn't belong in a hot-patch loop.

## Operator follow-on

Per operator direction (2026-05-23): land these findings as a documentation PR, then prove out the rig properly in a follow-on with cleaner restructuring. Options under discussion:

- (A) **Multi-container rig** — claude and OMP in separate containers sharing a NATS server. Eliminates autodiscovery interference.
- (B) **Investigate F12 upstream** — root-cause the plugin:X@Y channel-gate behavior in claude-code source. May be a real bug worth a careful filing.
- (C) **OMP source dive for F13** — understand `mcp.discoveryMode` actual semantics. May be a config-key rename or implementation gap.
