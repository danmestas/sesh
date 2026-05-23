# Integration-Rig Findings — AFK Fix Plans Index

**Date:** 2026-05-22
**Origin:** `test/integration/FINDINGS.md` — 4 PASS / 4 FAIL from the Docker rig.
**Goal:** convert each finding into an AFK-ready spec + plan so the operator can dispatch fixes autonomously with minimal interactive decisions.

## Findings × Plans

| ID | Finding | Plan file | Owner repo(s) | PR target | Status |
|----|---------|-----------|---------------|-----------|--------|
| F1 | Inbound prompt doesn't trigger a turn (channel gate) | `2026-05-22-integration-fix-F1-channel-flag.md` | sesh (rig) + sesh-channels (docs) | `sesh#feat/integration-rig-f1-channel-flag` + `sesh-channels#docs/claude-channel-launch-flag` | **PLANNED** |
| F2 | OMP doesn't read `SESH_SESSION` | `2026-05-22-integration-fix-F2-omp-session-env.md` | `@agent-ops/sesh-channels` SDK + sesh-channels (OMP + audit pi/gemini/grok) + sesh (rig) | SDK PR, then 1-4 adapter PRs, then rig PR | **PLANNED** (serialized by npm publish) |
| F3 | `sesh up` single-owner-per-label is intentional | `2026-05-22-integration-fix-F3-session-claim-model.md` | sesh (docs) | `sesh#docs/session-ownership-clarification` | **PLANNED** |
| F4 | Claude Code ergonomic blockers | `2026-05-22-integration-fix-F4-claude-ergonomics.md` | sesh (rig) — upstream gaps documented, **not filed** | `sesh#feat/integration-rig-f4-claude-settings` | **PLANNED** |
| F5 | OMP autodiscovers claude's `.mcp.json` | `2026-05-22-integration-fix-F5-omp-mcp-discovery.md` | sesh-channels (strict mode) + sesh (rig) | `sesh-channels#feat/claude-channel-strict-mode` + `sesh#feat/integration-rig-f5-strict-mode` | **PLANNED** |
| F6 | `hub.nats.url` disappears on hub exit | `2026-05-22-integration-fix-F6-hub-url-persistence.md` | sesh (docs + regression guard) | `sesh#docs/hub-url-lifecycle-clarification` | **PLANNED** |
| F7 | OMP ANSI escapes in logs | `2026-05-22-integration-fix-F7-F8-rig-polish.md` | sesh (rig) | `sesh#chore/integration-rig-f7-f8-polish` (shared) | **PLANNED** |
| F8 | Docker build I/O error on tight disk | `2026-05-22-integration-fix-F7-F8-rig-polish.md` | sesh (docs) | same as F7 | **PLANNED** |

Every finding has an AFK-ready plan. None are blocked at "cannot be made AFK-ready" — though F1 depends on one operator taste decision (the MCP server name) before dispatch.

## Operator decisions — single consolidated list

The operator answers these N questions once before AFK dispatch, then the subagents pick up from the plans.

### Decision 1 (F1.1) — Channel-server name

**Axis: taste.** The `nats` server name appears in:

- `test/integration/config/claude.mcp.json` under `mcpServers.nats`
- `entrypoint.sh`'s `--dangerously-load-development-channels nats` arg

Default: keep `nats` (matches upstream `synadia-agents` convention). Alternatives: `sesh-channel`, `sesh-nats`, `agents`.

**Pick one. Default acceptable: yes — `nats`.**

### Decision 2 (F2.1) — `@agent-ops/sesh-channels` SDK release version

**Axis: reversibility** (npm publish is one-way). When `readSessionLabel` lands, publish as:

- (a) `0.1.1` (patch — additive, non-breaking)
- (b) `0.2.0` (minor — additive but signals "you should adopt this")

Plan defaults to (a) `0.1.1`. Both are correct; the difference is signaling.

**Pick one. Default acceptable: yes — `0.1.1`.**

