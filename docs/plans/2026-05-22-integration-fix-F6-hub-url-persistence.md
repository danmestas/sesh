# F6 — `hub.nats.url` disappears when `sesh up` exits

**Date:** 2026-05-22
**Status:** AFK-ready
**Severity:** P3 — design call, not a correctness bug
**Owner:** sesh (`cli/hub_serve.go` + `cli/hubinfo.go` interaction with `cli/session.go`)

## Root cause (function and line precise)

`cli/hub_serve.go:123` does `defer func() { _ = ClearHubInfo(seshDir) }()`. `ClearHubInfo` (`cli/hubinfo.go:97-118`) deletes `~/.sesh/hub.nats.url` (and `hub.fossil.url`). When the hub exits — either explicitly via SIGINT or implicitly via the auto-shutdown loop when the last leaf disconnects (`hub_serve.go:135-137, 187-227`) — those URL files vanish.

Downstream rigs / tools that read `~/.sesh/hub.nats.url` after the daemon has exited see ENOENT. The rig's harness already works around this by caching the URL to `/var/artifacts/hub.nats.url` on first sighting (`entrypoint.sh:177-188`).

The session JSON (`cli/session.go:46-54 SessionState`) already has a `nats_url` field that's populated by `Session.Publish` during `sesh up` boot. `cli/session.go:46-54` declares:

```go
type SessionState struct {
    PID       int        `json:"pid"`
    Scope     string     `json:"scope,omitempty"`
    NATSURL   string     `json:"nats_url,omitempty"`    // for NATS clients under this session
    ...
}
```

But this file is also removed on session Release. So the question is: is there a *persisted* place to look for the URL after both hub and session have gone away?

There isn't, by design. Both `hub.nats.url` and `<cwd>/.sesh/sessions/<label>.json` are lifecycle-bound state ("a live PID = live URL"). The fix isn't to make either file outlive its owner — that would mislead readers into dialing a dead port. The fix is to make downstream tools cache the URL while the hub is alive.

## Alternatives considered

### Option A — Document the lifecycle-bound semantics + recommend downstream caching

`hub.nats.url` is alive iff the hub is alive. Tools should cache it on first sighting and stop reading from it after the session ends.

**Interface complexity:** docs only.
**Blast radius:** docs.
**Risk:** doesn't help operators who don't read docs; but the rig already does the right thing, so the immediate gap is closed.

### Option B — Add a `sesh hub url` subcommand that prints the URL to stdout

Tools can `sesh hub url > /var/artifacts/hub.nats.url` instead of reading `~/.sesh/hub.nats.url`. The subcommand reads `~/.sesh/hub.nats.url` on demand (live), or polls until it appears with a timeout flag.

**Interface complexity:** small — new subcommand, but in `cli/hub.go`.
**Blast radius:** sesh CLI surface +1 verb.
**Risk:** small — additive.

### Option C — Persist the URL in session JSON so it survives daemon exit

Modify `Session.Publish` to write `nats_url`, and **don't delete the session JSON** on Release; instead, mark a `released_at` timestamp. Downstream tools that need history can read it.

**Interface complexity:** moderate — changes session lifecycle from "exists iff held" to "exists iff held OR recently released".
**Blast radius:** every consumer of `<cwd>/.sesh/sessions/<label>.json` — watchers, `sesh down`, `sesh status`, leaf attach logic.
**Risk:** large. Breaks the invariant that an existing JSON file means a live session. Cascades into `cli/session.go:79 ClaimSession` (which currently reaps a file with a dead PID — would need to distinguish "stale claim" from "graceful release").

### Option D — Persist the URL in session JSON, **only `nats_url`**, but keep removing the file on Release

This is a half-measure: the session JSON gains `nats_url` (it already has it — see `cli/session.go:49`), the watcher writes there, downstream tools read there *while the session is live*. The hub.nats.url file remains lifecycle-bound. Nothing changes about the after-exit story — but the session JSON becomes the canonical place to look for "URL of the hub serving this session" while alive, decoupling readers from `~/.sesh/hub.nats.url`'s narrower lifecycle.

**Interface complexity:** none — the field already exists; this is a doc-and-discipline change about which file to read.
**Blast radius:** docs + downstream callers that currently read `hub.nats.url`.
**Risk:** none for downstream tools that don't change. Net positive for ones that do.

### Chosen approach — Option A + Option D combined, with Option B as an optional follow-up

The fundamental answer is: **downstream tools should read `<cwd>/.sesh/sessions/<label>.json#nats_url` while the session is alive, and cache the URL if they need it after the session ends.** The session JSON is the canonical per-session source; `hub.nats.url` is the cross-session "currently-bound" URL.

Operators who want a one-liner can use Option B's subcommand as a follow-up; that's net additive and can ship later.

