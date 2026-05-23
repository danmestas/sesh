# Docker Integration Rig — sesh + sesh-channels End-to-End

**Date:** 2026-05-22
**Status:** Planning
**Author:** dmestas (drafted with claude-code assistance)

## Goal

Spin up a containerized environment that recreates a fresh user install of **sesh + sesh-channels + Claude Code + Oh My Pi**, drive both adapters into a single sesh session via `sesh up --exec`, and verify:

1. Both agents register on the bus with correct metadata (including `role` / `class`)
2. Heartbeats fire per Synadia §8.2
3. Prompts can be sent to each agent and replies stream back
4. Attachments survive the round-trip
5. The two agents can prompt each other (inter-agent mesh talk)
6. sesh's session manifest's `agents[]` populates correctly
7. The hub holds together under steady-state load (60s+)

Where things break, **diagnose root cause first, then file issues** per CLAUDE.md upstream-investigation discipline.

## Spec under test

The integration rig exercises code from all four merged workstreams:

| Subsystem | Recent merge | What we're validating |
|---|---|---|
| **sesh role/class core** | sesh#90 (`f9ffba48`) | `internal/agentmeta`, `Config.Role`/`Class`, `metadata.role`/`class` emission, agent_watcher parse, session JSON shape |
| **sesh up --exec** | sesh#96 (`e8b8922`) | Adapter spawn with role/class flag/env propagation, lifecycle |
| **orch-spawn SESH_* exports** | orch#190 (`b3ead334`) | Out of scope for this rig (orch isn't installed in container — keeps the test surface tight) |
| **`@agent-ops/sesh-channels` SDK** | sesh#97 (`4df641c`), npm `0.1.0` | `readRoleClass`, `readAdapterConfig`, ESM-only resolution |
| **sesh-channels adapter migration** | sesh-channels#1 (`e5f10e6d`) | All 5 adapter `config.ts` import + use the SDK; bun-test green per adapter |
| **parallel coordination subjects** | sesh#91, #94 | If these add `sesh.*` registration on the bus, observe them too |

## Architecture

### Container layout

Single privileged container (simpler than docker-compose for an initial integration loop). One image, multiple processes:

```
docker container "sesh-integration"
├── PID 1: tini (signal forwarding)
│   ├── Process 1: sesh hub (started by `sesh up smoke-test`)
│   │   └── embeds NATS server on a chosen port
│   ├── Process 2: Claude Code (started via `sesh up --exec`)
│   │   └── subprocess: bun run /opt/sesh-channels/claude-nats-channel/server.ts  ← the MCP server
│   ├── Process 3: Oh My Pi (started via `sesh up --exec`)
│   │   └── extension: /opt/sesh-channels/omp-nats-channel  ← the channel
│   └── Process 4: integration test harness (TS, runs after a settle period)
└── volume mount: ${HOST}/test-artifacts:/var/sesh/artifacts
    (session.json, hub logs, adapter logs, test output go here for post-run inspection)
```

If single-container proves too cramped (PID 1 issues with tini + multiple long-running children, or sesh hub conflicting with adapter port assumptions), fall back to docker-compose with three services:
- `hub` (sesh)
- `claude-host` (Claude Code + claude-nats-channel)
- `omp-host` (Oh My Pi + omp-nats-channel)

All sharing a common network so NATS URL discovery via `nats://hub:4222` works.

### Image

Multistage Dockerfile:

- **Stage 1 — sesh-builder:** `golang:1.26-bookworm`. `git clone github.com/danmestas/sesh`, `go build -o /out/sesh ./cmd/sesh` (or wherever its main is — Recon Task 1).
- **Stage 2 — runtime:** `oven/bun:1.2-debian`. Install Node 20 alongside Bun. Copy sesh binary from Stage 1. Install Claude Code (`npm i -g @anthropic-ai/claude-code` if that's the package name — Recon Task 4) and Oh My Pi (Recon Task 5). Clone or `npm i` sesh-channels (TBD: copy via `git clone github.com/danmestas/sesh-channels` for v1; later switch to per-adapter npm installs once they're published). Set up `~/.claude/`, `~/.omp/`, `~/.sesh/` skeletons with the right MCP / extension config files.

