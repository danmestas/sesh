# Changelog

All notable changes to `@agent-ops/sesh-channels`.

## 0.1.1 — 2026-05-22

Additive, non-breaking.

- `readSessionLabel(opts?)` — boot-time session label resolver: prefers `$SESH_SESSION`, then walks cwd → root for the nearest `.sesh/sessions/<label>.json`. Returns `null` if no unique label is reachable (with a one-line stderr diagnostic on ambiguity). Mirrors the resolution logic claude-nats-channel formerly inlined as `discoverSessionLabel()`. Adapters compose this helper with their own NATS_SESSION_NAME / config / basename(cwd) fallback chain.
- New exported type `SessionLabelOptions` for the helper's `{ startDir, env, warn }` injection points (test-friendliness).

Closes the F2 adapter-inconsistency finding from the docker integration rig (`omp-nats-channel` formerly ignored `$SESH_SESSION`).

## 0.1.0 — 2026-05-22

Initial release. ESM-only, Node ≥ 20.

- `readRoleClass()` — boot-time env reader: trims, defaults, validates SESH_ROLE / SESH_CLASS. Throws `ConfigError` on invalid input.
- `readAdapterConfig(defaultAgent)` — composes `readRoleClass` with NATS_URL / SESH_AGENT / SESH_OWNER / SESH_SESSION reads into a full `AdapterConfig`.
- `ConfigError` — typed error for invalid role/class at boot.
- Public types: `AgentClass`, `AdapterRoleClass`, `AdapterConfig`.

Ports the rules from [`internal/agentmeta`](https://github.com/danmestas/sesh/tree/main/internal/agentmeta) (Go) and `docs/proposals/2026-05-21-agent-role-registration.md`.
