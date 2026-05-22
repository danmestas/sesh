# Plan A — Ship `@agent-ops/sesh-channels` from sesh's `agents/sdk-ts/`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a tiny published TS package (`@agent-ops/sesh-channels`) from a new `agents/sdk-ts/` subdir of the sesh repo. Mirrors `internal/agentmeta` (Go). Consumed by sesh-channels adapters (Plan B) so the 5 inline validators collapse to one shared import.

**Architecture:** One npm package, one ~50-LoC source file, one test file, one README. Lives at `sesh/agents/sdk-ts/`. Versioned alongside sesh's wire-protocol changes. Published from sesh CI on tag push (later — manual publish for v0.1.0). Source of truth for the rules is `docs/proposals/2026-05-21-agent-role-registration.md` and `internal/agentmeta/` — this package is the TS port, not the canonical document.

**Tech Stack:** TypeScript (strict), `tsup` for ESM+CJS dual build, `bun:test` for tests, `@types/node` for the Node.js types. Bun ≥ 1.2 for local development. Node ≥ 20 supported by consumers.

**Source proposal:** `docs/proposals/2026-05-21-agent-role-registration.md` ("Canonical role/class rules" section).

---

## File Structure

**Create (in sesh repo):**

```
agents/
└── sdk-ts/
    ├── package.json          # name, version, scripts, ESM-only build config
    ├── tsconfig.json         # strict TS config
    ├── tsup.config.ts        # ESM + .d.ts build
    ├── src/
    │   └── index.ts          # single file — all types, validators, defaults, readers (~80 LoC)
    ├── test/
    │   └── agent-meta.test.ts
    ├── README.md
    └── CHANGELOG.md
```

**Modify:** none in this plan (sesh's Go side already has `internal/agentmeta`; we don't touch it).

**Module shape:** one file, 4 public symbols (`readRoleClass`, `readAdapterConfig`, `ConfigError`, `AgentClass` type) + 1 public type alias (`AdapterConfig` and `AdapterRoleClass`). Internal helpers (`validateRole`, `validateClass`, `defaultedRole`, `defaultedClass`, regex/length constants) are not exported — they're implementation details of `readRoleClass` / `readAdapterConfig` and exposing them invites callers to bypass validation.

---

## Task 1: Confirm npm scope and package name

**Decision (operator-confirmed 2026-05-22):** npm name is **`@agent-ops/sesh-channels`** (scope `@agent-ops`).

- [ ] **Step 1: Confirm availability and scope ownership**

Run:

```bash
npm view @agent-ops/sesh-channels 2>&1 | head -3 || echo "available"
npm org ls agent-ops 2>&1 | head -5
```

Expected: package name returns "available" (404). Org `agent-ops` either exists (lists members) or doesn't (404 — must be created).

- [ ] **Step 2: Create the scope if it doesn't exist**

If `npm org ls agent-ops` returned "not found":

```bash
npm org create agent-ops --type=user --otp=<your-otp-if-2fa>
```

Skip if the org already exists with the operator as a member.

- [ ] **Step 3: No commit yet** — registry-side setup only. The first commit lands at Task 2.

---

## Task 2: Scaffold the package (ESM-only)

**Files:**
- Create: `agents/sdk-ts/package.json`, `agents/sdk-ts/tsconfig.json`, `agents/sdk-ts/tsup.config.ts`

- [ ] **Step 1: Create `agents/sdk-ts/package.json`**

```json
{
  "name": "@agent-ops/sesh-channels",
  "version": "0.1.0",
  "description": "Canonical role/class types, validators, and defaults for the sesh agent registration protocol. TS port of github.com/danmestas/sesh/internal/agentmeta.",
  "license": "Apache-2.0",
  "author": "Dan Mestas",
  "repository": {
    "type": "git",
    "url": "git+https://github.com/danmestas/sesh.git",
    "directory": "agents/sdk-ts"
  },
  "homepage": "https://github.com/danmestas/sesh/tree/main/agents/sdk-ts#readme",
  "bugs": "https://github.com/danmestas/sesh/issues",
  "keywords": ["sesh", "nats", "agent", "synadia-agent-protocol", "role", "class"],
  "type": "module",
  "module": "./dist/index.js",
  "types": "./dist/index.d.ts",
  "exports": {
    ".": {
      "import": "./dist/index.js",
      "types": "./dist/index.d.ts"
    }
  },
  "files": ["dist/", "README.md", "CHANGELOG.md"],
  "engines": { "node": ">=20" },
  "scripts": {
    "build": "tsup",
    "test": "bun test",
    "typecheck": "tsc --noEmit",
    "prepublishOnly": "npm run typecheck && npm run test && npm run build"
  },
  "devDependencies": {
    "@types/node": "^20.14.0",
    "tsup": "^8.3.0",
    "typescript": "^5.6.0"
  }
}
```