### Networking

- sesh starts its embedded NATS on `nats://127.0.0.1:<random>` and writes the URL to `~/.sesh/hub.url` (per current sesh behavior)
- All adapters read `~/.sesh/hub.url` for the NATS URL
- The integration test harness reads the same file to connect

### Test harness

A standalone TS program (`test/integration/harness.ts`) using `@nats-io/transport-node` + `@synadia-ai/agents` (the caller SDK). Runs **inside the container** after a 5s settle period. Reads `~/.sesh/hub.url`, connects to NATS, runs the assertion suite.

## Reconnaissance (do BEFORE writing the Dockerfile)

This is unavoidable scout-work. Each item is a 5-minute task; together they shape the rig.

- [ ] **Recon Task 1: Where does sesh's main live, and how do you build it?**
  - `find ~/projects/sesh -name 'main.go' | head` — Identify the entry point
  - Confirm `go build` command and output binary name
  - Note any required ENV (e.g., `CGO_ENABLED`)

- [ ] **Recon Task 2: Read sesh#96 `sesh up --exec` surface**
  - `gh pr view 96 --repo danmestas/sesh --json files`
  - Read the flag definition: does `--exec` take a single command string? An argv list? Repeatable for multiple processes? Does it bring `--role` / `--class` as separate flags or env?
  - Determine the exact CLI invocation to spawn an adapter with role/class

- [ ] **Recon Task 3: Read sesh#91/#94 coordination-subject impact on registration**
  - If adapters now emit additional metadata or subscribe to additional subjects, we want to observe them in the harness

- [ ] **Recon Task 4: Identify Claude Code CLI install path**
  - Search npm: `npm view @anthropic-ai/claude-code` (or similar — could be `claude-code-cli`, etc.)
  - Confirm the binary it puts on PATH (`claude`? `claude-code`?)
  - Confirm the MCP config file location (`~/.claude/mcp.json`?) and shape
  - Note required env: `ANTHROPIC_API_KEY` at minimum

- [ ] **Recon Task 5: Identify Oh My Pi install path**
  - Visit `github.com/oh-my-pi/pi-coding-agent` README for install instructions
  - Note required env / config: `~/.omp/agent/config.yml`
  - Note any required model-provider API keys

- [ ] **Recon Task 6: Verify the MCP config for `claude-nats-channel`**
  - Read `claude-nats-channel/server.ts` startup: does it expect any env vars beyond `SESH_ROLE`/`SESH_CLASS`/`SESH_OWNER`/`SESH_SESSION`/`NATS_URL`?
  - What's the MCP config block that points Claude Code at it? Check its README.

- [ ] **Recon Task 7: Verify OMP extension config syntax**
  - `~/.omp/agent/config.yml` shape (already glimpsed in README: `extensions: - /absolute/path/to/omp-nats-channel`)
  - Confirm OMP loads it on session start

After recon is done, write the Dockerfile.

## Rig structure

