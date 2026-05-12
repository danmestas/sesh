# Multi-agent coordination patterns on sesh

Sesh is a neutral transport: a NATS + Fossil mesh organized as a tree of
leaves under a per-user hub. It does not implement any specific
coordination pattern. Every well-known multi-agent pattern in current
literature maps onto its primitives — this document names which sesh
primitives carry each pattern and gives a minimum wire-level sketch.

Six patterns covered, deduplicated from current sources:

| Pattern                   | When to use                                                         |
| ------------------------- | ------------------------------------------------------------------- |
| Generator–verifier        | Quality-critical output with explicit evaluation criteria           |
| Orchestrator–subagent     | Clear task decomposition, bounded subtasks                          |
| Hierarchical multi-tier   | Workflows that decompose recursively across multiple levels         |
| Agent teams               | Parallel, independent, long-running subtasks                        |
| Message bus               | Event-driven pipelines with a growing agent ecosystem               |
| Shared state / blackboard | Collaborative work where agents build on each other's findings      |

Snippets below are illustrative pseudocode using the [`nats` CLI](https://github.com/nats-io/natscli);
they encode the wire shape, not copy-paste-runnable scripts.

---

## Sesh primitives at a glance

| Primitive                | Where lives                                          | What it gives you                                       |
| ------------------------ | ---------------------------------------------------- | ------------------------------------------------------- |
| NATS pub/sub             | Any sesh's `nats_url`                                | Subject-based fan-out with wildcards                    |
| Queue groups             | NATS clients                                         | Load-balanced delivery to one of N same-named workers   |
| Request/reply            | NATS clients                                         | Correlated RPC with timeouts                            |
| JetStream streams        | Hub + per-session messaging dirs                     | Persistent ordered subject logs                         |
| JetStream consumers      | On streams                                           | Durable cursors, ACK semantics, max-deliver, ack-wait   |
| JetStream KV             | On streams                                           | Structured KV with CAS and change watchers              |
| JetStream Object Store   | On streams                                           | Versioned blob store                                    |
| Fossil repo              | Hub + per-session                                    | Content-addressed VCS, sync'd over NATS via EdgeSync    |
| Leaf tree                | Topology                                             | Hierarchical fan-out with upstream interest propagation |

Each `sesh up` publishes its `nats_url` and `leaf_url` in
`<cwd>/.sesh/sessions/<label>.json`. Clients and sub-leaves attach by
reading that file (see the README for the basic flow).

---

## Pattern 1: Generator–verifier

A generator produces an initial output. A verifier evaluates it against
explicit criteria and either accepts it or rejects it with feedback. The
loop terminates on acceptance or after a bounded number of attempts.

**When to use** — quality-critical output (customer emails, code diffs,
generated documents) where you can name the evaluation criteria.

**Wire shape** — two roles, one task per round. Subjects:

```
gen.<task-id>.draft       generator publishes each attempt
verify.<task-id>.verdict  verifier publishes accepted | rejected+feedback
```

Synchronous variant via request/reply:

```sh
# Generator side
draft=$(produce-something "$task")
verdict=$(nats request verify.$task.draft "$draft" --timeout=30s)
if [ "$(jq -r .accepted <<<"$verdict")" = "true" ]; then
  echo "$draft" > final.txt
else
  feedback=$(jq -r .feedback <<<"$verdict")
  # iterate with feedback included in the next prompt
fi

# Verifier side
nats reply 'verify.*.draft' '<evaluation result as JSON>'
```

Asynchronous variant with audit trail:

```sh
# Once per task: a stream that records every attempt
nats stream add audit-$task --subjects "audit.$task.>"

# Generator publishes drafts
nats pub audit.$task.draft.attempt-N "$payload"

# Verifier subscribes and publishes verdicts on the same stream
nats sub "audit.$task.draft.>"
nats pub audit.$task.verdict.N "$verdict"
```

The audit stream is replayable for post-hoc analysis.

**Trust note** — without NATS auth, any client can publish a "verdict".
For behavioral verification (system-prompt-constrained verifier agent),
this is fine. For authoritative verdicts that gate real-world action, see
[Gaps and extensions](#gaps-and-extensions).

---

## Pattern 2: Orchestrator–subagent

An orchestrator decides how to decompose a task. It dispatches subtasks to
specialized subagents, then synthesizes results. Subagents stay focused on
their narrow responsibility; the orchestrator keeps the overall context.

**When to use** — task decomposition is clear and subtasks have minimal
interdependence (automated code review, multi-step analysis, parallel
research).

**Wire shape** — one orchestrator session, N subagent sessions or
processes. Subjects:

```
dispatch.<role>.<task-id>  orchestrator → subagent
result.<task-id>           subagent → orchestrator (or via req/rep)
```

Parallel fan-out:

```sh
# Orchestrator
nats pub dispatch.researcher.t-001 '{"query":"AI safety literature"}'
nats pub dispatch.coder.t-001     '{"spec":"implement foo"}'
nats pub dispatch.reviewer.t-001  '{"target":"PR #123"}'

# Collect (one message per dispatched subtask)
nats sub 'result.t-001' --count=3 --timeout=120s
```

Sequential is the same shape with one subagent's `result` feeding the
next's `dispatch`.

**Queue groups for worker pools** — multiple subagent processes registered
with the same queue group give load-balancing for free:

```sh
# Three researcher processes on different sesh leaves, same group
nats sub 'dispatch.researcher.>' --queue=researchers
```

One dispatch goes to exactly one of them. Add more processes to scale
throughput.

---

## Pattern 3: Hierarchical multi-tier

A top-tier orchestrator delegates to mid-tier managers, who delegate to
workers. Each tier owns its own abstraction. This is sesh's leaf tree —
the pattern matches the topology directly.

**When to use** — workflows that decompose recursively; large enough that
mid-tier coordination is itself worth abstracting (enterprise document
processing, multi-stage data pipelines with sub-pipelines).

**Wire shape** — each tier runs its own embedded hub:

```
hub (~/.sesh)
├── sesh "orchestrator"                       ← top tier
│   ├── edgesync sub-leaf "research-mgr"      ← mid tier
│   │   ├── worker process: web-scraper
│   │   └── worker process: paper-reader
│   └── edgesync sub-leaf "content-mgr"
│       ├── worker process: writer
│       └── worker process: editor
└── sesh "monitor"                            ← peer at top tier (observer)
```

Sub-leaves are spawned with the parent's `leaf_url`:

```sh
ALPHA_LEAF=$(jq -r .leaf_url < .sesh/sessions/alpha.json)
edgesync hub serve --leaf-upstream="$ALPHA_LEAF" \
                   --http-port=0 --nats-client-port=0 --nats-leaf-port=0
```

Subject scoping per tier:

```
t1.>                       all top-tier traffic
t1.research.>              mid-tier under research-mgr
t1.research.web.>          worker tier under web-scraper
t1.research.result.>       aggregated results bubbling back up
```

Bubble-up flow: a worker publishes `t1.research.web.result.<id>`; the
research-mgr subscribes to `t1.research.web.result.>` and aggregates,
then publishes `t1.research.result.<id>`; the orchestrator subscribes to
`t1.research.result.>` for complete research output.

Hierarchy depth is unbounded — each tier just nests one more sub-leaf.

---

## Pattern 4: Agent teams

Each teammate works autonomously on its own subtask for an extended
period, building up context as it goes. A coordinator distributes work
and watches for completion signals.

**When to use** — parallel subtasks that benefit from sustained,
multi-step work with their own state (per-service migration, per-bug
triage, parallel test-suite repair).

**Wire shape** — one `sesh up` per teammate. Work distribution via a
JetStream stream with pull consumers; durable per-teammate state lives in
that session's local JetStream.

```sh
# Coordinator session: create the work queue once
nats stream add team-work --subjects 'team.work.>' --storage=file
nats consumer add team-work workers \
    --filter 'team.work.>' --pull \
    --max-deliver=3 --ack-wait=30s

# Coordinator publishes work items
nats pub team.work.migrate-service '{"service":"auth"}'
nats pub team.work.migrate-service '{"service":"billing"}'

# Each teammate pulls
while true; do
  msg=$(nats consumer next team-work workers --raw)
  process "$msg"          # the next job, ACK'd implicitly by the CLI
done

# Teammates publish milestones
nats pub team.milestones.teammate-1 '{"status":"deps-updated","service":"auth"}'

# Coordinator watches all milestones
nats sub 'team.milestones.>'
```

`--max-deliver=3` retries on nack; messages that exhaust retries land in
the stream's DLQ (configurable).

**Durable per-teammate state** — a teammate's working state lives in its
session's JetStream under `<cwd>/.sesh/sessions/<label>.messaging/`. If
the teammate crashes, the next `sesh up --session=<same-label>` from the
same cwd resumes it.

---

## Pattern 5: Message bus

NATS subjects are a message bus by default. Events flow; agents subscribe
to whatever they care about. New agent types plug in without changing
existing ones.

**When to use** — event-driven workflows where the next step emerges from
the event rather than a fixed plan; ecosystems of agent types that will
grow (security alert triage, multi-channel notifications, data
processing).

**Wire shape** — subject design IS the bus design.

```sh
# Triage agent classifies and routes
nats sub 'alert.raw.>'
# After classification, republish on a more specific subject
nats pub alert.network.high incident-data
nats pub alert.identity.medium incident-data

# Specialized investigators subscribe to their lane
nats sub 'alert.network.>'    # network agent
nats sub 'alert.identity.>'   # identity agent

# Investigators may request enrichment
nats pub enrich.geo.request '{"ip":"1.2.3.4"}'

# Enrichment agents subscribe and reply
nats sub 'enrich.>.request'
```

**Durable pipelines with retries** — back each stage with a JetStream
stream:

```sh
nats stream add alerts \
    --subjects 'alert.>' --storage=file --retention=workqueue
nats consumer add alerts triagers \
    --filter 'alert.raw.>' --pull --max-deliver=3
```

`retention=workqueue` deletes messages once a consumer ACKs. For replay
and observability, use `retention=limits` instead.

**Subject design conventions**

- Hierarchy reflects routing: `alert.<lane>.<state>`
- Verbs at the leaf: `alert.network.detected`, `alert.network.investigated`
- Reserve `_sys.>` for cross-cutting concerns (traces, metrics, health)

---

## Pattern 6: Shared state / blackboard

Agents collaborate via a shared store. No single agent owns the workflow;
each reads what others have produced and adds its contribution.

**When to use** — collaborative research, design exploration, multi-stage
analysis where contributions don't partition cleanly by subtask.

Sesh has two complementary stores in the substrate, both already wired:

### JetStream KV — structured state with watchers

```sh
# Create a KV bucket
nats kv add scratchpad

# Agent A writes
nats kv put scratchpad plan '{"phase":"research","next":"draft"}'

# Agent B reacts to changes
nats kv watch scratchpad plan
# This is a subscription on $KV.scratchpad.plan;
# change events arrive as messages with old + new values.
```

Optimistic concurrency via CAS:

```sh
nats kv put scratchpad plan "$new_value" --revision $prev_rev
# Fails if the revision moved; agent re-reads and retries.
```

Use KV when the shared state is small and the question is "what is X
right now."

### Fossil repo — content-addressable artifacts

Every sesh leaf has a Fossil repo at `<cwd>/.sesh/sessions/<label>.repo`.
The hub has `~/.sesh/hub.repo`. EdgeSync syncs them over NATS. Agents
commit artifacts (research docs, code, diffs, datasets) to their local
repo; the change syncs to the hub and to any interested leaf.

Pattern: commit the artifact to Fossil, announce the pointer on NATS.

```sh
# Agent A commits research output
fossil commit -m "research findings" research.md
revid=$(fossil info | grep checkout | awk '{print $2}')

# Announce the pointer
nats pub blackboard.update.research \
  "$(jq -n --arg r "$revid" --arg p research.md '{rev:$r, path:$p}')"

# Agent B subscribes
nats sub 'blackboard.update.>'
# On message: fossil pull, then read research.md at $rev
```

Use Fossil when shared state is content-addressed and history matters.
Most multi-agent systems hand-roll this on S3 or Postgres; sesh has it
natively via EdgeSync's sync engine.

---

## Combining patterns

Production systems mix patterns. Common hybrids:

- **Orchestrator–subagent with shared-state subtask** — orchestrator
  dispatches discrete tasks; one subtask is a collaborative analysis that
  uses the blackboard.
- **Message bus with agent-team workers** — events route to lanes; each
  lane has a team of long-running workers with durable state.
- **Hierarchical with generator–verifier at each tier** — mid-tier
  managers verify their workers' output before aggregating up.

Sesh doesn't constrain the mix. The substrate is the same regardless of
what pattern you wire on top.

---

## Gaps and extensions

Three things sesh doesn't give you today; if you hit them, here's the
shape of the fix.

### No identity / signed messages

Any client on the mesh can publish on any subject. Behavioral isolation
works fine (constrain a "spy" agent via its system prompt or rule set);
enforced trust requires NATS auth. The path is per-role nkeys, optional
account-per-project for hard isolation. Add when generator-verifier
verdicts gain authority over real-world action, or when you need
multi-tenant separation.

### No distributed tracing

Each sesh writes slog locally. For multi-stage pipelines you'll want
trace propagation across messages. See
[message-envelope.md](./message-envelope.md) for the recommended
convention (NATS headers + W3C `traceparent`). A tracing consumer that
converts incoming `traceparent` headers to OTLP spans and exports them
is ~50 lines of Go; no sesh changes required.

### Single-machine hub

Sesh auto-spawns the hub at `localhost`. To split agent teams across
machines, either point sesh at a remote leaf URL (sesh would need to
learn `SESH_HUB_URL=nats-leaf://hub.host:7422`) or run NATS clustering on
the hub. Small extension when geo-distribution becomes a requirement.

---

## Further reading

- [Anthropic — Multi-agent coordination patterns: Five approaches and when to use them](https://claude.com/blog/multi-agent-coordination-patterns)
- [Microsoft AZD-for-beginners — Multi-Agent Coordination Patterns](https://github.com/microsoft/AZD-for-beginners/blob/main/docs/chapter-06-pre-deployment/coordination-patterns.md)
- [NATS by Example](https://natsbyexample.com/)
- [NATS JetStream concepts](https://docs.nats.io/nats-concepts/jetstream)
