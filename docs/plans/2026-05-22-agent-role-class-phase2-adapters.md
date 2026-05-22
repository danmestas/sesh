# Agent Role & Class Registration — Phase 2 (NATS-channel adapters) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each `*-nats-channel` adapter reads `$SESH_ROLE` and `$SESH_CLASS` at boot and sets the corresponding `metadata.role` / `metadata.class` fields on its NATS Micro service registration, so role-aware routing (per the coordination-subjects proposal) works end-to-end.

**Architecture:** Each adapter repo (`claude-nats-channel`, `pi-nats-channel`, `omp-nats-channel`, `grok-nats-channel`, `gemini-nats-channel`) follows the same pattern: read two env vars at adapter startup, validate, attach to the `AgentService` constructor `metadata` (via the underlying NATS Micro service). The reference implementation lands in `claude-nats-channel` first (Phase 1 acceptance criterion); the other four are mechanical copies.

**Tech Stack:** TypeScript, `@synadia-ai/agent-service`, `bun` (Node ≥ 20 also works), Vitest or Bun's test runner.

**Source proposal:** `docs/proposals/2026-05-21-agent-role-registration.md` (Phase 2, lines 131-133)

**Depends on:** Phase 1 plan (sesh-side parsing). Without Phase 1, the metadata is emitted but nothing on the sesh side reads it.

---

## Cross-Repo Context

The adapter repos are siblings of sesh. Their canonical locations on the operator's machine:

- `~/projects/claude-nats-channel/` (or `~/projects/agent-channels/agents/claude-code/` depending on monorepo layout)
- `~/projects/pi-nats-channel/`
- `~/projects/omp-nats-channel/`
- `~/projects/grok-nats-channel/` (does NOT yet exist — see `docs/specs/2026-05-19-grok-synadia-nats-channel-*.md`; once that adapter is built, it must follow this plan from day one)
- `~/projects/gemini-nats-channel/`

The acceptance test from Phase 1 only requires **claude-nats-channel** to be updated. Tasks 5-8 below replicate to the others.

---

## File Structure (per adapter)

Most adapters share the same TypeScript shape. Touch points per adapter repo:

**Modify:**
- `src/server.ts` (or `src/index.ts` / `src/agent.ts` — whichever bootstraps `AgentService`) — add env reads + validation, wire into `AgentService` `metadata`
- `src/config.ts` (if it exists, else create) — centralize env reads
- `README.md` — document the two new env vars in the configuration section

**Create:**
- `src/config.ts` — only if config logic is currently inline in `server.ts`

**Test:**
- `test/config.test.ts` — unit tests for env reads + validation
- `test/integration.test.ts` — confirms `$SRV.INFO.agents` response includes the metadata

---

## Task 1: claude-nats-channel — extract config module with role and class

**Repo:** `~/projects/claude-nats-channel/`

**Files:**
- Create: `src/config.ts`
- Test: `test/config.test.ts`

- [ ] **Step 1: Locate the current env-var reading site**

Run: `cd ~/projects/claude-nats-channel && grep -rn 'process.env' src/ | head -20`

Identify the file (likely `src/server.ts`) that reads `process.env.NATS_URL` and similar. If env reads are scattered, prefer extracting them into a new `src/config.ts`. If a `config.ts` already exists, extend it instead of creating a new one.

- [ ] **Step 2: Write the failing config test**

File: `test/config.test.ts`

```ts
import { describe, expect, test, beforeEach, afterEach } from "vitest"; // or bun:test
import { readConfig, validateRole, validateClass, ConfigError } from "../src/config";

describe("config", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
  });

  test("reads SESH_ROLE and SESH_CLASS from env", () => {
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const cfg = readConfig();
    expect(cfg.role).toBe("implementer");
    expect(cfg.class).toBe("active");
  });

  test("defaults role=worker class=active when unset", () => {
    delete process.env.SESH_ROLE;
    delete process.env.SESH_CLASS;
    const cfg = readConfig();
    expect(cfg.role).toBe("worker");
    expect(cfg.class).toBe("active");
  });

  test("validateRole rejects uppercase, spaces, slashes, too-long", () => {
    expect(() => validateRole("BadCase")).toThrow(/must match/);
    expect(() => validateRole("has space")).toThrow(/must match/);
    expect(() => validateRole("slash/role")).toThrow(/must match/);
    expect(() => validateRole("a".repeat(64))).toThrow(/max 63/);
  });

  test("validateClass accepts active/observer, rejects others", () => {
    expect(() => validateClass("active")).not.toThrow();
    expect(() => validateClass("observer")).not.toThrow();
    expect(() => validateClass("passive")).toThrow(/active|observer/);
  });

  test("readConfig throws if SESH_CLASS is invalid", () => {
    process.env.SESH_CLASS = "passive";
    expect(() => readConfig()).toThrow(ConfigError);
  });
});
```