```
sesh-channels/test/integration/
├── PLAN.md                         # this doc
├── Dockerfile                      # multistage: sesh-builder + bun runtime + adapters
├── compose.yaml                    # fallback for multi-service layout (optional)
├── entrypoint.sh                   # starts sesh, waits, runs harness
├── secrets/                        # gitignored; mounted at runtime
│   ├── anthropic-key               # ANTHROPIC_API_KEY value (operator-supplied)
│   └── omp-model-key               # if OMP needs one
├── config/                         # static configs baked into the image
│   ├── claude-mcp.json             # Claude Code MCP config pointing at claude-nats-channel
│   ├── omp-config.yml              # Oh My Pi config loading omp-nats-channel
│   └── sesh-init.sh                # `sesh up smoke-test --exec ... --exec ...`
├── harness/
│   ├── harness.ts                  # test program (runs inside container)
│   ├── package.json                # @nats-io/transport-node, @synadia-ai/agents
│   ├── cases/
│   │   ├── 01-registration.ts      # $SRV.INFO.agents — 2 agents present, correct metadata
│   │   ├── 02-heartbeats.ts        # agents.hb.* — N heartbeats per agent in 30s window
│   │   ├── 03-prompt-claude.ts     # prompt claude-code, assert reply stream + terminator
│   │   ├── 04-prompt-omp.ts        # prompt omp, assert reply stream + terminator
│   │   ├── 05-attachment.ts        # prompt with attachment, assert adapter saw it
│   │   ├── 06-cross-adapter.ts     # claude prompts omp (or vice versa), reply round-trips
│   │   ├── 07-session-json.ts      # ~/.sesh/sessions/smoke-test.json has 2 agents w/ role/class
│   │   └── 08-steady-state.ts      # run heartbeat watcher for 90s, assert no drops
│   └── tsconfig.json
└── README.md                       # how to build + run the rig locally
```

## Test cases (numbered, ordered)

Each test case PASSes / FAILs with a one-line reason. Each FAIL gets a section in the post-run report.

### 01 — Registration

Connect to NATS, send `$SRV.INFO.agents`, collect replies for 2s.

- Assert: exactly 2 responses
- Assert: one has `metadata.agent === "cc"` (or `claude-code`, per claude-nats-channel's emission)
- Assert: one has `metadata.agent === "op"` (per omp-nats-channel README — "OMP registers under agent kind `op`")
- Assert: both have `metadata.role` matching what `sesh up --exec` was given
- Assert: both have `metadata.class === "active"`
- Assert: both have `metadata.session === "smoke-test"`
- Assert: both have `metadata.protocol_version === "0.3"`

### 02 — Heartbeats

Subscribe to `agents.hb.>` for 90 seconds.

- Assert: ≥ 2 heartbeats per agent (claude + omp) within the window (default §8.2 cadence is 30s; expect 2-3 per agent in 90s)
- Assert: heartbeat payload contains the same metadata as `$SRV.INFO.agents`
- Assert: subject shape is `agents.hb.<agent>.<owner>.<name>` (5-token, §8.1 v0.3 verb-first)

### 03 — Prompt Claude

Send a small text prompt via `@synadia-ai/agents`:

```ts
const a = new Agents({ nc });
const replies = a.prompt({ agent: "cc", owner: "<owner>", name: "<session>", prompt: "Reply with the single word: SUCCESS." });
for await (const chunk of replies) { ... }
```

- Assert: at least one `{"type":"status","data":"ack"}` chunk arrives first (mandatory §6.4)
- Assert: at least one `{"type":"response","data":"..."}` chunk
- Assert: response text contains "SUCCESS" (case-insensitive — claude may add punctuation)
- Assert: an empty-body, no-headers terminator arrives within 60s
- Assert: no `Nats-Service-Error-Code` header on any chunk

### 04 — Prompt OMP

Same shape as 03, but target `agent: "op"`.

- Same assertions as 03
- Note: OMP may have different prompt latency; allow 120s total budget

### 05 — Attachment delivery

Send a prompt with a small base64-encoded text attachment:

```ts
const replies = a.prompt({
  agent: "cc",
  owner, name,
  prompt: "Return the byte length of the attached file.",
  attachments: [{ filename: "test.txt", content: Buffer.from("hello sesh").toString("base64") }],
});
```

- Assert: response contains "10" somewhere (length of "hello sesh")
- If claude's `attachments_ok` metadata is `false`, this is an expected-failure case — record it as such, don't fail the suite

### 06 — Cross-adapter

Open question — depends on what "talk to each other" means in this architecture. Three interpretations:

**(a) Caller-mediated:** The harness prompts claude, claude's reply includes a directive, the harness translates that into a prompt to omp. (This is the most testable interpretation but doesn't really demonstrate "agents speaking to each other".)