For the immediate rig: keep the existing `cp -f hub.nats.url /var/artifacts/` caching pattern (it's correct under the existing semantics). Document the rationale.

## Operator decisions deferred

**Decision F6.1 — Ship `sesh hub url` subcommand (Option B) as part of this fix, or defer?**

**Axis: architecture** (adds CLI surface that becomes hard to revert once depended on). Plan ships with **defer** (Option A + D only). If the operator wants Option B in this round, this plan needs a Task 3-N for the subcommand impl + tests.

## AFK-ready plan (Option A + D)

### Task 1 — Document the lifecycle of `hub.nats.url` and `nats_url` in session JSON

**File:** `/Users/dmestas/projects/sesh/docs/synadia-agents-on-sesh.md` — extend the existing NATS-URL discovery section (or add one if none exists) with:

```markdown
## NATS URL discovery and lifecycle

sesh publishes the hub's NATS client URL in two places, each with a
different lifecycle:

### `~/.sesh/hub.nats.url` (hub-lifetime)

Written atomically by `sesh hub serve` (cli/hub_serve.go:116-122 via
WriteHubInfo). Cleared on hub exit (cli/hub_serve.go:123,
cli/hubinfo.go:97-118 ClearHubInfo). The file exists iff a hub daemon
is currently running.

Use when: you're a process that wants to attach to "the current sesh hub"
without knowing which session brought it up.

### `<cwd-walk>/.sesh/sessions/<label>.json#nats_url` (session-lifetime)

Written by `Session.Publish` (cli/session.go:111-139) at the start of
`sesh up`. Removed by `Session.Release` (cli/session.go:188-194) when
`sesh up` exits. The file exists iff a session is currently held under
that label.

Use when: you're a process that wants the NATS URL for a *specific*
session, identified by label.

### Caching across exit

If your tool needs the URL after the hub / session has exited (e.g.,
post-run analysis on an integration test rig), cache the URL to your
tool's own artifact directory on first sighting:

```bash
# Inside an entrypoint, while sesh up is alive:
cp -f ~/.sesh/hub.nats.url /var/artifacts/hub.nats.url
```

Do NOT rely on either file existing after the owning process has exited;
they are lifecycle-bound by design (cli/hub_serve.go:123,
cli/session.go:188-194). A stale URL pointing at a dead port is a worse
failure mode than ENOENT.
```

### Task 2 — Cross-link from `cli/hubinfo.go`

**File:** `/Users/dmestas/projects/sesh/cli/hubinfo.go`

The existing comment at `ClearHubInfo` (lines 97-118) is good but doesn't link out. Add:

```diff
 // ClearHubInfo removes hub.nats.url and hub.fossil.url — the two files
 // WriteHubInfo manages. ENOENT is swallowed so callers can defer Clear
 // unconditionally without caring which subset got published before
 // shutdown. Only throwaway tier-3 hub state is touched here —
 // JetStream (~/.sesh/messaging/) and the Fossil hub repo
 // (~/.sesh/hub.repo*) are never removed by this function.
 //
 // hub.url is not in scope: it belongs to the daemon's lease and is
 // removed by Lease.Release when the daemon exits.
+//
+// Downstream tools that need the URL after the hub exits should read
+// <cwd>/.sesh/sessions/<label>.json#nats_url while the session is alive
+// and cache it locally. Both this file and the session JSON are
+// lifecycle-bound by design — see docs/synadia-agents-on-sesh.md
+// "NATS URL discovery and lifecycle".
 func ClearHubInfo(stateDir string) error {
```

### Task 3 — Confirm `Session.Publish` already writes `nats_url`

**File:** `/Users/dmestas/projects/sesh/cli/up.go`

Search for the call site that constructs and publishes the `SessionState`. It should already set `NATSURL` — verify, no change expected. If for some reason the field is empty, fix that call site (this would be a hidden bug, separate from F6's main thrust — file a follow-up).

A failing test guarding the contract:

**File:** `/Users/dmestas/projects/sesh/cli/session_test.go` — add:

```go
// TestSessionStatePersistsNATSURL verifies the session JSON written by
// Session.Publish carries the NATSURL field, so downstream tools can read
// it from <cwd>/.sesh/sessions/<label>.json#nats_url. See F6 in the
// integration-rig findings and docs/synadia-agents-on-sesh.md
// "NATS URL discovery and lifecycle".
func TestSessionStatePersistsNATSURL(t *testing.T) {
    dir := t.TempDir()
    s, err := ClaimSession(dir, "f6-test")
    if err != nil {
        t.Fatalf("claim: %v", err)
    }
    defer s.Release()

    wantURL := "nats://127.0.0.1:65535"
    if err := s.Publish(SessionState{
        PID:     os.Getpid(),
        NATSURL: wantURL,
    }); err != nil {
        t.Fatalf("publish: %v", err)
    }

    got, err := ReadSession(dir, "f6-test")
    if err != nil {
        t.Fatalf("read: %v", err)
    }
    if got.NATSURL != wantURL {
        t.Fatalf("nats_url: got %q want %q", got.NATSURL, wantURL)
    }
}
```

(Adjust imports / package as the existing session_test.go pattern dictates.)

### Task 4 — Rig README: explain the cache pattern + which file is canonical

**File:** `/Users/dmestas/projects/sesh/test/integration/README.md` — append:

```markdown
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
`sesh/docs/synadia-agents-on-sesh.md#nats-url-discovery-and-lifecycle`.

For per-session URL discovery (which session owns which hub), prefer
`<cwd>/.sesh/sessions/<label>.json#nats_url` over `hub.nats.url` — the
session JSON's `nats_url` field is written at `sesh up` boot and is the
canonical per-session reference.
```

### Task 5 — Commit + PR

```bash
cd /Users/dmestas/projects/sesh
git checkout -b docs/hub-url-lifecycle-clarification
git add docs/synadia-agents-on-sesh.md cli/hubinfo.go cli/session_test.go test/integration/README.md
git commit -m "docs(hub): clarify hub.nats.url / session.nats_url lifecycle + add regression guard (closes F6)"
```

Open a PR. Don't push to main directly.

## Dependencies

None. F6 is independent.

## Optional follow-ups

- **`sesh hub url` subcommand (Option B):** prints the URL to stdout; polls if not yet bound; respects a `--timeout` flag. Surface as a separate proposal; do not ship in this F6 PR.
- **`Session.URL(label) (string, error)` helper for Go callers:** read `<cwd>/.sesh/sessions/<label>.json` and return the `nats_url` field. Convenient for in-process consumers (`internal/refagent`, future tools). Surface as a separate proposal.
