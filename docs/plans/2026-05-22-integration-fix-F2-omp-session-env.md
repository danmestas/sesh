# F2 — `omp-nats-channel` doesn't read `SESH_SESSION`

**Date:** 2026-05-22
**Status:** AFK-ready
**Severity:** P1 — adapter inconsistency
**Owner:** sesh-channels (`omp-nats-channel`) + `@agent-ops/sesh-channels` SDK

## Root cause (function and line precise)

`claude-nats-channel/server.ts:234-256, 489-494` defines and uses `discoverSessionLabel()`, which prefers `process.env.SESH_SESSION`, then walks cwd up looking for a unique `.sesh/sessions/<label>.json`, then falls back to `process.env.NATS_SESSION_NAME` then `config.sessionName` then `basename(CLAUDE_CWD)`.

`omp-nats-channel/extensions/nats-channel.ts:963-968` resolves the session label with a *different* order, missing `SESH_SESSION` entirely:

```ts
// 2. Resolve owner + session base name
owner = sanitizeSubjectToken(process.env.USER ?? "unknown") || "unknown";
const rawSession =
    (process.env.NATS_SESSION_NAME ??
        config.sessionName ??
        sanitizeSubjectToken(basename(ctx.cwd))) || "op";
```

This makes `sesh up --exec` (which exports `SESH_SESSION=<label>` per the role/class Phase 2 work — see `~/projects/sesh/docs/plans/2026-05-22-sesh-up-exec-implementation-plan.md`) work for claude but silently misbehave for OMP. OMP registers with `metadata.session=<cwd-basename>`, the agent watcher excludes it from the session manifest's `agents[]` because the session-token mismatch hides it, and case 04 of the rig fails ack-then-empty.

The current rig works around it by exporting `NATS_SESSION_NAME=$SESH_SESSION` in the wrapper (`entrypoint.sh:140-145`). That workaround should disappear when this lands.

The grok / gemini / pi channels likely have the same bug — they were written from the OMP template per `~/projects/sesh-channels/docs/adr/0001-...` and the role/class Phase 2 plan. **Verify before assuming.**

## Alternatives considered

### Option A — Add a `SESH_SESSION` check directly in `omp-nats-channel/extensions/nats-channel.ts:963-968`

Smallest local change. One extra `??` link in the chain.

**Interface complexity:** trivial.
**Blast radius:** one file. No SDK changes.
**Reversibility:** trivial.
**Risk:** drifts the env-resolution implementation away from claude's again next time someone adds a new fallback. Doesn't fix grok/gemini/pi if they have the same bug.

### Option B — Promote `discoverSessionLabel` into `@agent-ops/sesh-channels` and have all 5 adapters consume it

The role/class consolidation already proved this pattern: `readAdapterConfig()` in the SDK is the canonical source. Adding `readSessionLabel()` (or `resolveSessionName()` — naming is a taste decision, see deferred) extends that pattern.

**Interface complexity:** moderate — one new exported function in the SDK, 4 adapter sites updated.
**Blast radius:** SDK + 5 adapter repos. SDK is at 0.1.0 — adding a new export is non-breaking.
**Reversibility:** medium. Adapter sites can revert by inlining; SDK function stays (no harm) or is removed in 0.2.0 if all adapters revert.
**Risk:** SDK and adapter PRs must land in order (SDK first, then adapters). Standard two-repo coordination.

### Option C — Have `omp-nats-channel`'s `extensions/config.ts` re-export `readSessionLabel` next to `readAdapterConfig`

A half-measure: the adapter's `config.ts` (currently `import { readAdapterConfig } from "@agent-ops/sesh-channels"`) could fall back on `readAdapterConfig().session` if non-empty. But `readAdapterConfig` returns `process.env.SESH_SESSION ?? ""` — it doesn't do the cwd-walk discovery that claude's `discoverSessionLabel` does. Equivalence is partial.

### Chosen approach — Option B

