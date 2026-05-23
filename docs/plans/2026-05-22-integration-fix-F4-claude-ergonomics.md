# F4 — Claude Code ergonomic blockers for unattended container runs

**Date:** 2026-05-22
**Status:** AFK-ready (rig-side documentation only — no upstream filings per CLAUDE.md no-third-party-filing rule)
**Severity:** P2 — cumulatively painful, individually small
**Owner:** sesh rig (`test/integration/`) — document; **NOT** anthropic/claude-code (third-party, off-limits)

## Root cause (per sub-symptom)

The rig already implements four workarounds in `Dockerfile` and `entrypoint.sh`. Each is a real claude-code ergonomic gap when running unattended in a container:

### F4.1 — `--dangerously-skip-permissions` refuses to run under root

claude-code 2.1.126 exits with "for security reasons" when `geteuid() == 0`. Source: claude binary string `you can't use --dangerously-skip-permissions because the session was not launched with --dangerously-skip-permissions` and adjacent "not allowed under root" check (binary offsets ~194285, ~214927).

Rig workaround: `Dockerfile:96` creates a non-root `integ` user (uid 1500), runs the entrypoint as that user.

### F4.2 — `.mcp.json` auto-discovery dialog blocks even with `--dangerously-skip-permissions`

Project-local `.mcp.json` files trigger a "New MCP server found" 1/2/3 trust dialog at first sight. `--dangerously-skip-permissions` does not auto-accept it.

Rig workaround: `Dockerfile:108-110` does not bake a `.mcp.json` into `/workspace`. The entrypoint passes `--strict-mcp-config --mcp-config /opt/claude.mcp.json` instead. `--strict-mcp-config` disables `.mcp.json` discovery entirely; the explicit `--mcp-config` is treated as operator-supplied and pre-trusted.

### F4.3 — First-run "Bypass Permissions mode" warning dialog blocks startup

claude renders a dialog: `WARNING: Claude Code running in Bypass Permissions mode` with `Yes, I accept` / `No, exit` options (binary offsets ~222281-222285). `bypassPermissionsModeAccepted: true` in `~/.claude.json` persists across runs in the operator's home but doesn't transfer cleanly into a fresh container because claude reads `~/.config.json` (which is different).

There IS a managed-settings key for this: `skipDangerousModePermissionPrompt` (binary string ~161599). Setting it true in `~/.claude/settings.json` (or a managed-settings file) would dismiss the dialog without TTY input.

Rig workaround: `entrypoint.sh:117-120` opens a FIFO and feeds `2\n` (the "accept" option) after a 6 s delay.

### F4.4 — `--print` + `--dangerously-skip-permissions` interact oddly with non-TTY stdin

This is observed behavior; the rig's solution is `script -qfc` (PTY wrapper) so claude thinks it's on a TTY. See `entrypoint.sh:131`.

## Alternatives considered

### Option A — Keep all the rig workarounds; document them; surface upstream gaps as operator-visible notes (no filing)

Per CLAUDE.md: no filings at non-user-owned repos. We document the workarounds in the rig README so other operators don't rediscover them, and surface the upstream gaps as a single "Notes for Anthropic" section the operator can optionally lift verbatim into a feedback channel of their choosing (or skip).

**Interface complexity:** docs only.
**Blast radius:** rig README.
**Reversibility:** trivial.

### Option B — Apply the `skipDangerousModePermissionPrompt: true` managed-setting and drop the FIFO `2\n` for F4.3

This is a real improvement: managed settings remove the dialog at the source, so we don't need the FIFO timing kludge. The rig already lays down `/home/integ/.claude/`; one extra settings.json line eliminates one workaround.

**Interface complexity:** +1 file in the Dockerfile copy step.
**Blast radius:** rig only.
**Reversibility:** trivial.
**Risk:** none — if the setting is unknown to a future claude version, it's silently ignored.

### Chosen approach — Option B + Option A's documentation

Adopt the managed-settings dismissal for F4.3 (drops one timed FIFO feed — also reduces the FIFO complexity that F1 added). Keep the non-root user (F4.1), strict-mcp-config (F4.2), and PTY wrapper (F4.4) workarounds as-is. Document all four in the rig README. Surface a separate "Notes for upstream" doc the operator can choose to file or skip.

## Operator decisions deferred

**Decision F4.1 — Lift the upstream notes to anthropic/claude-code?**