**(b) MCP-tool-mediated:** Claude Code has access to a tool that lets it send a NATS request to omp. The tool would have to be exposed by `claude-nats-channel` (or some other MCP server). If the rig wires this up, claude can natively call omp.

**(c) Synadia §7 mid-stream query:** Either agent emits a `{"type":"query", "data":{...}}` chunk mid-reply to ask the other a question, gets a §7 response back.

Recon Task 8 (during rig build): determine which of (a/b/c) is actually wired up by `claude-nats-channel` / `omp-nats-channel`. **If none**, file an enhancement issue against sesh-channels titled "agents need a documented way to address each other on the mesh"; the test case becomes (a) for v1.

### 07 — Session JSON

Read `~/.sesh/sessions/smoke-test.json` from the container's host-mounted artifacts dir.

- Assert: `agents` is a JSON array of length 2
- Assert: each entry has `agent`, `owner`, `instance_id`, `subject`, `role`, `class`
- Assert: claude entry's `subject === "agents.prompt.cc.<owner>.smoke-test"`
- Assert: omp entry's `subject === "agents.prompt.op.<owner>.smoke-test"`
- Assert: role/class values match what `sesh up --exec` was given for each

### 08 — Steady-state stability

Run for 90 seconds with `agents.hb.>` subscription and a periodic `$SRV.INFO.agents` query every 15s.

- Assert: heartbeat count grows monotonically
- Assert: agent set returned by `$SRV.INFO.agents` is stable (same 2 entries throughout)
- Assert: no agent disappears and reappears (which would indicate a crash-restart cycle)

## Where I predict failure

Each prediction is a specific assertion that's likely to FAIL on first run. Together they form the punch list we'll work through:

1. **`sesh up --exec` semantics may not propagate `SESH_SESSION` to the spawned process** — adapter registers with `session=""` and the agent_watcher excludes it from session manifest because metadata.session doesn't match the label. Symptom: 01 fails (agents missing), 07 fails.

2. **MCP config for `claude-nats-channel` may not have a stable / documented location.** Symptom: Claude Code fails to load the channel at startup, no registration appears.

3. **Oh My Pi's extension load path may require an absolute path** that's different from what we configure. Symptom: omp-nats-channel never starts.

4. **Claude Code may require a TTY** for interactive auth or first-run setup. Symptom: container hangs at Claude Code startup.

5. **`@agent-ops/sesh-channels` ESM-only may break in OMP's bundler / loader.** If OMP uses a CJS-only loader, adapter startup will fail with `ERR_REQUIRE_ESM`. Symptom: 01 fails for omp specifically.

6. **`SESH_OWNER` not set in container** (no `$USER` either). Adapter falls back to `"anon"`. Caller doesn't know to send to `agents.prompt.cc.anon.smoke-test`. Symptom: 03 fails with timeout (no agent on the subject).

7. **NATS URL discovery race** — adapter starts before sesh's hub finishes binding. Symptom: connect retries succeed eventually (backoff is wired) but first 5-10s of the rig have noise.

8. **Heartbeat cadence may be too slow for the test window.** Default is 30s. In a 90s test window we'd see ~3 heartbeats per agent. If implementation defaults are different (e.g., 60s), the count assertion needs tuning.

9. **Attachment max-payload limit.** Default NATS max payload is 1 MB. If `claude-nats-channel`'s `max_payload` metadata reflects a smaller value, attachments larger than that fail. Probably fine for our 10-byte test attachment, but flag if real-world usage will hit it.

10. **`claude-nats-channel`'s `agents_ok` metadata may be `false`** — meaning attachments aren't supported. Test 05 becomes an expected-failure case. If the operator wants attachments to work on claude, that's an upstream issue.

11. **OMP installation in Debian** may need build deps (Python, gcc, etc.) we haven't anticipated.

12. **`sesh up --exec` may not block** — if it returns immediately after spawning, the parent process exits and SIGKILLs the children. Symptom: nothing on the bus.

13. **Cross-adapter (case 06)** is most likely to be entirely unsupported in current adapter code. Expected failure → enhancement issue.