ESM-only by default. If a CJS consumer surfaces later, add `"require": "./dist/index.cjs"` to `exports` + `format: ["esm", "cjs"]` in `tsup.config.ts` as a minor version bump. Don't ship dual-build speculatively.

- [ ] **Step 2: Create `agents/sdk-ts/tsconfig.json`**

```json
{
  "compilerOptions": {
    "target": "es2022",
    "module": "esnext",
    "moduleResolution": "bundler",
    "lib": ["es2022"],
    "strict": true,
    "noImplicitAny": true,
    "noImplicitReturns": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true,
    "esModuleInterop": true,
    "skipLibCheck": true,
    "declaration": true,
    "declarationMap": true,
    "sourceMap": true,
    "outDir": "dist",
    "rootDir": "src",
    "types": ["node"]
  },
  "include": ["src/**/*"],
  "exclude": ["dist", "node_modules", "test"]
}
```

- [ ] **Step 3: Create `agents/sdk-ts/tsup.config.ts`**

```ts
import { defineConfig } from "tsup";

export default defineConfig({
  entry: ["src/index.ts"],
  format: ["esm"],
  dts: true,
  splitting: false,
  sourcemap: true,
  clean: true,
  target: "node20",
});
```

- [ ] **Step 4: Install dependencies**

Run:

```bash
cd /Users/dmestas/projects/sesh/agents/sdk-ts
bun install
```

Expected: `bun.lock` created; `node_modules/` populated. No errors.

- [ ] **Step 5: Add `node_modules` and `dist` to sesh's root `.gitignore`**

Verify with `grep -E 'node_modules|^dist/' /Users/dmestas/projects/sesh/.gitignore`. If either is absent, append:

```gitignore
node_modules/
dist/
```

- [ ] **Step 6: Checkpoint**

Stage the new files (not `node_modules` or `dist`). Commit: `feat(sdk-ts): scaffold @agent-ops/sesh-channels package (no source yet)`.

---

## Task 3: Write the failing tests

**Files:**
- Create: `agents/sdk-ts/test/agent-meta.test.ts`

- [ ] **Step 1: Write the test suite (behavior-only, no testing of trivial constants or compile-time-proven type narrowing)**

File: `agents/sdk-ts/test/agent-meta.test.ts`

```ts
import { describe, expect, test, afterEach } from "bun:test";
import { ConfigError, readAdapterConfig, readRoleClass } from "../src/index";

describe("readRoleClass", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
  });

  test("reads SESH_ROLE and SESH_CLASS from env", () => {
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const got = readRoleClass();
    expect(got.role).toBe("implementer");
    expect(got.class).toBe("active");
  });

  test("applies defaults when env unset", () => {
    delete process.env.SESH_ROLE;
    delete process.env.SESH_CLASS;
    const got = readRoleClass();
    expect(got.role).toBe("worker");
    expect(got.class).toBe("active");
  });

  test("trims whitespace before defaulting and validating", () => {
    process.env.SESH_ROLE = "  worker  ";
    process.env.SESH_CLASS = "  observer  ";
    const got = readRoleClass();
    expect(got.role).toBe("worker");
    expect(got.class).toBe("observer");
  });

  test("throws ConfigError on invalid role (uppercase, space, slash, oversize)", () => {
    process.env.SESH_CLASS = "active";
    for (const bad of ["Worker", "im plementer", "im/plementer", "a".repeat(64)]) {
      process.env.SESH_ROLE = bad;
      expect(() => readRoleClass()).toThrow(ConfigError);
    }
  });

  test("throws ConfigError on invalid class", () => {
    process.env.SESH_ROLE = "worker";
    for (const bad of ["passive", "ACTIVE", "spy"]) {
      process.env.SESH_CLASS = bad;
      expect(() => readRoleClass()).toThrow(ConfigError);
    }
  });
});

describe("readAdapterConfig", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
  });

  test("composes NATS / agent / owner / session / role / class", () => {
    process.env.NATS_URL = "nats://example:4222";
    process.env.SESH_OWNER = "dmestas";
    process.env.SESH_SESSION = "rc-test";
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const c = readAdapterConfig({ defaultAgent: "claude-code" });
    expect(c.natsUrl).toBe("nats://example:4222");
    expect(c.agent).toBe("claude-code");
    expect(c.owner).toBe("dmestas");
    expect(c.session).toBe("rc-test");
    expect(c.role).toBe("implementer");
    expect(c.class).toBe("active");
  });

  test("SESH_AGENT env var overrides defaultAgent", () => {
    process.env.SESH_AGENT = "custom-bot";
    const c = readAdapterConfig({ defaultAgent: "claude-code" });
    expect(c.agent).toBe("custom-bot");
  });

  test("defaults NATS_URL, owner, session when env unset", () => {
    delete process.env.NATS_URL;
    delete process.env.SESH_OWNER;
    delete process.env.SESH_SESSION;
    delete process.env.USER;
    delete process.env.SESH_AGENT;
    const c = readAdapterConfig({ defaultAgent: "pi" });
    expect(c.natsUrl).toBe("nats://localhost:4222");
    expect(c.owner).toBe("anon");
    expect(c.session).toBe("");
    expect(c.agent).toBe("pi");
  });
});
```

