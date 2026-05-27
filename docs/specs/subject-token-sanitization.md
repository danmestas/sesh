# Subject-token sanitization (canonical cross-stack spec)

Status: active · Owners: sesh (Go) + sesh-channels (TS) · Tracking: sesh#147

v0.4 NATS subjects are built from a `Coord` of positional tokens:

```
agents.<verb>.<machine>.<project>.<session>[.<role>[.<inst>]]
```

For a session-scoped agent to be reachable, **every stack that builds or
subscribes to these subjects must produce byte-identical tokens for the same
inputs.** The Go shim (`internal/subject`) and the TypeScript adapters
(`@agent-ops/sesh-channels-sdk`) are independent implementations of this one
spec; this document is the spec, and
`internal/subject/testdata/sanitize-vectors.json` is its executable form (both
stacks assert against the same vectors).

## Two axes — do not conflate

| Axis | Surface | Rule | Replacement |
|------|---------|------|-------------|
| **A — subject tokens** | `agents.<verb>.<machine>.<project>.<session>…` | this document | disallowed → `-`, `-` preserved |
| **B — bucket names** | `sesh_tasks_*`, `sesh_messages_*`, `sesh_artifacts_*`, `sesh_objects_*` (keyed on `scope-id`) | `docs/task-management.md` | `.`/`-`/space → `_` |

Axis B is stricter (NATS KV bucket names disallow `-`) and is **out of scope
here**. `sesh-channels/sdk/src/scope.ts` and Go `scope.Bucket` implement Axis B
and must not be changed to match this rule.

## Axis-A canonical rule

Given a raw token string, produce the sanitized token by:

1. Replace every character **not** in `[A-Za-z0-9_-]` with `-`, emitting **one
   `-` per UTF-16 code unit** of the disallowed character (a BMP char → one `-`;
   an astral/surrogate-pair char such as an emoji → two `-`).
2. Lowercase (ASCII).
3. Trim leading and trailing `-` (one or more). Internal `-` runs are **not**
   collapsed.
4. If the result is empty, the caller applies a per-token fallback
   (`machine→"local"`, `project→"default"`, `session→"default"`,
   `role→"worker"`) — the sanitizer itself returns the empty string.

`-` MUST be preserved (real machine ids look like
`m4-macbook-tail51604c-ts-net`).

The UTF-16-code-unit semantics in step 1 exist to match the deployed TS
implementation, whose regex `/[^a-zA-Z0-9_-]/g` has no `u` flag and therefore
operates on code units. **A future change adding the `u` flag on the TS side
must update the Go implementation and these vectors in the same change**, or the
stacks will silently diverge on astral input.

### Reference implementations

- Go: `internal/subject.SanitizeToken` (`internal/subject/sanitize.go`)
- TS: `sanitizeSubjectToken` (`sesh-channels`; currently per-adapter, consolidating into the SDK per sesh#147 follow-up)

Both load `internal/subject/testdata/sanitize-vectors.json` in their test
suites; drift becomes a CI failure.

## Project / session source (contract)

A session-scoped shim's `--scope-id` is `<project>.<session>`, split on the
first `.` into two separate subject tokens (sesh#121/#124,
`TestSendMessage_DottedScopeID_PublishesPrompt`). The TS adapter sources
`project` from `$SESH_PROJECT` (falling back to owner) and `session` from its
resolved session label. These round-trip only when the spawner sets the shim's
scope-id to `<$SESH_PROJECT>.<$SESH_SESSION>`; a startup assertion of that
invariant is a recommended hardening follow-up.

## Migration

None. Axis-A subjects are ephemeral (built fresh per request/registration; no
JetStream stream, durable consumer, or KV bucket is keyed on them). Shipping a
sanitization change is safe provided the Go shim and TS adapters for a given
session are released together.
