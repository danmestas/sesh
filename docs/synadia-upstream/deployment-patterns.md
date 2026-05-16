# Upstream draft — Contribution 3: Appendix D — Deployment patterns

**Target repo:** `synadia-ai/synadia-agent-sdk-docs`  
**Amends:** core-protocol.md — adds new informative Appendix D after
Appendix C.  
**Normative status:** INFORMATIVE. Nothing in this appendix introduces
new MUST/SHOULD/MAY obligations beyond those already stated in normative
sections. It describes and names recurring deployment configurations so
that implementors and consumer docs have citable vocabulary.  
**Status:** draft, ready to copy-paste into an upstream PR.

---

## Proposed spec text

---

## Appendix D: Deployment patterns (informative)

This appendix names and describes recurring configurations of NATS servers,
agents, and callers that the protocol has been observed running in. No
section here overrides or extends the normative requirements in §1–§12.

The protocol assumes a NATS server exists (§1). The patterns below describe
how that assumption is typically satisfied in practice.

---

### D.1  Standalone NATS server

The simplest configuration: one externally-operated NATS server, one or
more agents, one or more callers.

```
[Caller] ──publish/subscribe──▶ [NATS server] ◀── subscribe/publish ── [Agent]
```

Suitable for:
- Development and testing against `demo.nats.io`.
- Production deployments where the NATS operator is a separate team.
- Multi-tenant environments where all agents share account-level isolation
  via NATS authentication (§10).

The protocol makes no assumptions about who started the server or how it is
configured. Agents register their service (§3), discover each other (§4), and
communicate through it without caring about the topology.

---

### D.2  Channel-plugin pattern

An agent runtime (e.g., Claude Code via the `nats-channel` plugin) connects
to an externally-operated NATS server and registers its `prompt` endpoint
using the account name derived from the NATS credentials. No embedded server;
no auto-spawn.

```
[Claude Code + nats-channel plugin]
        │  registers agents.prompt.cc.<user>.<session>
        ▼
[NATS server (e.g., demo.nats.io or Synadia Cloud)]
        ▲
[Caller / orchestration tool]
        │  discovers via $SRV.INFO.agents
        │  publishes to agents.prompt.cc.<user>.<session>
```

Characteristic properties:

- The NATS server is external and persistent; it survives agent restarts.
- The agent derives its 4th subject token (`owner`) from the NATS account or
  a configured username, and its 5th token (`name`) from the working directory
  or a session flag.
- Multiple simultaneous agent instances share the `agents` queue group on the
  `prompt` endpoint (§3.3), giving automatic load-balancing.
- Discovery (`$SRV.PING`) reveals all instances on the account.

This pattern is the default for the `synadia-ai/nats-channel` plugin.

---

### D.3  Hub-and-leaf with auto-spawn

An agent runtime that also manages a per-user embedded NATS server (the
"hub"). The hub is started automatically on the first agent invocation and
shuts down when the last agent disconnects. Each agent session runs as a
NATS leaf node connecting to the hub.

```
First `sesh up` (auto-spawns hub if absent):

  ~/.sesh/hub.url ◀── written by hub at bind time
       │
  [Hub: embedded NATS server]   ← user-scoped singleton
       │  leaf protocol
  [Session leaf A]              ← sesh up --session=morning
       └── registers agents.prompt.<agent>.<owner>.morning

Later `sesh up` in same project (hub already running):

  [Hub]
       │  leaf protocol
  ├── [Session leaf A]          ← still running
  └── [Session leaf B]          ← sesh up --session=afternoon
            └── registers agents.prompt.<agent>.<owner>.afternoon

`sesh down --session=morning`:

  Session leaf A disconnects.
  Hub remains up while leaf B is connected.
  Hub exits when the last leaf disconnects (unless --keepalive).
```

Characteristic properties:

- The hub is a singleton per user (`~/.sesh/hub.url` is created O_EXCL; the
  second caller finds the file and dials the existing hub).
- The hub URL (`nats-leaf://...`) is written atomically at bind time, making
  it safe to race on startup.
- Each session leaf carries its own NATS client URL (`nats://...`) recorded
  in `<cwd>/.sesh/sessions/<label>.json`. Sub-processes and callers dial that
  URL directly without parsing logs.
- Session collision is local: a second `sesh up` with the same label
  finds the `.json` file already present (O_EXCL semantics) and refuses to
  start, reporting the holder PID.