**Axis: ethics** (filing at a third-party public repo where the body content reflects on the operator). Per CLAUDE.md "no filings at non-user-owned repos", the plan ships with **no** upstream filing. If the operator wants to file, they take the verbatim notes from `docs/upstream-notes-claude-code-ergonomics.md` (created by this plan) and paste them into the channel they prefer. The plan does not file them automatically.

## AFK-ready plan

### Task 1 — Add `~/.claude/settings.json` with `skipDangerousModePermissionPrompt: true`

**File:** `/Users/dmestas/projects/sesh/test/integration/config/claude-settings.json` (new)

```json
{
  "skipDangerousModePermissionPrompt": true
}
```

**File:** `/Users/dmestas/projects/sesh/test/integration/Dockerfile` — add a COPY line after the existing `claude.mcp.json` copy:

```dockerfile
# Skip the "Bypass Permissions mode" warning dialog at startup. Without this,
# claude-code 2.1.126 renders a TTY-only dialog that requires "2\n" input,
# which the rig was previously feeding via a FIFO timing kludge. The
# managed-settings key dismisses the dialog at the source. (F4.3)
COPY --chown=integ:integ test/integration/config/claude-settings.json /home/integ/.claude/settings.json
```

### Task 2 — Drop the first FIFO `2\n` feed in `entrypoint.sh`

**File:** `/Users/dmestas/projects/sesh/test/integration/entrypoint.sh`

After Task 1 lands, F4.3's dialog no longer fires. The FIFO becomes:

```bash
# Hold the claude FIFO open. After F1 + F4 wired the appropriate flags +
# managed-settings, the only dialog that still fires under
# --dangerously-load-development-channels is the dev-channels warning
# (~10s after startup). The bypass-permissions warning is dismissed via
# `skipDangerousModePermissionPrompt: true` in ~/.claude/settings.json.
(
  sleep 10
  printf '2\n'
  sleep infinity
) > /tmp/claude.fifo &
( sleep infinity > /tmp/omp.fifo ) &
```

**Coordinates with F1**: F1's plan added a second `2\n` feed for the dev-channels dialog. After F4 lands the bypass-permissions dialog is gone, so the FIFO only needs one feed (for dev-channels). Net: F1 + F4 together leave us with one timed feed instead of two.

### Task 3 — Document all four ergonomic workarounds in the rig README

**File:** `/Users/dmestas/projects/sesh/test/integration/README.md` — append:

```markdown
## Claude Code unattended-mode workarounds (F4)

The rig works around four real Claude Code 2.1.126 ergonomic gaps for
containerized / unattended runs. Each is documented here so future operators
don't rediscover them.

### F4.1 — Non-root user required

`claude --dangerously-skip-permissions` refuses to run when `geteuid() == 0`.
The Dockerfile creates a non-root `integ` user (uid 1500) and runs the
entrypoint as that user. See `Dockerfile:96-103`.

### F4.2 — `.mcp.json` auto-discovery dialog

A project-local `.mcp.json` triggers a "New MCP server found" 1/2/3 trust
dialog at first sight, even with `--dangerously-skip-permissions`. The rig
does not bake any `.mcp.json` into `/workspace`. claude-code is launched
with `--strict-mcp-config --mcp-config /opt/claude.mcp.json` instead.
`--strict-mcp-config` disables `.mcp.json` discovery; the explicit
`--mcp-config` is treated as operator-supplied and pre-trusted.
See `Dockerfile:108-110` and `entrypoint.sh` claude launch.

### F4.3 — Bypass-Permissions warning dialog

claude-code renders `WARNING: Claude Code running in Bypass Permissions mode`
at first run. The managed-settings key
`skipDangerousModePermissionPrompt: true` dismisses it at the source.
The rig copies `test/integration/config/claude-settings.json` to
`/home/integ/.claude/settings.json` to enable this. See `Dockerfile`.

### F4.4 — Non-TTY stdin behavior

claude under non-TTY stdin (containerized stdout redirect) behaves
inconsistently with `--print` + `--dangerously-skip-permissions`. The rig
wraps claude with `script -qfc <cmd> /dev/null` (Linux PTY wrapper) so
claude thinks it's running on a TTY. See `entrypoint.sh` claude launch.

### Dev-channels warning dialog (covered by F1, not F4)

`--dangerously-load-development-channels` (required by F1) raises a
separate "Loading development channels" dialog. The rig auto-feeds `2\n`
to dismiss it ~10 s after startup. See F1's plan for context.
```