14. **Coordination-subject side effects (#91/#94)** may flood the bus with `sesh.*` subjects we don't expect. Could cause our subscriber loops to scale poorly. Tune subscription filters tightly.

15. **Synadia §6.4 "mandatory leading ack chunk"** — verify both adapters actually emit it. If they don't, the test 03/04 harness will time out waiting.

## Diagnosis & issue-filing workflow

For every FAIL:

1. **Read container logs** — `docker logs sesh-integration` + `docker exec sesh-integration cat /var/sesh/artifacts/*.log`
2. **Diagnose root cause** to function-and-line — chase to "function X in repo Y does Z when it should do W"
3. **Classify owner** — sesh / sesh-channels / orch / upstream third party
4. **File at the owning repo** with:
   - Title: failing assertion in one line
   - Body: reproduction steps (`docker build && docker run ...`), observed vs expected, code path, fix shape suggestion
5. **Track in a punch list** at `test/integration/FINDINGS.md` so we can see the full picture

## Reusability

If the rig works, three follow-ups make it permanent:

1. **`sesh-channels/.github/workflows/integration.yml`** — run the rig on every PR. Needs operator-set Anthropic API key as a GitHub secret. Test budget ~3 minutes per PR.
2. **A `bun run integration` script** in `sesh-channels/package.json` so anyone can run the rig locally
3. **Result snapshots** — first green run produces a baseline `FINDINGS.md` of `PASS` rows; subsequent runs diff against it

## Operator decisions needed

These are the actual one-way doors the operator should resolve before I start writing the Dockerfile:

1. **Auth (resolved — use darken's OAuth-mount pattern).** No operator-supplied API keys.
   - **Claude:** at rig-up time, `security find-generic-password -s "Claude Code-credentials" -w` reads the operator's macOS keychain OAuth blob. Write to a tmpfile, mount into the container at `~/.claude/.credentials.json`. A prelude script inside the container extracts `claudeAiOauth.accessToken` → exports `CLAUDE_CODE_OAUTH_TOKEN`, `unset ANTHROPIC_API_KEY`. Source pattern: `~/projects/darken/images/claude/darkish-prelude.sh` lines 51-76 + `~/projects/darken/cmd/darken/creds.go:stageClaudeCreds`.
   - **OMP / Pi:** OAuth state lives in `~/.omp/agent/agent.db` (SQLite, table `auth_credentials` — `provider` + `credential_type=oauth` + `data` blob). Operator's local install has rows for `provider=anthropic`, `google-gemini-cli`, and `openai-codex`. Rig pattern: at rig-up time, `TMPDIR=$(mktemp -d) && cp ~/.omp/agent/agent.db* "$TMPDIR/"` and mount `$TMPDIR` into the container at `/home/<user>/.omp/agent:rw`. Read-write because SQLite + WAL/SHM needs to checkpoint; we copy first to avoid touching the operator's live OMP state. OMP picks up the row on startup via the same DB-load path it uses locally.
   - **Net:** the rig's `compose.yaml` (or `docker run` script) reads the operator's existing macOS install at runtime — no secret files committed, no env-var prompts, no `.gitignore` needed beyond the usual.

2. **Single container vs docker-compose.**
   - Default plan: single container with tini + a process supervisor script. Simpler for v1. Switch to compose if PID 1 / signal forwarding gets ugly.

3. **Test against npm-published vs source-checkout for sesh-channels.**
   - Default plan: `git clone github.com/danmestas/sesh-channels` inside the image (matches what a real install would do). Use the just-published `@agent-ops/sesh-channels@0.1.0` from npm (validates the publish actually works).

4. **Cross-adapter (test 06).**
   - Default plan: implement (a) caller-mediated first. If the adapters expose a way to message each other natively, add (b) or (c) as a follow-up.

5. **CI integration timing.**
   - Default plan: ship the local rig first, validate it works, then wire it into GitHub Actions in a follow-up PR.

Unless the operator redirects, I'll proceed with all the defaults above.