- Hub auto-shutdown keeps the process table clean for interactive developer
  use. The `--keepalive` flag suppresses this for CI or long-lived daemon
  scenarios.

#### D.3.1  Worked startup sequence

```
t=0  User runs: sesh up --session=morning
t=0  sesh tries O_EXCL create of ~/.sesh/hub.url
       → success (no hub running)
t=0  sesh forks hub process; waits for hub.url to be written
t=1  Hub binds :4222 (NATS) and :7422 (leaf)
t=1  Hub writes nats-leaf://127.0.0.1:7422 to ~/.sesh/hub.url
t=1  sesh reads hub.url; dials leaf connection
t=2  Session leaf connects; claims .sesh/sessions/morning.json (O_EXCL)
t=2  Session leaf writes {pid, nats_url, leaf_url} to morning.json
t=2  Session leaf registers: agents.prompt.<agent>.<owner>.morning
t=2  Caller/tool discovers via $SRV.INFO.agents and dials nats_url
```

#### D.3.2  Sub-leaf extension

An external worker (e.g., an EdgeSync sub-leaf) can join the mesh by
connecting to the hub as a second leaf node. The session's JSON state file
exposes two URLs the sub-leaf needs: `leaf_url` for the NATS leaf connection
and `fossil_url` for the initial HTTP clone that seeds the sub-leaf's
content-addressable store. Both are required: `--leaf-upstream` carries
ongoing commits via NATS, while `--seed-from-upstream` populates the
sub-leaf with the parent session's prior state at startup. Without the
seed, the sub-leaf starts cold and misses all commits made before it
joined.

```sh
LEAF=$(jq -r .leaf_url   < .sesh/sessions/morning.json)
HTTP=$(jq -r .fossil_url < .sesh/sessions/morning.json)

edgesync hub serve \
  --repo=./.subleaf.repo \
  --leaf-upstream="$LEAF" \
  --seed-from-upstream="$HTTP"
```

The sub-leaf clones the parent's Fossil repo over HTTP at startup
(catching the seed commit plus any prior agent commits), inherits the
parent's project-code, and subscribes to ongoing commits via the leaf
NATS connection. From that point it participates in discovery and the
shared subject space without any additional configuration.

#### D.3.3  Distinguishing from channel-plugin

| Property                | Channel-plugin (D.2)             | Hub-and-leaf auto-spawn (D.3)              |
|-------------------------|----------------------------------|--------------------------------------------|
| NATS server             | External, persistent             | Embedded in hub process, auto-spawned      |
| Server lifetime         | Independent of agents            | Tied to agent sessions (`--keepalive` opt) |
| Discovery scope         | All agents on the NATS account   | All agents dialing this hub                |
| Credential management   | NATS credentials file / context  | No credentials (loopback or tailnet)       |
| Multi-machine           | Yes (shared server)              | One hub per machine; cross-machine via leaf federation |
| Typical use             | Cloud / shared NATS account      | Local developer workstation, CI worker     |

---

### D.4  Hub federation (informative sketch)

> **Sketch only.** This subsection is not ready for upstream submission
> as-is. It outlines the topology at a conceptual level but does not yet
> carry a concrete worked example (subjects, leaf node config snippet,
> service-discovery flow across hubs). Either fill that in before
> submitting, or split D.4 out of the initial PR and propose it
> separately.

Multiple hubs on different machines may be connected via NATS leaf federation,
making agents registered on hub A visible to callers on hub B. This pattern
is not specified by the protocol; it relies entirely on NATS's built-in leaf
node routing. An agent registered on `agents.prompt.claude-code.aconnolly.morning`
on hub A appears at the same subject on hub B once the leaf connection is
established.

This pattern is mentioned here because it is the natural extension of D.3 to
multi-machine or multi-user deployments; implementors should consult the NATS
documentation on leaf node configuration for details.

---

## Appendix D — Quick reference

| Pattern            | Server origin        | Auto-spawn | Per-session state file | Typical user     |
|--------------------|----------------------|------------|------------------------|------------------|
| D.1 Standalone     | External             | No         | No                     | Production ops   |
| D.2 Channel-plugin | External             | No         | No                     | Cloud / shared   |
| D.3 Hub-and-leaf   | Embedded (auto)      | Yes        | Yes (O_EXCL JSON)      | Developer / CI   |
| D.4 Hub federation | Embedded + federated | Yes (each) | Yes (per hub)          | Multi-machine    |

---

*End of Appendix D.*