Promote `discoverSessionLabel` into the SDK as `readSessionLabel()`. This is the same pattern Phase 2 used for `readRoleClass()` / `readAdapterConfig()`. The CLAUDE.md "Don't write a workaround in the consuming repo" rule applies here: the bug is genuinely in the OMP adapter, and the *fix* belongs in the SDK so all adapters benefit.

Sequence: SDK PR first (publish 0.1.1 / 0.2.0), then adapter PRs that bump the SDK dep and switch to `readSessionLabel`. If the operator wants to ship F1 / F4-F8 in parallel without waiting on the npm publish, the rig's `NATS_SESSION_NAME` workaround in `entrypoint.sh:145` can stay until the new SDK version is out. **The operator decides** (axis: reversibility — npm publishing is one-way; see deferred).

## Operator decisions deferred

**Decision F2.1 — SDK release timing.** Two binary options:

- (a) Bump `@agent-ops/sesh-channels` to **0.1.1** (patch — non-breaking additive export). All adapters opt in by updating their dep. Older adapters keep working.
- (b) Bump to **0.2.0** (minor — additive but signals "you should adopt this"). Same wire effect; different signaling.

**Axis: reversibility** — npm publishes are practically irreversible (unpublish is discouraged + windowed). Pick one; the plan ships with (a) selected by default.

**Decision F2.2 — Naming.** The function in claude's server.ts is `discoverSessionLabel`. The SDK already has `readRoleClass` / `readAdapterConfig`. Options:

- (a) `readSessionLabel` — matches existing SDK verb.
- (b) `discoverSessionLabel` — matches what it does, but diverges from SDK convention.

**Axis: taste.** Plan ships with (a) `readSessionLabel`. To change, find/replace in the SDK source file (per Task 1) and the adapter import sites (Tasks 3-7).

## AFK-ready plan

### Task 1 — Add `readSessionLabel` to `@agent-ops/sesh-channels` SDK

The SDK source is in a separate repo (not in this tree — see Decision F2.1 below for the publish path). The SDK currently exports:

```ts
// src/index.ts (current — verified at
// /Users/dmestas/projects/sesh-channels/claude-nats-channel/node_modules/@agent-ops/sesh-channels/dist/index.js)
export { ConfigError, readAdapterConfig, readRoleClass };
```

Add a new exported function `readSessionLabel(opts?)` with this exact shape — copy-paste from `claude-nats-channel/server.ts:209-256`, generalize:

```ts
// src/session.ts (new file)

import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";

const SESH_SESSIONS_DIRNAME = join(".sesh", "sessions");

export interface SessionLabelOptions {
  /** Override for testing — defaults to `process.cwd()`. */
  startDir?: string;
  /** Override for testing — defaults to `process.env`. */
  env?: NodeJS.ProcessEnv;
  /**
   * Stderr sink for the "ambiguous sessions" diagnostic. Defaults to
   * `process.stderr.write`. Passing a no-op silences it (e.g., for tests).
   */
  warn?: (msg: string) => void;
}

/**
 * Resolve a sesh session label without an explicit operator-supplied value.
 *
 * Resolution order:
 *   1. `process.env.SESH_SESSION` (canonical — set by `sesh up --exec` and
 *      `orch-spawn` SESH_* exports).
 *   2. Walk cwd → root looking for the nearest `.sesh/sessions/` dir; if it
 *      contains exactly one `<label>.json`, that label wins.
 *   3. If the dir has multiple `.json` files, emit a one-line stderr warning
 *      (ambiguity diagnostic) and return null — caller falls back to its
 *      own default.
 *   4. Returns null if no sesh state is reachable.
 *
 * Mirrors `discoverSessionLabel()` formerly in claude-nats-channel/server.ts.
 * Adapters should compose this with `process.env.NATS_SESSION_NAME` (operator
 * override) and any adapter-local fallback (e.g., `basename(cwd)`).
 */
export function readSessionLabel(opts: SessionLabelOptions = {}): string | null {
  const env = opts.env ?? process.env;
  const warn = opts.warn ?? ((msg) => process.stderr.write(msg));
  const explicit = env.SESH_SESSION?.trim();
  if (explicit) return explicit;

  let dir = opts.startDir ?? process.cwd();
  while (true) {
    const sessionsDir = join(dir, SESH_SESSIONS_DIRNAME);
    try {
      const files = readdirSync(sessionsDir).filter((f) => f.endsWith(".json"));
      if (files.length === 1) return files[0]!.replace(/\.json$/, "");
      if (files.length > 1) {
        warn(
          `sesh-channels: ambiguous sesh sessions in ${sessionsDir}: ` +
            `${files.join(", ")} — set $SESH_SESSION to pick one\n`,
        );
        return null;
      }
    } catch {
      // sessions dir doesn't exist here; walk up
    }
    const parent = dirname(dir);
    if (parent === dir) return null;
    dir = parent;
  }
}
```