- [ ] **Step 3: Run test to verify it fails**

Run: `bun test test/config.test.ts` (or `npx vitest run test/config.test.ts`)

Expected: FAIL — `Cannot find module '../src/config'`.

- [ ] **Step 4: Create the config module**

File: `src/config.ts`

```ts
// SOURCE OF TRUTH for the role/class rules below:
//   https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md
//   ("Canonical role/class rules" section)
//
// Do NOT diverge from sesh's canonical rules — the proposal mandates that
// every adapter port the same regex, length bound, default, and class enum
// verbatim. If the proposal changes, update this file and bump the
// reference URL above.

export class ConfigError extends Error {}

export type AgentClass = "active" | "observer";

export interface AdapterConfig {
  natsUrl: string;
  agent: string;
  owner: string;
  session: string;
  role: string;
  class: AgentClass;
}

const ROLE_RE = /^[a-z0-9_-]+$/;

export function validateRole(role: string): void {
  if (role.length === 0) throw new ConfigError("role is empty");
  if (role.length > 63) throw new ConfigError(`role ${JSON.stringify(role)} max 63 bytes`);
  if (!ROLE_RE.test(role)) throw new ConfigError(`role ${JSON.stringify(role)} must match ^[a-z0-9_-]+$`);
}

export function validateClass(cls: string): asserts cls is AgentClass {
  if (cls !== "active" && cls !== "observer") {
    throw new ConfigError(`class ${JSON.stringify(cls)} must be "active" or "observer"`);
  }
}

export function readConfig(): AdapterConfig {
  const natsUrl = process.env.NATS_URL ?? "nats://localhost:4222";
  const agent = process.env.SESH_AGENT ?? "claude-code"; // adapter-specific default
  const owner = process.env.SESH_OWNER ?? process.env.USER ?? "anon";
  const session = process.env.SESH_SESSION ?? "";
  const role = (process.env.SESH_ROLE ?? "").trim() || "worker";
  const cls = (process.env.SESH_CLASS ?? "").trim() || "active";

  validateRole(role);
  validateClass(cls); // narrows cls to AgentClass

  return { natsUrl, agent, owner, session, role, class: cls };
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `bun test test/config.test.ts`

Expected: PASS — 5 tests pass.

- [ ] **Step 6: Checkpoint your progress**

Stage `src/config.ts` and `test/config.test.ts`. Commit: `feat(config): extract adapter config with SESH_ROLE and SESH_CLASS env reads + validation`.

---

## Task 2: claude-nats-channel — wire role and class into AgentService metadata

**Files:**
- Modify: `src/server.ts` (or wherever `new AgentService({...})` is constructed)
- Test: `test/integration.test.ts`

- [ ] **Step 1: Locate the AgentService construction**

Run: `grep -rn 'new AgentService' src/`

Find the bootstrapping site. It will look something like:

```ts
const service = new AgentService({
  nc,
  agent: "claude-code",
  owner: "dmestas",
  name: "main",
});
```

- [ ] **Step 2: Write the failing integration test**

File: `test/integration.test.ts` (extend if it exists)

```ts
import { describe, expect, test, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { connect, NatsConnection } from "@nats-io/transport-node";
import { AgentService } from "@synadia-ai/agent-service";
import { spawn, ChildProcess } from "node:child_process";
import { readConfig } from "../src/config";

// Spawn a real nats-server binary on a random port for the duration of the
// suite. CI must have `nats-server` on PATH (install via Docker layer, brew,
// or the bun setup script). Mirrors Phase 1's in-process Go pattern.
let serverProc: ChildProcess | null = null;
let serverUrl = "";

async function startServer(): Promise<string> {
  return new Promise((resolve, reject) => {
    const proc = spawn("nats-server", ["-p", "-1"], { stdio: ["ignore", "pipe", "pipe"] });
    let resolved = false;
    proc.stderr?.on("data", (chunk) => {
      const line = chunk.toString();
      // nats-server logs the chosen port on the "Listening for client connections on" line.
      const m = line.match(/Listening for client connections on 0\.0\.0\.0:(\d+)/);
      if (m && !resolved) {
        resolved = true;
        serverProc = proc;
        resolve(`nats://127.0.0.1:${m[1]}`);
      }
    });
    proc.on("error", reject);
    setTimeout(() => {
      if (!resolved) reject(new Error("nats-server did not start within 5s"));
    }, 5000);
  });
}

describe("AgentService metadata includes role/class", () => {
  let nc: NatsConnection;

  beforeAll(async () => {
    serverUrl = await startServer();
  });

  afterAll(() => {
    serverProc?.kill();
  });

  beforeEach(async () => {
    nc = await connect({ servers: serverUrl });
  });

  afterEach(async () => {
    await nc.close();
  });

  test("metadata.role and metadata.class appear in $SRV.INFO response", async () => {
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const cfg = readConfig();

    const svc = new AgentService({
      nc,
      agent: cfg.agent,
      owner: cfg.owner,
      name: "test-instance",
      metadata: {
        role: cfg.role,
        class: cfg.class,
        ...(cfg.session ? { session: cfg.session } : {}),
      },
    });
    svc.onPrompt(async (_env, res) => res.send("ok"));
    await svc.start();

    try {
      const reply = await nc.request("$SRV.INFO.agents", new Uint8Array(), { timeout: 1000 });
      const info = JSON.parse(new TextDecoder().decode(reply.data));
      expect(info.metadata.role).toBe("implementer");
      expect(info.metadata.class).toBe("active");
    } finally {
      await svc.stop();
    }
  });
});
```

- [ ] **Step 3: Run test to verify it fails or skips**

Run: `bun test test/integration.test.ts`

Expected: FAIL — adapter doesn't pass `metadata` yet (or `AgentService` constructor doesn't accept it). If the test requires an external `nats-server`, document the prerequisite in the test or use an in-process server alternative.

- [ ] **Step 4: Wire role/class into AgentService at the bootstrap site**

In `src/server.ts`, replace the `AgentService` construction:

```ts
import { readConfig } from "./config";

async function main() {
  const cfg = readConfig();
  const nc = await connect({ servers: cfg.natsUrl });

  const service = new AgentService({
    nc,
    agent: cfg.agent,
    owner: cfg.owner,
    name: cfg.session || "main",
    description: `${cfg.agent} adapter`,
    metadata: {
      role: cfg.role,
      class: cfg.class,
      ...(cfg.session ? { session: cfg.session } : {}),
    },
  });

  service.onPrompt(async (envelope, response) => {
    // existing handler — unchanged
  });

  await service.start();
  // existing shutdown wiring — unchanged
}
```

**Note:** `@synadia-ai/agent-service` accepts arbitrary keys in the top-level `metadata` option (verified by the spec's "metadata extension" pattern; see `~/references/synadia-agents/README.md`). If a particular adapter version of the SDK does not, file an upstream issue and use `extraEndpoints[].metadata` as a fallback — but prefer top-level.

- [ ] **Step 5: Run the integration test to verify it passes**

Run: `bun test test/integration.test.ts`

Expected: PASS — `info.metadata.role === "implementer"` and `info.metadata.class === "active"`.

- [ ] **Step 6: Run the full test suite**

Run: `bun test`

Expected: all PASS. Existing tests that construct `AgentService` without the new metadata still pass because role/class default to `worker` / `active`.

- [ ] **Step 7: Checkpoint your progress**

Commit: `feat(server): emit metadata.role and metadata.class on AgentService registration`.

---

## Task 3: claude-nats-channel — README documents the env vars

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Locate the existing "Configuration" or "Environment variables" section**

Run: `grep -n -i 'environment\|configuration\|env var' README.md`

- [ ] **Step 2: Add SESH_ROLE and SESH_CLASS to the table**

Append (or insert into the existing env-var table) these rows:

```markdown
| `SESH_ROLE`  | optional | `worker` | Free-form role token (`^[a-z0-9_-]+$`, 1–63 chars). Identifies the function this agent plays in the swarm — e.g. `implementer`, `verifier`, `spy`. Surfaced as `metadata.role` on the NATS Micro service and as `role` in sesh's session manifest. |
| `SESH_CLASS` | optional | `active` | One of `active` or `observer`. Coordination-subject routing keys on this: `active` agents subscribe to `workers.*`, `observer` agents subscribe to `spies.*`. |
```

- [ ] **Step 3: Checkpoint your progress**

Commit: `docs(readme): document SESH_ROLE and SESH_CLASS env vars`.

---

## Task 4: claude-nats-channel — end-to-end against sesh

**Files:** (no edits — verification only against a live sesh session)

- [ ] **Step 1: Bring up sesh with Phase 1 patches**

Pre-req: Phase 1 plan is merged and `sesh` binary on `$PATH` includes the role/class parsing.

Run: `cd ~/projects/sesh && sesh up rc-e2e`

- [ ] **Step 2: Launch the adapter with role/class set**

Run: `SESH_ROLE=implementer SESH_CLASS=active SESH_SESSION=rc-e2e bun run src/server.ts`

Wait ~2s for registration + watcher pickup.

- [ ] **Step 3: Read the session manifest**

Run: `cat ~/projects/sesh/.sesh/sessions/rc-e2e.json | jq '.agents'`

Expected output (excerpt):

```json
[
  {
    "agent": "claude-code",
    "owner": "<your-USER>",
    "instance_id": "<some-id>",
    "subject": "agents.prompt.claude-code.<owner>.rc-e2e",
    "role": "implementer",
    "class": "active"
  }
]
```

- [ ] **Step 4: Re-launch with SESH_CLASS=observer and verify**

Stop the adapter (`Ctrl+C`), then:

Run: `SESH_ROLE=spy SESH_CLASS=observer SESH_SESSION=rc-e2e bun run src/server.ts`

Wait 2s. Run: `cat ~/projects/sesh/.sesh/sessions/rc-e2e.json | jq '.agents[].class'`

Expected: `"observer"`.

- [ ] **Step 5: Tear down**

Run: `sesh down rc-e2e`

- [ ] **Step 6: Checkpoint your progress**

If everything passes, no commit needed (this is verification). Open a PR for Tasks 1-3 against `claude-nats-channel`'s `main`.

---

## Task 5: Replicate to pi-nats-channel

**Repo:** `~/projects/pi-nats-channel/`

- [ ] **Step 1: Apply Tasks 1-3 verbatim**

Same `src/config.ts`, same constructor wiring (replace `"claude-code"` default with `"pi"` if the adapter sets one), same README update. The validation logic is identical.

- [ ] **Step 2: Run the adapter's existing test suite**

Run: `cd ~/projects/pi-nats-channel && bun test`

Expected: all PASS.

- [ ] **Step 3: End-to-end verification against sesh**

Run: `SESH_ROLE=verifier SESH_CLASS=active SESH_SESSION=rc-e2e bun run src/server.ts` and confirm via `jq` that the agent appears with the right metadata.

- [ ] **Step 4: Checkpoint**

Open PR against `pi-nats-channel/main`.

---

## Task 6: Replicate to omp-nats-channel

**Repo:** `~/projects/omp-nats-channel/`

- [ ] **Step 1: Apply Tasks 1-3 verbatim** (default `agent` value: `omp`)
- [ ] **Step 2: Run adapter tests**
- [ ] **Step 3: End-to-end verification**
- [ ] **Step 4: Open PR**

---

## Task 7: Replicate to gemini-nats-channel

**Repo:** `~/projects/gemini-nats-channel/`

- [ ] **Step 1: Apply Tasks 1-3 verbatim** (default `agent` value: `gemini`)
- [ ] **Step 2: Run adapter tests**
- [ ] **Step 3: End-to-end verification**
- [ ] **Step 4: Open PR**

---

## Task 8: Grok adapter — fold into Phase 1 of grok-nats-channel build

**Repo:** `~/projects/grok-nats-channel/` (does not yet exist as of 2026-05-22)

This adapter is being built fresh per `docs/specs/2026-05-19-grok-synadia-nats-channel-*.md`. Rather than retrofit, **the role/class wiring lands as part of the initial implementation** — the spec's "Task 3: server.ts" step already covers `AgentService` construction; add `metadata.role` / `metadata.class` from `readConfig()` at that point. Cite this plan in the grok spec's Task 3.

- [ ] **Step 1: Edit `docs/specs/2026-05-19-grok-synadia-nats-channel-implementation-plan.md`**

In Task 3 ("server.ts"), add the env-var reads + `metadata.role` / `metadata.class` wiring per `src/config.ts` shape in Task 1 of this plan.

- [ ] **Step 2: Checkpoint**

Commit (in sesh repo): `docs(specs/grok): require SESH_ROLE/SESH_CLASS in initial implementation`.

---

## Acceptance

- [x] claude-nats-channel reads `SESH_ROLE` / `SESH_CLASS` at boot. → Task 1
- [x] claude-nats-channel emits `metadata.role` / `metadata.class` on registration. → Task 2
- [x] claude-nats-channel README documents the new env vars. → Task 3
- [x] claude-nats-channel passes the Phase 1 acceptance criterion (sesh-side test sees the metadata). → Task 4
- [x] pi-nats-channel, omp-nats-channel, gemini-nats-channel updated. → Tasks 5-7
- [x] grok-nats-channel spec amended so the new adapter ships with role/class from day one. → Task 8

---

## Out of Scope

- **Phase 1 (sesh core):** see `2026-05-22-agent-role-class-phase1-sesh.md`.
- **Phase 3 (orch-spawn env export):** see `2026-05-22-agent-role-class-phase3-orch-spawn.md`.
- **Phase 4 (sesh up --exec):** gated on sesh#89.