### Decision 3 (F2.2) — Naming of the new SDK function

**Axis: taste.** Options:

- (a) `readSessionLabel` (matches SDK convention: `readRoleClass`, `readAdapterConfig`)
- (b) `discoverSessionLabel` (matches what the source claude-nats-channel function is named)

Plan defaults to (a). To change, find/replace at the SDK + adapter sites listed in F2's plan.

**Pick one. Default acceptable: yes — `readSessionLabel`.**

### Decision 4 (F3.1) — Add `sesh up --join` flag now, or docs-only?

**Axis: architecture.** Adding `--join` locks in primary/secondary session semantics across the session/up/watcher modules.

- (a) Docs only (Option A in F3's plan)
- (b) Add `--join` flag (Option B)

Plan defaults to (a). If you want (b) in this round, F3's plan needs additional tasks for the flag impl + lifecycle semantics — flag it before dispatch.

**Pick one. Default acceptable: yes — docs only.**

### Decision 5 (F4.1) — File the claude-code ergonomic notes upstream?

**Axis: ethics** (third-party public repo + content reflects on operator).

- (a) Do not file (default, per CLAUDE.md no-third-party-filing). The plan ships a pre-drafted `docs/upstream-notes-claude-code-ergonomics.md` you can lift verbatim later if you want.
- (b) Operator personally files them at anthropic/claude-code after the AFK wave completes.

Plan defaults to (a). The AFK subagents never file at third-party repos regardless of this answer — but (b) means you the operator open the issue yourself, not the subagents.

**Pick one. Default acceptable: yes — do not file (defer indefinitely).**

### Decision 6 (F5.1) — `NATS_CHANNEL_STRICT` default

**Axis: architecture** (changing user-visible default behavior is a one-way migration).

- (a) `strict` defaults to `false` (current auto-suffix behavior preserved). Operators opt in. The rig sets it.
- (b) `strict` defaults to `true` (loud failure becomes default). Operators opt out.

Plan defaults to (a). (b) would be a follow-on PR after operators have had a release to adopt.

**Pick one. Default acceptable: yes — opt-in.**

### Decision 7 (F5.2) — File OMP MCP-exclusion feature request upstream?

**Axis: ethics.** Same shape as Decision 5. Default: do not file. Plan ships pre-drafted notes the operator can lift later.

**Pick one. Default acceptable: yes — do not file.**

### Decision 8 (F6.1) — Ship `sesh hub url` subcommand as part of F6?

**Axis: architecture** (adds CLI verb that becomes hard to revert).

- (a) Defer (docs-only F6)
- (b) Ship subcommand alongside F6

Plan defaults to (a). (b) needs additional tasks for the subcommand impl + tests — flag before dispatch.

**Pick one. Default acceptable: yes — defer.**

---

**Net: 8 binary questions, all with operator-acceptable defaults. Reading time ~3 minutes. Total operator-side work before AFK dispatch: ~5 minutes if all defaults are accepted.**

## Suggested AFK execution order

### Wave 1 — Independent, parallel (dispatch all 4 immediately)

| Plan | Touches | Parallelism note |
|------|---------|------------------|
| F1   | `sesh/test/integration/{entrypoint.sh, README.md}` + `sesh-channels/claude-nats-channel/README.md` | Rig + docs only; no test conflicts. Highest-impact — unblocks 4 rig cases. |
| F3   | `sesh/{docs/synadia-agents-on-sesh.md, cli/up.go (help text only), cli/session.go (comment only), cli/session_test.go}` | Docs + comment; cli/up.go change is one help string. |
| F6   | `sesh/{docs/synadia-agents-on-sesh.md, cli/hubinfo.go (comment only), cli/session_test.go, test/integration/README.md}` | Docs + regression test. |
| F7/F8 | `sesh/test/integration/{entrypoint.sh, README.md}` | Rig only. |

**Conflict resolution note:** F1 and F7/F8 both touch `entrypoint.sh`. F4 also touches `entrypoint.sh`. Land F1 first (largest change), then rebase F4 onto F1, then rebase F7/F8 onto F4. Mechanical conflicts.

**F3 + F6 both touch `docs/synadia-agents-on-sesh.md`** with non-overlapping sections. Land in either order; second-to-land rebases trivially.

**F3 + F6 both touch `cli/session_test.go`** with non-overlapping test functions. Same — trivial rebase.

### Wave 2 — Serial on SDK publish (after Wave 1 completes)

| Plan | Touches | Sequencing |
|------|---------|------------|
| F2 (SDK part) | `@agent-ops/sesh-channels` source repo (location TBD by operator) | Land + publish first. |
| F2 (OMP) | `sesh-channels/omp-nats-channel/{extensions/nats-channel.ts, extensions/config.ts, test/, package.json}` | After SDK publish. |
| F2 (audit grok/gemini/pi) | Same shape as OMP, one PR per adapter that has the bug | Parallel with OMP if disjoint. |
| F2 (rig workaround removal) | `sesh/test/integration/entrypoint.sh` | After OMP PR merges. |

### Wave 3 — Strict mode (after Wave 1 + F1 confirmed green)

| Plan | Touches | Sequencing |
|------|---------|------------|
| F5 (sesh-channels) | `sesh-channels/claude-nats-channel/{server.ts, README.md, test/}` | Land + publish (or local checkout). |
| F5 (rig) | `sesh/{test/integration/config/claude.mcp.json, test/integration/README.md, docs/upstream-notes-omp-mcp-discovery.md}` | After F5 sesh-channels lands. |

### Wave 4 — Claude ergonomics (after F1, before Wave 2)

| Plan | Touches | Sequencing |
|------|---------|------------|
| F4   | `sesh/{test/integration/config/claude-settings.json (new), Dockerfile, entrypoint.sh, README.md, docs/upstream-notes-claude-code-ergonomics.md (new)}` | After F1 (because F4 simplifies the FIFO that F1 added a feed to). |

### Visual dispatch graph

```
        Wave 1 (4 parallel):
        ┌─────── F1 (rig + sesh-channels README) ─────────┐
        │                                                  │
        ├─────── F3 (sesh docs) ───────────────────────────┤
        │                                                  ├──> Wave 4: F4 (after F1)
        ├─────── F6 (sesh docs + test) ────────────────────┤              │
        │                                                  │              ├──> Wave 3: F5 (after F1)
        └─────── F7/F8 (rig polish) ───────────────────────┘
                                                                          ↓
        Wave 2 (sequential):
        F2 SDK ───> publish ───> F2 OMP (and audit pi/gemini/grok in parallel) ───> F2 rig drop-workaround
```

## What cannot be made AFK-ready

**Nothing.** All eight findings have AFK-ready plans. Two have operator-driven *follow-ups* the AFK subagents won't execute:

- **Optional upstream filings** at anthropic/claude-code (F4) and oh-my-pi/pi-coding-agent (F5(c)). Per CLAUDE.md no-third-party-filing, the AFK subagents draft the notes but never file them. The operator can lift the drafts post-AFK if they choose.
- **Optional follow-up proposals** for `sesh up --join` (F3), `sesh hub url` subcommand (F6), `Session.URL()` helper (F6). These are scope-expansions outside the F-finding contract; surface as separate proposals if/when desired.

## Verifying the wave landed correctly

After all PRs merge, re-run the rig:

```bash
cd /Users/dmestas/projects/sesh/test/integration
./scripts/run.sh
cat /var/artifacts/results.txt
```

Expected outcome: **8 / 8 PASS** (4 previously-failing cases 03/04/05/06 now green via F1 + F2; the 4 previously-passing cases 01/02/07/08 remain green; nothing regresses).

If anything regresses, F1 is the highest-leverage to inspect — its symptom (timeout, no chunks) is unmistakable.