8 tests, all behavior. No tests of constants (`DefaultRole === "worker"` is a tautology against the same source file), no tests of TS type narrowing (the compiler proves it), no tests of internal helpers (they're not exported).

- [ ] **Step 2: Run the tests to confirm they fail (no src/index.ts yet)**

Run:

```bash
cd /Users/dmestas/projects/sesh/agents/sdk-ts
bun test
```

Expected: every test FAILs with "Cannot find module '../src/index'" or similar. Total: ~20+ tests, all failing.

- [ ] **Step 3: Checkpoint**

Commit: `test(sdk-ts): write failing test suite for @agent-ops/sesh-channels (TDD red phase)`.

---

## Task 4: Implement `src/index.ts` (single file)

**Files:**
- Create: `agents/sdk-ts/src/index.ts`

- [ ] **Step 1: Write the single source file**

File: `agents/sdk-ts/src/index.ts`

```ts
// SOURCE OF TRUTH for these rules:
//   https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md
//   ("Canonical role/class rules" section)
//
// TypeScript port of github.com/danmestas/sesh/internal/agentmeta. Keep in
// lockstep with the Go side — drift between adapters and the hub corrupts
// the agents[] view in the session manifest.

export type AgentClass = "active" | "observer";

export interface AdapterRoleClass {
  role: string;
  class: AgentClass;
}

export interface AdapterConfig {
  natsUrl: string;
  agent: string;
  owner: string;
  session: string;
  role: string;
  class: AgentClass;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

const ROLE_RE = /^[a-z0-9_-]+$/;
const DEFAULT_ROLE = "worker";
const DEFAULT_CLASS: AgentClass = "active";

function validateRole(role: string): void {
  if (role.length === 0) throw new ConfigError("role is empty");
  if (role.length > 63) throw new ConfigError(`role ${JSON.stringify(role)} is ${role.length} bytes; max 63`);
  if (!ROLE_RE.test(role)) throw new ConfigError(`role ${JSON.stringify(role)} must match ^[a-z0-9_-]+$`);
}

function validateClass(c: string): asserts c is AgentClass {
  if (c !== "active" && c !== "observer") {
    throw new ConfigError(`class ${JSON.stringify(c)} must be "active" or "observer"`);
  }
}

/**
 * Read role and class from process.env (SESH_ROLE / SESH_CLASS), apply
 * defaults, then validate. Throws ConfigError on invalid input.
 */
export function readRoleClass(): AdapterRoleClass {
  const role = ((process.env.SESH_ROLE ?? "").trim()) || DEFAULT_ROLE;
  const cls = ((process.env.SESH_CLASS ?? "").trim()) || DEFAULT_CLASS;
  validateRole(role);
  validateClass(cls);
  return { role, class: cls };
}

/**
 * Read the full adapter config (NATS_URL, agent, owner, session, role, class)
 * from process.env. `defaultAgent` is the adapter's canonical agent name —
 * the only knob that differs across adapters.
 *
 * Throws ConfigError on invalid role/class. NATS_URL / owner / session fall
 * back to defaults when unset; nothing else throws.
 */
export function readAdapterConfig(opts: { defaultAgent: string }): AdapterConfig {
  const { role, class: cls } = readRoleClass();
  return {
    natsUrl: process.env.NATS_URL ?? "nats://localhost:4222",
    agent: process.env.SESH_AGENT ?? opts.defaultAgent,
    owner: process.env.SESH_OWNER ?? process.env.USER ?? "anon",
    session: process.env.SESH_SESSION ?? "",
    role,
    class: cls,
  };
}
```

Public surface: 4 symbols (`readRoleClass`, `readAdapterConfig`, `ConfigError`, type aliases `AgentClass` / `AdapterRoleClass` / `AdapterConfig`). Internal: validators, regex, defaults — not exported.

- [ ] **Step 2: Run the test suite**

Run:

```bash
cd /Users/dmestas/projects/sesh/agents/sdk-ts
bun test
```

Expected: 8 tests PASS, 0 failures.

- [ ] **Step 3: Run typecheck**

Run:

```bash
bun run typecheck
```

Expected: no errors.

- [ ] **Step 4: Run the build**

Run:

```bash
bun run build
```

Expected: `dist/index.js`, `dist/index.d.ts` produced (ESM-only — no `.cjs`). No errors.

- [ ] **Step 5: Checkpoint**

Commit: `feat(sdk-ts): implement @agent-ops/sesh-channels — readRoleClass + readAdapterConfig (ESM, single file)`.

---

## Task 5: Write `README.md` and `CHANGELOG.md`

**Files:**
- Create: `agents/sdk-ts/README.md`, `agents/sdk-ts/CHANGELOG.md`

- [ ] **Step 1: Write `agents/sdk-ts/README.md`**

```markdown
# @agent-ops/sesh-channels

Canonical role/class types, validators, and defaults for the **sesh agent registration protocol**. TS port of [`github.com/danmestas/sesh/internal/agentmeta`](https://github.com/danmestas/sesh/tree/main/internal/agentmeta).

Use this package in any NATS-channel adapter that registers an agent on the sesh mesh (claude / pi / omp / gemini / grok / your own). It owns the regex, the length bound, the class enum, and the defaulting policy — adapter code becomes a one-line import.

## Install

\`\`\`bash
npm i @agent-ops/sesh-channels
# or
bun add @agent-ops/sesh-channels
\`\`\`

## Usage

The 90% case: read role + class from env at boot, hand the result to your `AgentService` / `svcm.add` metadata.

\`\`\`ts
import { readRoleClass } from "@agent-ops/sesh-channels";
import { AgentService } from "@synadia-ai/agent-service";

const { role, class: cls } = readRoleClass(); // throws ConfigError on invalid input

const svc = new AgentService({
  nc,
  agent: "claude-code",
  owner: process.env.SESH_OWNER ?? "anon",
  name: "main",
  metadata: { role, class: cls /* ... your other metadata ... */ },
});
\`\`\`

Or, for the full adapter config (NATS_URL + agent + owner + session + role + class) in one call:

\`\`\`ts
import { readAdapterConfig } from "@agent-ops/sesh-channels";

const cfg = readAdapterConfig({ defaultAgent: "claude-code" }); // throws ConfigError on bad role/class

const svc = new AgentService({
  nc,
  agent: cfg.agent,
  owner: cfg.owner,
  name: cfg.session || "main",
  metadata: { role: cfg.role, class: cfg.class, session: cfg.session },
});
\`\`\`

## API

| Symbol | Purpose |
|---|---|
| `readRoleClass()` | Read `SESH_ROLE` / `SESH_CLASS` from env, apply defaults, validate. Returns `{ role, class }`. Throws `ConfigError` on invalid input. |
| `readAdapterConfig({ defaultAgent })` | Compose `readRoleClass` with NATS_URL / SESH_AGENT / SESH_OWNER / SESH_SESSION env reads. Returns a full `AdapterConfig`. Throws on bad role/class. |
| `ConfigError` | Thrown on invalid input. Distinct class so callers can `instanceof` check. |
| `AgentClass` (type) | Literal union `"active" \| "observer"`. |
| `AdapterRoleClass` (type) | `{ role: string; class: AgentClass }`. |
| `AdapterConfig` (type) | `AdapterRoleClass` + NATS_URL / agent / owner / session. |

Internal helpers (`validateRole`, `validateClass`, defaults) are intentionally not exported — callers shouldn't bypass validation, and the boundary is `readRoleClass` / `readAdapterConfig`.

## Canonical rules

Mirrored verbatim from [the proposal](https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md#canonical-roleclass-rules-cite-this-section-verbatim):

\`\`\`
role regex     : ^[a-z0-9_-]+$
role length    : 1..63 bytes inclusive
role default   : "worker"
class values   : "active" | "observer"
class default  : "active"
\`\`\`

Defaulting rule: empty / unset → apply default. Any other value: validate; on failure, throw at boot.

## Versioning

Tracks sesh's agent-registration wire format. Minor bumps for additive metadata fields (e.g., capabilities); major bumps for changes that break adapter compatibility.

## Source of truth

When the rules change, this package and `internal/agentmeta` MUST be updated together. The proposal at [`docs/proposals/2026-05-21-agent-role-registration.md`](https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md) is the canonical document.
\`\`\`

- [ ] **Step 2: Write `agents/sdk-ts/CHANGELOG.md`**

```markdown
# Changelog

All notable changes to `@agent-ops/sesh-channels`.

## 0.1.0 — 2026-05-22

Initial release. ESM-only, Node ≥ 20.

- `readRoleClass()` — boot-time env reader: trims, defaults, validates SESH_ROLE / SESH_CLASS. Throws `ConfigError` on invalid input.
- `readAdapterConfig({ defaultAgent })` — composes `readRoleClass` with NATS_URL / SESH_AGENT / SESH_OWNER / SESH_SESSION reads into a full `AdapterConfig`.
- `ConfigError` — typed error for invalid role/class at boot.
- Public types: `AgentClass`, `AdapterRoleClass`, `AdapterConfig`.

Ports the rules from [`internal/agentmeta`](https://github.com/danmestas/sesh/tree/main/internal/agentmeta) (Go) and `docs/proposals/2026-05-21-agent-role-registration.md`.
```

- [ ] **Step 3: Checkpoint**

Commit: `docs(sdk-ts): README and CHANGELOG for @agent-ops/sesh-channels 0.1.0`.

---

## Task 6: Publish v0.1.0 to npm

**Files:** none (publish is a registry-side action).

- [ ] **Step 1: Confirm npm login**

Run:

```bash
npm whoami
```

Expected: prints the operator's npm username. If it errors "ENEEDAUTH", run `npm login` first and re-try.

- [ ] **Step 2: Dry-run the publish**

Run:

```bash
cd /Users/dmestas/projects/sesh/agents/sdk-ts
npm publish --dry-run
```

Expected output: lists exactly `dist/index.js`, `dist/index.d.ts`, `dist/index.js.map`, `README.md`, `CHANGELOG.md`, `package.json`. No `.cjs`, no source files, no `node_modules`. Total size under 20 KB.

If anything looks wrong (e.g., `src/` files listed), check the `files` field in `package.json`.

- [ ] **Step 3: Publish for real**

Run:

```bash
npm publish --access public
```

Expected: `+ @agent-ops/sesh-channels@0.1.0`. Note the registry URL — verify at `https://www.npmjs.com/package/@agent-ops/sesh-channels`.

(If the package name differs per Task 1, substitute it everywhere.)

- [ ] **Step 4: Verify the install works from outside the repo**

Run (in a temp dir):

```bash
mkdir -p /tmp/sdk-smoke && cd /tmp/sdk-smoke
npm init -y >/dev/null
npm pkg set type=module
npm i @agent-ops/sesh-channels@0.1.0 2>&1 | tail -3
node --input-type=module -e 'import { readRoleClass } from "@agent-ops/sesh-channels"; console.log(readRoleClass());'
```

Expected: `{ role: 'worker', class: 'active' }` printed (env unset → defaults applied). Clean up: `rm -rf /tmp/sdk-smoke`.

- [ ] **Step 5: Tag the release in git**

Run:

```bash
cd /Users/dmestas/projects/sesh
git tag -a sdk-ts-v0.1.0 -m "@agent-ops/sesh-channels 0.1.0 — initial release"
git push origin sdk-ts-v0.1.0
```

- [ ] **Step 6: Checkpoint**

No commit needed (publish is external state). Final report at the end of Plan B should include the published version + npm URL.

---

## Acceptance

- [x] `@agent-ops/sesh-channels` exists at `agents/sdk-ts/src/index.ts` (single file) in sesh — Tasks 2, 4
- [x] Tests cover `readRoleClass` and `readAdapterConfig` behavior (8 tests, no tautologies) — Task 3
- [x] Build produces ESM + type declarations — Task 4
- [x] README cites the proposal as source of truth — Task 5
- [x] v0.1.0 published to npm, install works from outside the repo — Task 6
- [x] Public surface is 4 symbols (`readRoleClass`, `readAdapterConfig`, `ConfigError`, type aliases). Internal helpers stay internal.

---

## Out of scope

- **CI auto-publish on tag push.** Manual publish only for 0.1.0; automation is a follow-up.
- **Go-to-TS code generation.** The two implementations stay hand-mirrored. If drift becomes a real problem, revisit.
- **Versioning policy doc.** Mentioned briefly in README; full policy doc deferred.
- **Adapter migration.** That's Plan B.