Update `src/index.ts` to add the export:

```ts
// existing exports unchanged
export { readSessionLabel, type SessionLabelOptions } from "./session";
```

### Task 2 — TDD: failing test for `readSessionLabel`

The SDK's existing test pattern is in `claude-nats-channel/test/config.test.ts` (downstream consumer test). Add a unit test in the SDK's own test dir (likely `src/session.test.ts` or `test/session.test.ts` per the repo's convention — pick the existing pattern in the SDK repo).

```ts
// src/session.test.ts
import { describe, expect, test, beforeEach, afterEach } from "bun:test";
import { mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { readSessionLabel } from "./session";

describe("readSessionLabel", () => {
  let tmp: string;
  beforeEach(() => {
    tmp = join(tmpdir(), `readSessionLabel-${process.pid}-${Date.now()}`);
    mkdirSync(tmp, { recursive: true });
  });
  afterEach(() => {
    rmSync(tmp, { recursive: true, force: true });
  });

  test("prefers SESH_SESSION from env over filesystem", () => {
    const sessionsDir = join(tmp, ".sesh", "sessions");
    mkdirSync(sessionsDir, { recursive: true });
    writeFileSync(join(sessionsDir, "filesystem-label.json"), "{}");
    const label = readSessionLabel({
      startDir: tmp,
      env: { SESH_SESSION: "env-label" },
    });
    expect(label).toBe("env-label");
  });

  test("walks cwd → root for .sesh/sessions/<label>.json", () => {
    const nested = join(tmp, "a", "b", "c");
    mkdirSync(nested, { recursive: true });
    const sessionsDir = join(tmp, ".sesh", "sessions");
    mkdirSync(sessionsDir, { recursive: true });
    writeFileSync(join(sessionsDir, "smoke-test.json"), "{}");
    const label = readSessionLabel({ startDir: nested, env: {} });
    expect(label).toBe("smoke-test");
  });

  test("returns null when sessions dir has multiple files (ambiguous)", () => {
    const sessionsDir = join(tmp, ".sesh", "sessions");
    mkdirSync(sessionsDir, { recursive: true });
    writeFileSync(join(sessionsDir, "a.json"), "{}");
    writeFileSync(join(sessionsDir, "b.json"), "{}");
    let warned = "";
    const label = readSessionLabel({
      startDir: tmp,
      env: {},
      warn: (m) => { warned += m; },
    });
    expect(label).toBeNull();
    expect(warned).toContain("ambiguous sesh sessions");
  });

  test("returns null when no sesh state is reachable", () => {
    const label = readSessionLabel({ startDir: tmp, env: {} });
    expect(label).toBeNull();
  });

  test("trims SESH_SESSION whitespace; empty after trim falls through", () => {
    const sessionsDir = join(tmp, ".sesh", "sessions");
    mkdirSync(sessionsDir, { recursive: true });
    writeFileSync(join(sessionsDir, "from-fs.json"), "{}");
    expect(
      readSessionLabel({ startDir: tmp, env: { SESH_SESSION: "  spaced  " } }),
    ).toBe("spaced");
    expect(
      readSessionLabel({ startDir: tmp, env: { SESH_SESSION: "   " } }),
    ).toBe("from-fs");
  });
});
```

Verify red, then green after Task 1's implementation.

### Task 3 — Update OMP adapter

**File:** `/Users/dmestas/projects/sesh-channels/omp-nats-channel/extensions/nats-channel.ts`

At line 963-968, replace:

```ts
// 2. Resolve owner + session base name
owner = sanitizeSubjectToken(process.env.USER ?? "unknown") || "unknown";
const rawSession =
    (process.env.NATS_SESSION_NAME ??
        config.sessionName ??
        sanitizeSubjectToken(basename(ctx.cwd))) || "op";
```

with:

```ts
// 2. Resolve owner + session base name.
// Resolution order matches claude-nats-channel + the SDK's `readSessionLabel`
// helper:
//   1. $SESH_SESSION (sesh up --exec / orch-spawn canonical)
//   2. .sesh/sessions/<label>.json discovery via cwd walk
//   3. $NATS_SESSION_NAME (operator override)
//   4. config.sessionName (deprecated last-resort)
//   5. basename(cwd) as the final fallback
owner = sanitizeSubjectToken(process.env.USER ?? "unknown") || "unknown";
const rawSession =
    (readSessionLabel({ startDir: ctx.cwd }) ??
        process.env.NATS_SESSION_NAME ??
        config.sessionName ??
        sanitizeSubjectToken(basename(ctx.cwd))) || "op";
```

Add the import to the file's existing `@agent-ops/sesh-channels` import line (or alongside it). Currently the OMP adapter imports `readAdapterConfig` only via `extensions/config.ts`. Extend `extensions/config.ts`:

```ts
// /Users/dmestas/projects/sesh-channels/omp-nats-channel/extensions/config.ts
import {
  readAdapterConfig,
  readSessionLabel,
  ConfigError,
  type SessionLabelOptions,
} from "@agent-ops/sesh-channels";

export const readConfig = () => readAdapterConfig("omp");
export { ConfigError, readSessionLabel };
export type { AdapterConfig, AgentClass, SessionLabelOptions } from "@agent-ops/sesh-channels";
```

And update the `nats-channel.ts` import to pull `readSessionLabel`:

```ts
// Near the top, with the other "./config" import:
import { readConfig as readRoleClassConfig, readSessionLabel } from "./config";
```

Bump the SDK dep in `omp-nats-channel/package.json`:

```diff
-    "@agent-ops/sesh-channels": "^0.1.0",
+    "@agent-ops/sesh-channels": "^0.1.1",
```

(or `^0.2.0` if Decision F2.1 is (b)).

### Task 4 — Failing test for OMP session resolution

**File:** `/Users/dmestas/projects/sesh-channels/omp-nats-channel/test/session-label.test.ts` (new)

```ts
import { describe, expect, test, beforeEach, afterEach } from "bun:test";
import { mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { readSessionLabel } from "../extensions/config";

// This test asserts the OMP adapter re-exports the SDK helper and that the
// helper matches claude-nats-channel's resolution order. It's an integration
// guard: if the SDK or the adapter drifts, this fails.
describe("omp-nats-channel session label resolution", () => {
  let tmp: string;
  let saved: NodeJS.ProcessEnv;
  beforeEach(() => {
    saved = { ...process.env };
    tmp = join(tmpdir(), `omp-session-${process.pid}-${Date.now()}`);
    mkdirSync(tmp, { recursive: true });
  });
  afterEach(() => {
    process.env = { ...saved };
    rmSync(tmp, { recursive: true, force: true });
  });

  test("SESH_SESSION env wins over basename(cwd)", () => {
    process.env.SESH_SESSION = "smoke-test";
    const got = readSessionLabel({ startDir: tmp });
    expect(got).toBe("smoke-test");
  });

  test("walks .sesh/sessions/<label>.json when SESH_SESSION unset", () => {
    delete process.env.SESH_SESSION;
    const sessionsDir = join(tmp, ".sesh", "sessions");
    mkdirSync(sessionsDir, { recursive: true });
    writeFileSync(join(sessionsDir, "auto-discovered.json"), "{}");
    expect(readSessionLabel({ startDir: tmp })).toBe("auto-discovered");
  });
});
```

This test fails before Task 3 (because the export doesn't exist), passes after.

### Task 5 — Audit grok / gemini / pi channels for the same bug

For each of:

- `/Users/dmestas/projects/sesh-channels/grok-nats-channel/`
- `/Users/dmestas/projects/sesh-channels/gemini-nats-channel/`
- `/Users/dmestas/projects/sesh-channels/pi-nats-channel/`

Check the analog of OMP's lines 963-968 (look for `NATS_SESSION_NAME` + `basename(cwd)`). If the adapter resolves session label without `SESH_SESSION`, apply the same SDK-import substitution + SDK-dep bump. Bundle each adapter into its own PR (or one PR per parallel branch) so the diffs stay small.

For each adapter that doesn't have the bug (verify by reading the resolution site, not by assuming), still bump the SDK dep when you do their next change — but don't ship a no-op PR just for the bump.

### Task 6 — Remove the rig workaround

**File:** `/Users/dmestas/projects/sesh/test/integration/entrypoint.sh`

Once OMP has shipped the fix and is consuming a published SDK with `readSessionLabel`, drop these lines from `entrypoint.sh:139-145`:

```bash
# omp-nats-channel/extensions/nats-channel.ts only reads NATS_SESSION_NAME
# for the session token — it does NOT consult SESH_SESSION like
# claude-nats-channel/server.ts does (claude calls discoverSessionLabel
# which checks SESH_SESSION; OMP falls straight through to basename(cwd)).
# Work around by setting NATS_SESSION_NAME explicitly. This is an
# adapter-inconsistency finding for sesh-channels (see FINDINGS).
export NATS_SESSION_NAME="${SESH_SESSION:-}"
```

The Docker rig should still pass case 04 + case 06's OMP leg without the workaround. Run case 04 / case 06 to verify before committing the removal.

### Task 7 — Commit + PR sequence

The sequence is gated by the npm publish, so it's serial:

1. **PR-1: SDK adds `readSessionLabel`.**
   Repo: wherever `@agent-ops/sesh-channels` source lives (not in this tree — operator confirms during AFK dispatch).
   Title: `feat(sesh-channels): add readSessionLabel helper for adapter session-label resolution`
2. **Publish `@agent-ops/sesh-channels@0.1.1`** to npm (operator-driven step — see Decision F2.1).
3. **PR-2: OMP adapter consumes `readSessionLabel`.**
   Repo: `sesh-channels`. Branch: `fix/omp-channel-honors-sesh-session`.
   Title: `fix(omp-nats-channel): honor $SESH_SESSION via SDK's readSessionLabel (closes F2)`
4. **PR-3 / PR-4 / PR-5: grok / gemini / pi adapters** — if Task 5 finds the same bug, one PR each. If clean, skip.
5. **PR-6: sesh rig removes the workaround.**
   Repo: `sesh`. Branch: `chore/integration-rig-drop-omp-session-workaround`.
   Title: `chore(test/integration): drop NATS_SESSION_NAME workaround now that OMP honors SESH_SESSION`

## Dependencies

- **F2 depends on:** none (independent of F1, F3-F8).
- **F2 PR sequence is internally serialized** by the npm publish step. Within the sequence, PRs 3-5 (adapter fixes) can run in parallel waves once PR-2 is approved.
- **Rig change (PR-6) blocked by:** SDK publish + OMP adapter PR-2 merged and consumed by the rig's Dockerfile (bun install resolves to the new SDK version automatically since `^0.1.1` allows it; rebuild the container).

## Optional follow-ups

- Add the same `readSessionLabel`-based resolution to the `synadia-agents` upstream copies of these adapters. **Per CLAUDE.md no-third-party-filing, do not file at synadia-ai/synadia-agents — surface the gap as a Decision and let the operator decide whether to file or skip.**
