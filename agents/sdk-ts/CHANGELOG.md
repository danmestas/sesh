# Changelog

All notable changes to `@agent-ops/sesh-channels`.

## 0.1.0 — 2026-05-22

Initial release. ESM-only, Node ≥ 20.

- `readRoleClass()` — boot-time env reader: trims, defaults, validates SESH_ROLE / SESH_CLASS. Throws `ConfigError` on invalid input.
- `readAdapterConfig(defaultAgent)` — composes `readRoleClass` with NATS_URL / SESH_AGENT / SESH_OWNER / SESH_SESSION reads into a full `AdapterConfig`.
- `ConfigError` — typed error for invalid role/class at boot.
- Public types: `AgentClass`, `AdapterRoleClass`, `AdapterConfig`.

Ports the rules from [`internal/agentmeta`](https://github.com/danmestas/sesh/tree/main/internal/agentmeta) (Go) and `docs/proposals/2026-05-21-agent-role-registration.md`.
