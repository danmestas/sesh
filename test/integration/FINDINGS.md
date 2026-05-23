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
**Owner:** sesh-channels (most likely) or claude-code product behavior.

**Symptom:** Caller publishes to `agents.prompt.cc.<owner>.<session>`. The channel's MCP server receives the request, forwards it as an `notifications/claude/channel` MCP notification, and emits the mandatory §6.4 `{"type":"status","data":"ack"}` chunk on the reply subject. **Then nothing.** Claude does not invoke its `reply` tool. Ack-timers fire (every 30s — `ACK_INTERVAL_MS = 30_000` in `claude-nats-channel/server.ts`) until the caller times out at 90s.

Same shape on OMP — receives ack, emits terminator immediately (so the reply *terminates*, but with no response chunks).

**Diagnosis:** The model only produces output during an active conversation turn. Claude and OMP sitting idle in their TUIs receive the channel notification but don't start a turn. `--dangerously-skip-permissions` auto-accepts *tool calls*; a channel notification isn't a tool call — it's an inbound prompt waiting for the model to begin a turn.

Both `claude-nats-channel/server.ts:587-589` and the channel README mention experimental MCP capabilities `'claude/channel'` + `'claude/channel/permission'` that *should* drive auto-processing, but they don't in our setup.

**Reproduction:** Run rig as-is. Watch `harness/results.txt`. Cases 03/05/06 time out; case 04 returns empty response.

**Suggested fix shape (incomplete — needs brainstorm):** Either (a) the channel synthesizes a user-turn when an inbound prompt arrives, (b) the channel's MCP capability handshake actually does drive a turn and we're misusing it, or (c) the operator setup needs a documented prerequisite (background daemon / persistent active state). Investigation needed.

**Cross-references:**
- `~/projects/sesh-channels/claude-nats-channel/server.ts` lines 580-620 (capabilities handshake), 850+ (notification dispatch), `ACK_INTERVAL_MS` constant
- `~/projects/sesh-channels/omp-nats-channel/extensions/nats-channel.ts` analogous notification path

---

### F2 — `omp-nats-channel` doesn't read `SESH_SESSION`; `claude-nats-channel` does

**Severity:** P1 — adapter inconsistency, surfaced during rig debugging.
**Owner:** sesh-channels (omp-nats-channel).

**Symptom:** When the rig sets `SESH_SESSION=smoke-test` in the spawn env, claude-nats-channel registers with `metadata.session=smoke-test` (correct). omp-nats-channel registers with `metadata.session=workspace` — it ignored `SESH_SESSION` and fell back to `basename(cwd)`. The agent_watcher then excluded OMP from the session manifest entirely until the rig started exporting `NATS_SESSION_NAME=$SESH_SESSION` as a workaround.

**Diagnosis:** Two different env-var resolution paths.
- claude: `~/projects/sesh-channels/claude-nats-channel/server.ts:235` calls `discoverSessionLabel()` which prefers `process.env.SESH_SESSION` first.
- omp: `~/projects/sesh-channels/omp-nats-channel/extensions/nats-channel.ts:965-968` checks `NATS_SESSION_NAME` → `config.sessionName` → `basename(ctx.cwd)`. No `SESH_SESSION`.

**Reproduction:** Spawn omp-nats-channel with `SESH_SESSION=foo` but no `NATS_SESSION_NAME`. Verify `$SRV.INFO.agents` shows `metadata.session=<cwd-basename>` instead of `foo`.

**Suggested fix shape:** Import or reimplement `discoverSessionLabel` in omp-nats-channel so the env-var resolution order is identical across all 5 adapters. Pattern probably belongs in `@agent-ops/sesh-channels` as `readSessionLabel()` so all adapters share one implementation (matches the role/class consolidation already shipped).

**Cross-references:**
- `claude-nats-channel/server.ts:235` (correct)
- `omp-nats-channel/extensions/nats-channel.ts:965-968` (incomplete)
- Workaround in `test/integration/entrypoint.sh` (the OMP wrapper exports `NATS_SESSION_NAME`)

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
2. **`.mcp.json` auto-discovery dialog blocks even with `--dangerously-skip-permissions`** — Claude prompts "New MCP server found in .mcp.json: nats — 1/2/3". Workaround: `--strict-mcp-config --mcp-config /opt/claude.mcp.json` and no project `.mcp.json`.
3. **First-run "Bypass Permissions mode" warning dialog blocks startup** — `bypassPermissionsModeAccepted: true` in `~/.claude.json` doesn't persist across containers (claude reads `.config.json`, which is different). Rig auto-feeds `2\n` via a FIFO.
4. **`--print` and `--dangerously-skip-permissions` interact oddly when stdin isn't a TTY** — bypass-permissions still wants interactive input even in `--print` mode.

**Suggested fix shape:** Upstream issues at `anthropic/claude-code`. Either separate small issues or one "containerized claude ergonomics" umbrella. Per CLAUDE.md no-third-party-filing rule, we don't file these — but the rig README should document the workarounds so anyone else running containerized claude doesn't rediscover them.

**Cross-references:**
- `test/integration/entrypoint.sh` (FIFO + `--strict-mcp-config` workarounds)
- `test/integration/Dockerfile` (non-root `integ` user)

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
