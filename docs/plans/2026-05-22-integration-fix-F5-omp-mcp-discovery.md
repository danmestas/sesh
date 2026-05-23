# F5 — OMP's MCP discoveryMode autoloads any project `.mcp.json`

**Date:** 2026-05-22
**Status:** AFK-ready
**Severity:** P2 — startup-time consistency issue
**Owner:** sesh-channels (`claude-nats-channel`) for idempotent registration + sesh rig docs

## Root cause

OMP's config exposes `mcp.discoveryMode: true` (per Oh My Pi's own design; see `~/projects/sesh-channels/omp-nats-channel/README.md` and the rig's `test/integration/config/omp-config.yml`). When set, OMP auto-discovers any `.mcp.json` file in its working directory and instantiates the listed MCP servers inside OMP's own process.

When the rig had a `/workspace/.mcp.json` with `mcpServers.nats` (intended for claude-code), OMP discovered it too and started a *second* `claude-nats-channel/server.ts` subprocess as an MCP server inside OMP. That second instance registered on the bus as a third `agents`-service entry, even though only two agents (`cc` + `op`) were intended.

The rig works around it by removing `/workspace/.mcp.json` (claude's MCP config is at `/opt/claude.mcp.json` via `--strict-mcp-config --mcp-config`). But this is a real co-location hazard for any operator who runs both claude + OMP in the same workspace.

## Three sub-issues here

### F5(a) — Documentation gap: claude + OMP co-located workspace

The pattern "claude reads .mcp.json from the project, OMP reads .mcp.json from the project, they fight over the same file" is foreseeable. Document it.

### F5(b) — `claude-nats-channel` is not idempotent

If OMP and claude both spawn `claude-nats-channel/server.ts` against the same (agent, owner, name) triple, both register on the bus. There's no detection that another instance already owns that subject. The collision-detect logic in `server.ts:322-351 resolveSessionName` auto-suffixes names (`-2`, `-3`) when it sees another service on the same agent+owner+name, so the second instance registers as `cc.<owner>.<name>-2` rather than failing — making the duplicate silent rather than loud.

### F5(c) — OMP's MCP autodiscovery has no exclusion mechanism

This is an upstream OMP feature gap. Per CLAUDE.md no-third-party-filing, we don't file at oh-my-pi/pi-coding-agent. Surface as an operator note.

## Alternatives considered

### Option A — Docs only (rig README + claude-nats-channel README)

Document the co-location hazard and the workaround: use `--mcp-config /path/to/file.json` for claude (not `.mcp.json`), and place OMP's extensions at absolute paths in `~/.omp/agent/config.yml` (already required by OMP's design).

**Interface complexity:** docs only.
**Blast radius:** docs only.
**Risk:** real-world co-location keeps biting operators who don't read the docs.

### Option B — Make `claude-nats-channel` idempotent

Detect a same-triple existing service before registering; refuse with a clear error rather than auto-suffixing into the duplicate. This is defense-in-depth: even if the operator misconfigures, the bus stays clean.

**Interface complexity:** moderate — change `resolveSessionName` to either fail-fast or use a "claim by triple" semantic.
**Blast radius:** claude-nats-channel only; potentially also OMP/grok/gemini/pi if we generalize.
**Reversibility:** medium — flag-gated and reversible if it bites legitimate use-cases.
**Risk:** the current auto-suffix behavior is documented as "Multiple sessions: if the default name is already taken by another claude-code instance owned by the same user, the plugin auto-appends `-2`, `-3`, etc." (README line ~152). Changing it would break that documented behavior.

### Option C — Add a `claude-nats-channel` config flag `strict: true` that disables auto-suffixing

Opt-in stricter mode for operators who want the loud failure. Default behavior is unchanged.

**Interface complexity:** small — one new config key, one branch in `resolveSessionName`.
**Blast radius:** claude-nats-channel only.
**Reversibility:** easy.
**Risk:** small — adds API surface but it's opt-in.

### Chosen approach — Option A (docs) + Option C (opt-in strict flag)

