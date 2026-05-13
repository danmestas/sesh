# orch ↔ sesh bridge

A NATS comms bridge that lets a parent Claude Code session ("orchestrator") drive N spawned [`orch`](https://github.com/danmestas/orch) builder panes ("subagents") through sesh's mesh instead of through tmux + filesystem-marker IPC.

This document describes the wire shape (subjects + JSON envelopes + hook touchpoints) and the current implementation state. The bridge belongs in `orch` itself (not in sesh) — sesh provides the transport, the bridge is `orch`'s adapter onto it.

## What the bridge is for

[`orch`](https://github.com/danmestas/orch) ships orchestration scripts (`orch-tell`, `orch-listen`, `orch-ask`, `orch-peek`, `orch-spawn`, …) whose default comms are tmux IPC + filesystem markers under `~/.cache/orch-stop/`. That works fine for one-orchestrator-one-machine setups but doesn't compose with sesh's mesh — there's no way for a subscriber on another leaf to observe the events, no way to durably replay them, no session scoping.

The bridge fills that gap. Three additional hook scripts on the orch side publish to NATS in parallel with the existing marker writes; one subscriber daemon receives prompts from NATS and dispatches them through the existing `orch-tell` to the matching tmux pane. Marker behavior is preserved — NATS is additional fan-out.

## Wire shape (the four subjects)

Subjects use the prefix `orch` by default. Override per-session via `ORCH_NATS_SUBJECT_PREFIX` env (see [Sesh-affinity](#sesh-affinity-current-state-and-gaps) below for what the prefix should look like once session-scoped).

### Outbound (orch → NATS) — three publish hooks

| Subject | Trigger | Body (JSON) |
|---|---|---|
| `orch.stop.<pane_num>` | Stop hook (assistant turn ends) | `{event:"stop", pane_id, session_id, cwd, ts_ns, ts_iso}` |
| `orch.notify.<pane_num>` | Notification hook (attention-needed) | `{event:"notify", pane_id, session_id, message, cwd, ts_ns, ts_iso}` |
| `orch.events.<pane_num>` | SessionStart hook → `tail -F` on transcript JSONL | one JSONL line per message (raw Claude Code transcript shape) |

`<pane_num>` is the numeric suffix of the tmux pane id (`%37` → `37`), because NATS subject tokens cannot contain `%`. `pane_id` inside the JSON body keeps the original `%`-prefixed form.

Each publish-hook gates on `$ORCH_PANE_ID` being set, so the hooks are safe to install globally — they no-op for non-orch Claude sessions. Each publish has `--timeout=1s` so a missing or unreachable NATS server doesn't stall the host hook past its budget.

### Inbound (NATS → orch) — one subscriber

| Subject | Body (JSON) | Effect |
|---|---|---|
| `orch.tell` | `{pane:"%37", prompt:"…"}` | `orch-tell %37 -` with prompt piped in |

Single fixed subject + JSON-shape body, not per-pane subjects. Two reasons:

- NATS subject tokens can't contain `%`, so per-pane subjects need lossy encoding of pane ids.
- The `nats sub --translate` hook receives the message body on stdin but does NOT expose `$NATS_SUBJECT` to the translator command — so a single subscriber can't reliably pair subject with body in shell. JSON-body sidesteps both issues.

Outbound (publishing) keeps subject-per-pane because the publisher already knows the subject — no parsing problem on that side.

## Hook touchpoints (Claude Code settings.json)

Three lifecycle hooks slot in as siblings to the existing `orch` marker hooks:

```jsonc
{
  "hooks": {
    "Stop":          [{"hooks":[ /* orch-stop-marker.sh + orch-nats-publish-stop.sh */ ]}],
    "Notification":  [{"hooks":[ /* orch-notify-marker.sh + orch-nats-publish-notify.sh */ ]}],
    "SessionStart":  [{"hooks":[ /* orch-nats-publish-jsonl.sh */ ]}]
  }
}
```

The `SessionStart` hook backgrounds a `tail -F | nats pub` of the session's JSONL transcript. Claude Code stores transcripts at `~/.claude/projects/<cwd-encoded>/<session_id>.jsonl`, where the encoding replaces both `/` AND `.` with `-` — so `/Users/x/.claude/worktrees/pr1` becomes `-Users-x--claude-worktrees-pr1` (note the `--` from the `.`). The hook is PID-gated to prevent double-spawn under `/resume`.

## Quick example: orchestrator drives 3 builders

```sh
# Spawn 3 builders, capture pane ids
PANE1=$(orch-spawn claude --cwd ./worktree1 --quiet)
PANE2=$(orch-spawn claude --cwd ./worktree2 --quiet)
PANE3=$(orch-spawn claude --cwd ./worktree3 --quiet)

# Kick off each via NATS — bridge-in subscriber routes to the right pane
nats pub orch.tell "$(jq -nc --arg p "$PANE1" '{pane:$p, prompt:"execute plan A"}')"
nats pub orch.tell "$(jq -nc --arg p "$PANE2" '{pane:$p, prompt:"execute plan B"}')"
nats pub orch.tell "$(jq -nc --arg p "$PANE3" '{pane:$p, prompt:"execute plan C"}')"

# Watch completions via NATS instead of fswatch on marker files
nats sub 'orch.stop.>' --count=3

# Live transcript stream for any builder
nats sub --raw 'orch.events.373' | jq -c 'select(.message.content[]?|.type=="text")|.message.content[].text'
```

Compared to using the bare `orch-tell` / `orch-listen` / fswatch path: equivalent capability, but the events flow over a substrate that supports multiple subscribers, can span sub-leaves, and (with JetStream) can replay.

## Sesh-affinity (current state and gaps)

The bridge as designed today is **NATS-substrate-only**. It does not currently use sesh's session container — meaning the subjects are flat, there's no JetStream replay, no per-session Fossil leaf, no session-scoped tool restrictions.

A 2026-05-12/13 experiment exercised the bridge against three autonomous builders working on a sibling repo. The bridge carried the comms end-to-end (Stops, Notifications, JSONL transcripts, prompt injection), but the builders worked in git worktrees + committed to git + pushed to GitHub — they never touched the Fossil leaf their sesh session would have provided.

The deltas between "bridge as-is" and "bridge with sesh-affinity":

| Concern | Bridge as-is | With sesh-affinity |
|---|---|---|
| NATS server | bare `nats-server` or `sesh hub serve` (uniform from the bridge's view) | sesh hub at `~/.sesh/hub.url`, attached via `~/.sesh/sessions/<label>.json` |
| Subject namespace | flat `orch.*` | session-scoped `sesh.<session>.orch.*` (via `ORCH_NATS_SUBJECT_PREFIX`) |
| Event durability | core NATS pub/sub (fire-and-forget) | JetStream stream in the session's domain, late-subscriber replay |
| Spawn integration | `orch-spawn` is sesh-naive | `sesh orch spawn <agent> --session=<label>` OR `orch-spawn --sesh-session=<label>` |
| Code substrate | git worktrees, builders commit + push directly | per-session Fossil leaf; `sesh promote` translates to git PR branches (TODO upstream) |
| Tool restrictions | spawn full-power; rely on prompt-injected interrupts | spawn-time suit-level denials (e.g., no `git push` / `gh pr create`) |

See [issue #18 — Experiment writeup: NATS-bridged orch orchestration — sesh-affinity gaps blocking full integration](https://github.com/danmestas/sesh/issues/18) for the full gap analysis and proposed work items.

The bridge is designed so that closing those gaps is mostly configuration, not rewrite:

- The publish hooks honour `ORCH_NATS_SUBJECT_PREFIX` — set it to `sesh.<session>.orch` in the spawn env and existing publish code Just Works.
- The publish hooks honour `NATS_URL` — point at `~/.sesh/sessions/<label>.json`'s `nats_url` field and the publishes route through the sesh hub.
- The publish hooks already use `--timeout=1s` so they degrade gracefully when the configured server is gone.

Spawn integration (`sesh orch spawn` / `orch-spawn --sesh-session=…`) is the missing keystone — it would set those env vars at spawn time so every spawned builder inherits the session context.

## Why this is a separate document

This sits between two repos. The transport (NATS / JetStream / hub) is sesh's responsibility; the orchestration verbs (spawn, tell, listen) are `orch`'s responsibility. The bridge — three hooks + one subscriber — adapts the second onto the first.

The implementation belongs in `orch` (its scripts live alongside `orch-tell`, etc., in `orch`'s `bin/` and `hooks/`). Documentation lives here in sesh because the bridge's wire surface is sesh's wire surface; the subjects, body schemas, and replay semantics are properties of the substrate, and a future contributor working on JetStream-on-`orch.*` or on `sesh orch spawn` needs to start from this document.

If you're touching orch's orchestration scripts, the bridge wire shape above tells you what to publish or subscribe to. If you're touching sesh's hub or session lifecycle, the bridge tells you what subjects to enable / scope / persist.

## See also

- [`coordination-patterns.md`](coordination-patterns.md) — abstract patterns (orchestrator-subagent etc.) that this bridge concretises
- [`message-envelope.md`](message-envelope.md) — base envelope conventions; the bridge bodies above are a specialisation
- [Issue #18](https://github.com/danmestas/sesh/issues/18) — sesh-affinity gaps and proposed work
- [`orch`](https://github.com/danmestas/orch) — implementation home (the bridge scripts belong in `orch/bin/` and `orch/hooks/` alongside the existing orch verbs)
