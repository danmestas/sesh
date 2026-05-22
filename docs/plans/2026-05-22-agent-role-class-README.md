# Agent Role & Class Registration — Plan Index

**Source proposal:** `docs/proposals/2026-05-21-agent-role-registration.md`

The proposal lays out four independently-shippable phases. This directory contains one plan per phase.

| Phase | Subsystem | Plan | Status |
|---|---|---|---|
| 1 | sesh core (Go) — `Config`, `AgentRef`, `agent_watcher` | [`2026-05-22-agent-role-class-phase1-sesh.md`](./2026-05-22-agent-role-class-phase1-sesh.md) | ready to implement |
| 2 | NATS-channel adapters (TS) — `claude-nats-channel` + siblings | [`2026-05-22-agent-role-class-phase2-adapters.md`](./2026-05-22-agent-role-class-phase2-adapters.md) | ready after Phase 1 merges |
| 3 | `orch-spawn` (cross-repo: `~/projects/orch/`) | [`2026-05-22-agent-role-class-phase3-orch-spawn.md`](./2026-05-22-agent-role-class-phase3-orch-spawn.md) | ready after Phase 1 merges |
| 4 | `sesh up --exec` flag plumbing | not planned | gated on [sesh#89](https://github.com/danmestas/sesh/issues/89) |

## Suggested ordering

1. **Phase 1** first — unblocks everything else and is the smallest concrete change (one Go repo, ~6 tasks).
2. **Phase 2 (claude-nats-channel only)** next — minimum to satisfy Phase 1's "at least one adapter updated" acceptance criterion.
3. **Phases 2 (remaining adapters) and 3** can land in parallel; they don't conflict.
4. **Phase 4** waits for sesh#89.

## Compatibility note

Every phase is additive against the upstream Synadia v0.3 protocol. Vanilla `@synadia-ai/agents` callers continue to discover and prompt sesh-hosted agents without code changes — `metadata.role` / `metadata.class` are opaque to the spec. See the proposal's "Metadata" section for the Synadia compatibility rationale.