The auto-suffix behavior is *correct* for the intended use case (operators running multiple parallel claude-code sessions). It's *wrong* for the rig / co-located-with-OMP case where a phantom registration should be a loud error. Adding `strict: true` to the config gives operators the right tool without breaking the existing pattern.

The rig will set `strict: true` in `/opt/claude.mcp.json`'s env passthrough (via a new `NATS_CHANNEL_STRICT=1` env var that `server.ts` reads). When OMP accidentally autoloads claude's MCP config, the second registration fails loudly instead of silently creating a phantom `cc-2` instance.

For F5(c) — surface the OMP gap as an operator note (no upstream filing).

## Operator decisions deferred

**Decision F5.1 — Strict mode default.** Two options:

- (a) `strict` defaults to `false` (current auto-suffix behavior preserved). Operators opt in.
- (b) `strict` defaults to `true` (loud failure becomes default). Operators opt out for multi-session workflows.

**Axis: architecture.** Changing the default of a user-visible behavior is a one-way door (or at least a noisy migration). Plan ships with (a) — preserve current default; operators opt in.

**Decision F5.2 — File upstream feedback at oh-my-pi/pi-coding-agent about F5(c)?**

**Axis: ethics** (third-party public repo). Per CLAUDE.md, no — the plan surfaces this as a note the operator can choose to file or skip.

## AFK-ready plan

### Task 1 — Failing test: idempotency / strict mode

