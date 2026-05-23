# Notes for Anthropic — Claude Code containerized ergonomics

> **Status:** Draft for operator's discretion. Per the project's
> no-third-party-filing rule, Claude Code subagents do not file this
> upstream. The operator may copy any subset verbatim into a feedback
> channel of their choosing, or discard.

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
