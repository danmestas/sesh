# sesh-channels Promotion — Plan Index

**Date:** 2026-05-22
**Goal:** Move the local `~/projects/agent-channels/` workspace to a published `github.com/danmestas/sesh-channels` repo, and eliminate the 5× duplicated validators by shipping a small `@agent-ops/sesh-channels` SDK from sesh proper.

## Why

Phase 2 of the role/class work landed identical validators in 5 adapter `config.ts` files. The Ousterhout audit flagged this as accepted-but-self-policing duplication (Patch #5), explicitly deferring the SDK to "when open question #5 lands."

It's time. Centralizing the rules in `@agent-ops/sesh-channels` collapses the 5 inline validators into 5 identical `import` statements, and turns "keep these in sync by hand" into "bump a dep version." This matters not just for role/class but for every future on-the-wire metadata field — capabilities, priority, anything else added to `metadata.*`.

Meanwhile, the agent-channels workspace itself is currently a local-only git repo with no remote. Publishing it as `sesh-channels` makes the adapter collection discoverable, reviewable, and runnable as a coherent family rather than five disconnected dirs.

## Architectural decision recap

- **sesh stays the canonical source of truth.** Wire format, role/class rules, session JSON schema all defined there.
- **sesh ships a small TS SDK (`@agent-ops/sesh-channels`)** from `agents/sdk-ts/`. Single-file (~80 LoC) ESM-only package that ports `internal/agentmeta`. Tracks sesh's protocol version.
- **`sesh-channels` is the canonical adapter collection.** New repo. Hosts the adapters we own (gemini, grok, and any sesh-flavored variants of claude/pi/omp we choose to maintain).
- **Synadia-owned adapters (claude/pi/omp) stay upstream** at `synadia-ai/synadia-agents`. The sesh-channels README documents them as "compatible upstream alternatives." If we want sesh-flavored forks later, that's a follow-up decision.
- **orch is unaffected.** Adapters are inter-agent comms; orch handles process/TTY/placement. The Phase 3 merge (orch#190) was correct as-is.

## One-way doors locked in this work

| # | Door | Decision | Cost if wrong |
|---|---|---|---|
| 1 | npm package name | `@agent-ops/sesh-channels` | Squat-prone — claim the scope on day 1 |
| 2 | GitHub repo name | `sesh-channels` | Renaming after public push breaks every consumer URL/clone |
| 3 | npm scope ownership | `@agent-ops` org owned by operator | If the scope is later abandoned, an attacker can squat it |
| 4 | Module format | **ESM-only** (no CJS) | Adding CJS later is additive (minor bump); dropping CJS later is breaking |
| 5 | Env var names | `SESH_ROLE`, `SESH_CLASS` | Baked into adapter contracts; renaming forces every adapter + operator workflow to update |
| 6 | Nested-repo strategy for gemini/grok | Decided in Plan B Task 6.5 — submodule / subtree / vendor | Switching after public push requires a `--force-with-lease` or repo recreation |
| 7 | Pushing nested-repo migration commits | Operator-gated in Plan B Tasks 7-8 | Once pushed to gemini's/grok's main, a downstream consumer might depend on it |

## Two plans

| Plan | Repo | Subject | Status |
|---|---|---|---|
| A | `danmestas/sesh` | Ship `@agent-ops/sesh-channels` from `agents/sdk-ts/` | Execute first |
| B | new: `danmestas/sesh-channels` | Promote agent-channels + migrate 5 adapters onto `@agent-ops/sesh-channels` | Execute after A publishes to npm |

Plan A must publish to npm before Plan B's adapter migration can `npm install` the SDK. Suggested ordering: complete Plan A through Task 7 (publish), then start Plan B.

## Files

| Plan | Path |
|---|---|
| A | `docs/plans/2026-05-22-sesh-channels-promotion-planA-sdk-ts.md` |
| B | `docs/plans/2026-05-22-sesh-channels-promotion-planB-adapter-repo.md` |

## Out of scope

- **Coordination-subject SDK** (`sesh.task.*` etc., per `docs/proposals/2026-05-20-sesh-parallel-coordination-subjects.md`). That's a separate workstream — once role/class lands cleanly via this SDK, the coordination SDK can layer on top.
- **Synadia-owned adapter forks.** Decision deferred.
- **Phase 4 `sesh up --exec`** — still gated on sesh#89.
- **Migrating existing sesh adapters' tests away from the inline tests.** Tests stay where they are; only the validators move.