**File:** `/Users/dmestas/projects/sesh-channels/claude-nats-channel/test/strict-mode.test.ts` (new — or integrated into an existing test file if there's a closer home)

```ts
import { describe, expect, test, beforeEach, afterEach } from "bun:test";
import { connect, type NatsConnection } from "@nats-io/transport-node";
import { spawn, type Subprocess } from "bun";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

const NATS_URL = process.env.NATS_TEST_URL ?? "nats://localhost:4222";

describe("strict mode duplicate-registration handling", () => {
  let nc: NatsConnection;
  let stateA: string;
  let stateB: string;

  beforeEach(async () => {
    nc = await connect({ servers: NATS_URL });
    stateA = join(tmpdir(), `strict-a-${process.pid}-${Date.now()}`);
    stateB = join(tmpdir(), `strict-b-${process.pid}-${Date.now()}-b`);
    mkdirSync(stateA, { recursive: true });
    mkdirSync(stateB, { recursive: true });
    writeFileSync(join(stateA, "config.json"), JSON.stringify({ context: "localhost" }));
    writeFileSync(join(stateB, "config.json"), JSON.stringify({ context: "localhost" }));
  });

  afterEach(async () => {
    await nc.close();
    rmSync(stateA, { recursive: true, force: true });
    rmSync(stateB, { recursive: true, force: true });
  });

  test("default mode auto-suffixes to avoid collision", async () => {
    const a = spawnServer(stateA, { NATS_SESSION_NAME: "test-collision" });
    await wait(800);
    const b = spawnServer(stateB, { NATS_SESSION_NAME: "test-collision" });
    await wait(800);
    // Both should be alive; one as test-collision, one as test-collision-2.
    expect(a.exitCode).toBeNull();
    expect(b.exitCode).toBeNull();
    a.kill();
    b.kill();
    await Promise.all([a.exited, b.exited]);
  });

  test("strict mode refuses to register a duplicate (agent, owner, name)", async () => {
    const a = spawnServer(stateA, { NATS_SESSION_NAME: "test-strict" });
    await wait(800);
    const b = spawnServer(stateB, {
      NATS_SESSION_NAME: "test-strict",
      NATS_CHANNEL_STRICT: "1",
    });
    // b should exit non-zero within a few seconds with a "duplicate" error.
    const code = await Promise.race([
      b.exited,
      wait(3000).then(() => -1),
    ]);
    expect(code).not.toBe(-1);
    expect(code).not.toBe(0);
    a.kill();
    await a.exited;
  });
});

function spawnServer(stateDir: string, env: Record<string, string>): Subprocess {
  return spawn({
    cmd: ["bun", "run", new URL("../server.ts", import.meta.url).pathname],
    env: {
      ...process.env,
      NATS_STATE_DIR: stateDir,
      USER: "test",
      CLAUDE_CWD: "/tmp/strict-test",
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
}

function wait(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
```

(Requires a local nats server. If the test environment doesn't provide one, mark the test `test.skipIf(!process.env.NATS_TEST_URL)` — same pattern the roundtrip-test uses implicitly by hard-coding the URL.)

### Task 2 — Implement strict mode in `claude-nats-channel/server.ts`

**File:** `/Users/dmestas/projects/sesh-channels/claude-nats-channel/server.ts`

Add the env-var read at the top of the file alongside `STATE_DIR`:

```ts
// Strict mode: refuse to auto-suffix on (agent, owner, name) collision. When
// set, a second instance trying to register the same triple as a live one
// fails loudly rather than silently registering as `<name>-2`. The default
// (unset) preserves the documented auto-suffix behavior (README §"Session
// names"). Set `NATS_CHANNEL_STRICT=1` to opt in. (F5 — guards against the
// OMP-discoveryMode-autoloading-claude.mcp.json hazard documented at
// docs/plans/2026-05-22-integration-fix-F5-omp-mcp-discovery.md.)
const STRICT_MODE = process.env.NATS_CHANNEL_STRICT === "1";
```

Modify `resolveSessionName` at lines 322-351 to honor it:

```ts
async function resolveSessionName(nc: NatsConnection, base: string, owner: string): Promise<string> {
  const svcm = new Svcm(nc)
  const client = svcm.client({ maxWait: 1000, maxMessages: 50 })

  const taken = new Set<string>()
  try {
    const iter = await client.info(SERVICE_NAME)
    for await (const si of iter) {
      if (si.metadata?.agent !== AGENT_ID) continue
      if (si.metadata?.owner !== owner) continue
      for (const ep of si.endpoints ?? []) {
        const tokens = ep.subject.split('.')
        if (tokens.length >= 5) taken.add(tokens[4]!)
      }
    }
  } catch {
    // No existing services or timeout — that's fine.
  }

  if (STRICT_MODE && taken.has(base)) {
    process.stderr.write(
      `nats channel: STRICT MODE — refusing to register; ` +
      `(agent=${AGENT_ID}, owner=${owner}, name=${base}) is already taken by a live service. ` +
      `Set NATS_CHANNEL_STRICT=0 (or unset it) to re-enable auto-suffixing.\n`,
    )
    process.exit(2)
  }

  let candidate = base
  let suffix = 2
  while (taken.has(candidate)) {
    candidate = `${base}-${suffix++}`
  }
  return candidate
}
```

### Task 3 — Document strict mode in claude-nats-channel README

**File:** `/Users/dmestas/projects/sesh-channels/claude-nats-channel/README.md`

Append a new subsection just after "Session names":

```markdown
### Strict mode

`NATS_CHANNEL_STRICT=1` makes the channel refuse to auto-suffix on
`(agent, owner, name)` collision. By default (env var unset), a second
instance trying to register the same triple as a live one auto-renames to
`<name>-2`, `<name>-3`, etc. — useful for multiple parallel sessions, but
problematic when a misconfiguration accidentally spawns a duplicate.

Use strict mode when:

- You're running claude-nats-channel in a containerized or unattended
  environment where a phantom duplicate registration should be a loud
  failure.
- You've co-located claude-code with Oh My Pi, and OMP's
  `mcp.discoveryMode: true` may autoload claude's `.mcp.json` — strict
  mode catches the resulting double-spawn.
```

### Task 4 — Wire strict mode in the rig

**File:** `/Users/dmestas/projects/sesh/test/integration/config/claude.mcp.json`

```diff
 {
   "mcpServers": {
     "nats": {
       "command": "bun",
       "args": [
         "run",
         "--cwd",
         "/opt/sesh-channels/claude-nats-channel",
         "--shell=bun",
         "--silent",
         "start"
       ],
       "env": {
         "CLAUDE_CWD": "/workspace",
-        "CLAUDE_PLUGIN_ROOT": "/opt/sesh-channels/claude-nats-channel"
+        "CLAUDE_PLUGIN_ROOT": "/opt/sesh-channels/claude-nats-channel",
+        "NATS_CHANNEL_STRICT": "1"
       }
     }
   }
 }
```

### Task 5 — Document co-location hazard in rig README

**File:** `/Users/dmestas/projects/sesh/test/integration/README.md` — append:

```markdown
## OMP + claude co-location MCP discovery hazard (F5)

OMP's config has `mcp.discoveryMode: true` (intentional, upstream behavior).
When OMP and claude run in the same workspace and a `.mcp.json` is present,
OMP autoloads every entry — including ones intended for claude only —
spawning duplicate channel instances.

The rig avoids this by:

1. Not baking a `.mcp.json` into `/workspace`. claude is launched with
   `--strict-mcp-config --mcp-config /opt/claude.mcp.json` (path outside
   the workspace, invisible to OMP).
2. Setting `NATS_CHANNEL_STRICT=1` in the claude MCP server's env block.
   If a duplicate registration ever does happen, the second instance
   fails loudly rather than registering as a phantom `cc-2`.

When co-locating claude + OMP in a real workspace, apply the same two
rules. The strict mode flag is documented in
`sesh-channels/claude-nats-channel/README.md#strict-mode`.
```

### Task 6 — Optional: upstream notes file for OMP

**File:** `/Users/dmestas/projects/sesh/docs/upstream-notes-omp-mcp-discovery.md` (new — operator may file or skip)

```markdown
# Notes for Oh My Pi — MCP discovery exclusion mechanism

> **Status:** Draft for operator's discretion. Per CLAUDE.md no-third-party-
> filing, Claude Code subagents do not file at oh-my-pi/pi-coding-agent.

When `mcp.discoveryMode: true` is set, OMP autoloads every entry in any
`.mcp.json` reachable from the working directory. There's no exclusion
mechanism: an operator who runs claude-code and OMP in the same workspace
has no way to tell OMP "ignore these specific MCP entries — they're for
claude, not for you".

Suggested fix: a config key `mcp.exclude: [<server-name>, ...]` (or
`mcp.allowlist: [<server-name>, ...]`) that filters what OMP autoloads.

Workaround for now: place claude's MCP config outside the workspace and
launch claude with `--strict-mcp-config --mcp-config /path/to/file.json`.

If interested, the integration rig at `github.com/danmestas/sesh/test/integration`
demonstrates the co-location hazard and the workaround.
```

### Task 7 — Commit + PR sequence

Two PRs (sesh-channels first, then rig):

1. **PR-1 (sesh-channels):**
   ```bash
   cd /Users/dmestas/projects/sesh-channels
   git checkout -b feat/claude-channel-strict-mode
   git add claude-nats-channel/server.ts \
           claude-nats-channel/README.md \
           claude-nats-channel/test/strict-mode.test.ts
   git commit -m "feat(claude-nats-channel): NATS_CHANNEL_STRICT for fail-loud on duplicate (refs F5)"
   ```

2. **PR-2 (sesh rig):**
   ```bash
   cd /Users/dmestas/projects/sesh
   git checkout -b feat/integration-rig-f5-strict-mode
   git add test/integration/config/claude.mcp.json \
           test/integration/README.md \
           docs/upstream-notes-omp-mcp-discovery.md
   git commit -m "fix(test/integration): wire NATS_CHANNEL_STRICT to detect OMP autodiscovery duplicates (closes F5)"
   ```

PR-2 depends on PR-1 being merged + a new sesh-channels release (or the rig's Dockerfile re-pulls from the local checkout, in which case PR-1's merge is enough).

## Dependencies

- F5 PR-2 (rig) depends on PR-1 (sesh-channels) being merged and pulled into the rig's build context.

## Optional follow-ups

- Generalize `NATS_CHANNEL_STRICT` to the SDK so other adapters can opt in. **Bundle with F2's SDK changes** if the operator wants one SDK release that addresses both.