### Task 4 — Surface the upstream gaps in a docs file (operator may file or skip)

**File:** `/Users/dmestas/projects/sesh/docs/upstream-notes-claude-code-ergonomics.md` (new — for operator's optional use)

```markdown
# Notes for Anthropic — Claude Code containerized ergonomics

> **Status:** Draft for operator's discretion. Per CLAUDE.md no-third-party-filing,
> Claude Code subagents do not file this. The operator may copy any subset
> verbatim into a feedback channel of their choosing, or discard.

The following four behaviors were observed while wiring claude-code into a
fully unattended Docker integration rig (Claude Code 2.1.126). Each
requires a workaround that's small in isolation but cumulatively makes
containerized claude painful.

## 1. `--dangerously-skip-permissions` refuses under root

claude-code 2.1.126 exits when `--dangerously-skip-permissions` is passed
under `euid=0`. In a Docker / Kubernetes context the default user is
often root unless the image explicitly creates and switches to a non-root
user. Workaround: create a non-root user in the image.

Suggested fix: allow root + `--dangerously-skip-permissions` when an
explicit opt-in is also passed (e.g.,
`--allow-root-with-dangerously-skip-permissions`), so containerized
operators can make an informed choice.

## 2. `.mcp.json` auto-discovery dialog blocks even with `--dangerously-skip-permissions`

The dialog "New MCP server found in .mcp.json: <name> — 1/2/3" requires
TTY input. Containerized claude has no TTY. `--dangerously-skip-permissions`
should suppress this dialog (or there should be a parallel
`--skip-mcp-discovery-prompt` setting).

Suggested fix: managed-settings key `skipMcpDiscoveryPrompt` analogous to
the existing `skipDangerousModePermissionPrompt`.

## 3. `bypassPermissionsModeAccepted` doesn't persist across containers

Setting `bypassPermissionsModeAccepted: true` in `~/.claude.json` works on
a developer's host machine but the in-container `~/.claude.json` is a
mount of a freshly-staged copy that may not preserve the key, depending
on how the operator stages credentials. The managed-settings key
`skipDangerousModePermissionPrompt: true` is the cleaner path but is
under-documented.

Suggested fix: document `skipDangerousModePermissionPrompt` in the public
claude-code docs (it's currently only visible in the binary's managed-
settings key table).

## 4. `--dangerously-load-development-channels` warning dialog has no managed-settings dismissal

This is the rig's largest single workaround. The dev-channels warning
dialog must be dismissed by typing `2\n` over the channel's stdin. There's
no managed-settings analog to `skipDangerousModePermissionPrompt` for it.

Suggested fix: managed-settings key `skipDevelopmentChannelsWarning`
(or `skipDangerousChannelsPromptDevOnly`), gated on `--dangerously-load-
development-channels` having been explicitly passed (so the dismissal
requires *two* explicit opt-ins: the flag + the settings key).

---

If the team is interested, the integration rig at
`github.com/danmestas/sesh/test/integration` reproduces all four blockers
verbatim and may be useful as a confirmation environment.
```

### Task 5 — Commit + PR

```bash
cd /Users/dmestas/projects/sesh
git checkout -b feat/integration-rig-f4-claude-settings
git add test/integration/config/claude-settings.json \
        test/integration/Dockerfile \
        test/integration/entrypoint.sh \
        test/integration/README.md \
        docs/upstream-notes-claude-code-ergonomics.md
git commit -m "fix(test/integration): drop bypass-perms FIFO via skipDangerousModePermissionPrompt + document ergonomic workarounds (closes F4)"
```

Open a PR. Don't push to main directly.

## Dependencies

- **F4 depends on F1:** F1 also touches the claude FIFO (adds the dev-channels feed). F4 simplifies that FIFO once the bypass-perms dialog is settings-suppressed. Land F1 first; rebase F4 onto it. If they conflict, the conflict is in `entrypoint.sh` and resolves trivially (keep both changes: the settings.json removes the bypass-perms dialog, F1's flag adds the dev-channels feed).

## Optional follow-ups

- **Do not file at anthropic/claude-code.** The upstream-notes doc is intentionally pre-filed (i.e., already drafted) so the operator can copy-paste if they want, but the AFK subagent does not file anything at third-party repos.
