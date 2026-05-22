# Plan B — Promote `agent-channels/` → `sesh-channels` and migrate adapters to `@agent-ops/sesh-channels`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish `~/projects/agent-channels/` as `github.com/danmestas/sesh-channels`, rename the local dir, add a top-level README explaining the family, and migrate the 5 adapters' inline validators onto the just-published `@agent-ops/sesh-channels` SDK.

**Architecture:** Single GitHub repo, 5 adapter subdirs, one shared workspace `package.json` (optional — keeps `bun install` global). Each adapter's `config.ts` collapses from ~58 lines (verbatim validators) to ~10 lines (env reads + a single SDK import). The Synadia-owned adapters (claude/pi/omp) are migrated here because they live in this workspace too — but they remain *also* present at `synadia-ai/synadia-agents` upstream; we don't try to land changes there.

**Tech Stack:** Bun 1.2+, TypeScript, the new `@agent-ops/sesh-channels` npm package. GitHub Actions for CI (one matrix job per adapter, `bun test`).

**Source proposal:** `docs/proposals/2026-05-21-agent-role-registration.md`. SDK Plan: `2026-05-22-sesh-channels-promotion-planA-sdk-ts.md`.

**Depends on:** Plan A complete through Task 7 (`@agent-ops/sesh-channels@0.1.0` published to npm).

---

## File Structure

**Existing (local-only):** `~/projects/agent-channels/` with 5 adapter subdirs + a snapshot commit + Phase 2 commits on `main`.

**Plan changes:**

```
~/projects/sesh-channels/                    # renamed from agent-channels
├── README.md                                # new — explains the family
├── .github/
│   └── workflows/
│       └── ci.yml                           # new — bun test matrix
├── package.json                             # new — workspace root (optional)
├── claude-nats-channel/
│   ├── config.ts                            # MODIFIED — imports from @agent-ops/sesh-channels
│   ├── test/config.test.ts                  # MODIFIED — imports + simplified assertions
│   └── package.json                         # MODIFIED — depend on @agent-ops/sesh-channels
├── pi-nats-channel/
│   └── (same shape)
├── omp-nats-channel/
│   └── (same shape)
├── gemini-nats-channel/
│   └── (same shape; nested .git stays untouched — its own remote)
└── grok-nats-channel/
    └── (same shape; nested .git stays untouched — its own remote)
```

**Modify:** Each adapter's `config.ts`, `test/config.test.ts`, `package.json`.

---

## Task 1: Create the GitHub repo

**Files:** none in the repo yet — this is registry setup.

- [ ] **Step 1: Check the repo name is free**

Run:

```bash
gh repo view danmestas/sesh-channels 2>&1 | head -3 || echo "available"
```

Expected: "Could not resolve to a Repository" or similar → available. If a repo already exists, stop and confer with the operator.

- [ ] **Step 2: Create the repo**

Run:

```bash
cd /Users/dmestas/projects/agent-channels
gh repo create danmestas/sesh-channels --public --source=. --remote=origin --description "Official sesh NATS-channel adapter family (claude / pi / omp / gemini / grok). Each adapter implements the Synadia Agent Protocol for one agent CLI and depends on @agent-ops/sesh-channels for the canonical role/class rules."
```

Expected: prints the new repo URL. `git remote -v` should now show `origin → https://github.com/danmestas/sesh-channels.git`.

- [ ] **Step 3: Push current main**

Run:

```bash
git push -u origin main
```

Expected: 6 commits pushed (the snapshot commit + 5 per-adapter Phase 2 commits).

- [ ] **Step 4: Verify on GitHub**

Run:

```bash
gh repo view danmestas/sesh-channels --json url,defaultBranchRef -q '.url + " | default: " + .defaultBranchRef.name'
```

Expected: prints the URL and confirms `default: main`.

- [ ] **Step 5: Checkpoint**

No new commit. Registry state changed; document the URL in your final report.

---

## Task 2: Rename the local directory

**Files:** filesystem rename only.

- [ ] **Step 1: Move the directory**

Run:

```bash
mv ~/projects/agent-channels ~/projects/sesh-channels
```

- [ ] **Step 2: Verify**

Run:

```bash
ls -d ~/projects/sesh-channels ~/projects/agent-channels 2>&1
```

Expected: `sesh-channels` exists, `agent-channels` is "No such file or directory."

- [ ] **Step 3: Update any sesh references to the old path**

Run:

```bash
grep -rn 'agent-channels' ~/projects/sesh/docs/plans/ ~/projects/sesh/docs/proposals/ 2>&1 | head -20
```

Expected: surfaces references in the Plan files. For each, update `agent-channels` → `sesh-channels` (Edit tool, careful — only paths, not narrative text where "agent-channels" is the historic name).

If no references exist, skip.

- [ ] **Step 4: Checkpoint**

In the sesh repo, if any plan/proposal paths were updated:

```bash
cd /Users/dmestas/projects/sesh
git add docs/plans/ docs/proposals/
git commit -m "docs: update agent-channels → sesh-channels paths after repo rename"
```

If no doc changes were needed, no commit.

---

## Task 3: Branch for migration

**Files:** none yet.

- [ ] **Step 1: Create a feature branch**

Run:

```bash
cd ~/projects/sesh-channels
git checkout -b feat/migrate-to-agent-meta-sdk
git branch --show-current
```

Expected: prints `feat/migrate-to-agent-meta-sdk`.

All subsequent migration tasks (4-9) commit on this branch.

---

## Task 4: Migrate `claude-nats-channel`

**Files:**
- Modify: `~/projects/sesh-channels/claude-nats-channel/{config.ts,package.json,test/config.test.ts}`

- [ ] **Step 1: Add the SDK as a dependency**

Edit `claude-nats-channel/package.json` — add to `dependencies`:

```json
"@agent-ops/sesh-channels": "^0.1.0",
```

Then run:

```bash
cd ~/projects/sesh-channels/claude-nats-channel
bun install
```

Expected: `bun.lock` updated; `node_modules/@agent-ops/sesh-channels` exists.

- [ ] **Step 2: Replace `config.ts` with the SDK-using version**

Open `claude-nats-channel/config.ts`. Replace the entire file with:

```ts
import { readAdapterConfig, ConfigError } from "@agent-ops/sesh-channels";

export const readConfig = () => readAdapterConfig("claude-code");
export { ConfigError };
export type { AdapterConfig, AgentClass } from "@agent-ops/sesh-channels";
```

This collapses ~58 LoC of inline validators into ~5 LoC. Both the canonical rules AND the `AdapterConfig` interface now live in the SDK, so future fields (e.g., `instanceId`) are a single SDK edit + version bump rather than 5 adapter edits.

- [ ] **Step 3: Update `test/config.test.ts`**

The existing tests imported local symbols (`validateRole`, `validateClass`). Update the imports:

```ts
import { describe, expect, test, afterEach } from "bun:test";
import { ConfigError, readConfig } from "../config";
```

Tests that exercise `validateRole` / `validateClass` directly should be deleted (those are now covered by `@agent-ops/sesh-channels`'s own test suite). Tests on `readConfig` behavior stay.

After this, `test/config.test.ts` should contain only:
- "reads SESH_ROLE / SESH_CLASS from env" test
- "applies defaults when unset" test
- "readConfig throws if SESH_CLASS is invalid" test
- "readConfig throws if SESH_ROLE is invalid" test (newly added per Phase 2 spec-compliance reviewer's nit)

Use this exact body:

```ts
import { describe, expect, test, afterEach } from "bun:test";
import { ConfigError, readConfig } from "../config";

describe("readConfig", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
  });

  test("reads SESH_ROLE and SESH_CLASS from env", () => {
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const c = readConfig();
    expect(c.role).toBe("implementer");
    expect(c.class).toBe("active");
  });

  test("defaults role=worker class=active when unset", () => {
    delete process.env.SESH_ROLE;
    delete process.env.SESH_CLASS;
    const c = readConfig();
    expect(c.role).toBe("worker");
    expect(c.class).toBe("active");
  });

  test("throws ConfigError when SESH_CLASS is invalid", () => {
    process.env.SESH_CLASS = "passive";
    expect(() => readConfig()).toThrow(ConfigError);
  });

  test("throws ConfigError when SESH_ROLE is invalid", () => {
    process.env.SESH_ROLE = "Bad Role";
    process.env.SESH_CLASS = "active";
    expect(() => readConfig()).toThrow(ConfigError);
  });
});
```

- [ ] **Step 4: Run tests**

Run:

```bash
cd ~/projects/sesh-channels/claude-nats-channel
bun test
```

Expected: 4 tests PASS, 0 failures. No "Cannot find module '@agent-ops/sesh-channels'" — the install in Step 1 should have wired it up.

- [ ] **Step 5: Verify server.ts still works**

The earlier Phase 2 commit added `metadata: { role: cfg.role, class: cfg.class }` to the `svcm.add` call in `server.ts`. Confirm no edit is needed to `server.ts` itself by running:

```bash
bun -e 'import("./config").then(m => console.log(m.readConfig()))'
```

Expected: prints a config object with `role: "worker"` and `class: "active"` (the defaults).

- [ ] **Step 6: Checkpoint**

Stage `config.ts`, `test/config.test.ts`, `package.json`, `bun.lock`. Commit: `refactor(claude-nats-channel): migrate config to @agent-ops/sesh-channels SDK`.

---

## Task 5: Migrate `pi-nats-channel`

Apply Task 4 verbatim with these per-adapter substitutions:

- File path: `pi-nats-channel/extensions/config.ts` (note `extensions/` subdir — confirm with `ls`)
- Test path: `pi-nats-channel/test/config.test.ts`
- Default `agent` value: `"pi"` (was `"claude-code"`)
- Indentation style: **tabs** (pi uses tabs — match existing style)

- [ ] **Step 1: Add SDK dep, run `bun install`**
- [ ] **Step 2: Replace `extensions/config.ts` with the SDK-using version (default agent: `"pi"`, tabs for indent)**
- [ ] **Step 3: Update `test/config.test.ts` to the 4-test version from Task 4 Step 3**
- [ ] **Step 4: `bun test` — expect 4 PASS**
- [ ] **Step 5: Verify `extensions/nats-channel.ts` still constructs metadata correctly (no edits needed)**
- [ ] **Step 6: Commit:** `refactor(pi-nats-channel): migrate config to @agent-ops/sesh-channels SDK`

---

## Task 6: Migrate `omp-nats-channel`

Same as Task 5 with these substitutions:

- File path: `omp-nats-channel/extensions/config.ts`
- Test path: `omp-nats-channel/test/config.test.ts`
- Default `agent` value: `"omp"`
- Indent: **tabs**

- [ ] Steps 1-6 mirror Task 5.

Commit: `refactor(omp-nats-channel): migrate config to @agent-ops/sesh-channels SDK`.

---

## Task 6.5: Decide nested-repo strategy for gemini and grok

**Why this step exists:** gemini and grok currently each have a nested `.git` inside the sesh-channels workspace, pointing at their own GitHub remotes. The naive "commit to both nested AND parent" workflow this plan originally proposed is the worst-of-three-options — implicit submodule semantics without declaring them, dual commits on every change, no clean sync. Resolve before Tasks 7-8 execute.

**Three options, pick one:**

### Option A — Declare as proper git submodules

Pros: standard tooling, `git submodule update` keeps in sync, CI's `submodules: recursive` checkout (Task 10) actually does the right thing.
Cons: submodules are notoriously painful for casual `git clone` users — they have to remember `--recurse-submodules`.

Steps if chosen:

```bash
cd ~/projects/sesh-channels
rm -rf gemini-nats-channel grok-nats-channel  # remove the worktrees (NOT the GitHub repos)
git submodule add https://github.com/danmestas/gemini-nats-channel.git gemini-nats-channel
git submodule add https://github.com/dmestas/grok-nats-channel.git grok-nats-channel
git commit -am "chore: declare gemini and grok as submodules (was implicit nested-repo)"
```

For Tasks 7-8: do the migration commits inside the submodule (its own branch/PR workflow), then `git submodule update --remote` from the parent to bump the pointer.

### Option B — Convert to plain subdirs (subtree)

Pros: a `git clone` of sesh-channels gets everything in one shot, no `--recurse-submodules` confusion. Each adapter lives entirely in one repo.
Cons: gemini and grok lose their independent release artifacts (their own GitHub repos go stale or get archived). Sync via `git subtree pull` becomes an explicit step.

Steps if chosen:

```bash
cd ~/projects/sesh-channels
rm -rf gemini-nats-channel grok-nats-channel
git subtree add --prefix=gemini-nats-channel https://github.com/danmestas/gemini-nats-channel.git main --squash
git subtree add --prefix=grok-nats-channel https://github.com/dmestas/grok-nats-channel.git main --squash
```

For Tasks 7-8: just edit files like any normal subdir. The nested `.git` is gone. To later sync upstream changes (or push back): `git subtree pull/push --prefix=<dir> <remote> <branch> --squash`.

### Option C — Detach (vendor)

Pros: simplest mental model — gemini and grok become "vendored copies"; the canonical sources stay at their own repos and operators manually copy updates over.
Cons: explicit drift surface; the implicit "this is the SAME code as upstream" guarantee weakens.

Steps if chosen:

```bash
cd ~/projects/sesh-channels
rm -rf gemini-nats-channel/.git grok-nats-channel/.git
git add gemini-nats-channel/ grok-nats-channel/
git commit -m "chore: vendor gemini and grok adapter sources (was nested .git)"
```

For Tasks 7-8: edit normally, then manually replay the migration commit upstream at `github.com/danmestas/gemini-nats-channel` and `github.com/dmestas/grok-nats-channel`.

**Recommendation:** Option A (submodule) if you intend to keep gemini and grok as independent releasable artifacts (own npm packages, own CI). Option B (subtree) if sesh-channels is becoming their canonical home and the standalone repos are legacy. Option C (vendor) only if upstream-divergence is acceptable.

- [ ] **Step 1: Pick one option**

Document the choice in `~/projects/sesh-channels/README.md` (Task 9 covers writing the README; just hold the choice until then).

- [ ] **Step 2: Execute the chosen option's "Steps if chosen" block**

- [ ] **Step 3: Update Tasks 7 and 8 to match the chosen workflow**

For Option A: Tasks 7/8 stay roughly as written (commit inside submodule + bump pointer). For Option B/C: Tasks 7/8 become normal "edit + commit in sesh-channels" — no more dual-commit dance. Adjust the Steps accordingly.

- [ ] **Step 4: Checkpoint**

Commit with a message naming the chosen option (e.g., `chore: declare gemini/grok as submodules` or `chore: vendor gemini/grok adapter sources`).

---

## Task 7: Migrate `gemini-nats-channel`

**Caveat:** behavior depends on Task 6.5's option — submodule (commit inside submodule + bump pointer), subtree (normal subdir commit), or vendor (normal subdir commit + manual upstream replay). The steps below describe submodule (Option A) — adapt if a different option was chosen.

Substitutions:

- File path: `gemini-nats-channel/src/config.ts`
- Test path: `gemini-nats-channel/test/config.test.ts`
- Default `agent` value: `"gemini"`
- Indent: **spaces** (gemini's style)
- The gemini adapter uses `RunBridgeOptions` not `svcm.add` directly — DON'T touch `src/bridge.ts` (Phase 2 already wired `role`/`class` through `extraMetadata`); only `src/config.ts` and the test file change.

- [ ] **Step 1: Add SDK dep to gemini's own `package.json`, `bun install`**
- [ ] **Step 2: Replace `src/config.ts` with the SDK-using version**
- [ ] **Step 3: Update test file to the 4-test version**
- [ ] **Step 4: `bun test` PASS, then `bun run typecheck` clean**
- [ ] **Step 5: Commit in the nested gemini repo:** `refactor(config): migrate to @agent-ops/sesh-channels SDK (refs sesh#90)` — this is a commit on `github.com/danmestas/gemini-nats-channel:main`. Do NOT push without operator confirmation.
- [ ] **Step 6: Back in the parent sesh-channels repo, commit the gitlink bump:** `refactor(gemini-nats-channel): bump submodule pointer for @agent-ops/sesh-channels migration`.

---

## Task 8: Migrate `grok-nats-channel`

Same nested-repo caveat as gemini. Plus: grok was on `feat/grok-nats-launcher-skill` with uncommitted WIP (`server.ts` had 152 lines of pending sesh discovery work). That WIP must remain intact.

Substitutions:

- File path: `grok-nats-channel/config.ts`
- Test path: `grok-nats-channel/test/config.test.ts`
- Default `agent` value: `"grok"`
- Indent: **spaces**

- [ ] **Step 1: Confirm WIP state before touching anything**

Run:

```bash
cd ~/projects/sesh-channels/grok-nats-channel
git status --short
git stash list | head -3
```

Note any uncommitted changes. Stash them before applying the migration:

```bash
git stash push -u -m "WIP: grok sesh-launcher work — preserved during agent-meta SDK migration"
```

- [ ] **Step 2: Branch off main**

```bash
git checkout main 2>/dev/null || git checkout master  # whichever the repo's default is
git checkout -b feat/migrate-to-agent-meta-sdk
```

- [ ] **Step 3: Add SDK dep, `bun install`**
- [ ] **Step 4: Replace `config.ts`, update test, run tests (4 PASS)**
- [ ] **Step 5: Commit on the grok feature branch:** `refactor(config): migrate to @agent-ops/sesh-channels SDK (refs sesh#90)`
- [ ] **Step 6: Switch back to the original branch and restore stash**

```bash
git checkout feat/grok-nats-launcher-skill
git stash pop
```

Verify the WIP is restored intact:

```bash
git status --short
```

Expected: matches the pre-migration `git status` output from Step 1.

- [ ] **Step 7: Back in sesh-channels parent, commit the gitlink bump**

Commit: `refactor(grok-nats-channel): bump submodule pointer for @agent-ops/sesh-channels migration`.

---

## Task 9: Top-level `README.md`

**Files:**
- Create: `~/projects/sesh-channels/README.md`

- [ ] **Step 1: Write the README**

```markdown
# sesh-channels

Official **NATS-channel adapter family** for the [sesh](https://github.com/danmestas/sesh) agent mesh. Each adapter bridges one agent CLI (Claude Code, Codex, Pi, Gemini, Grok) onto the sesh mesh via the [Synadia Agent Protocol v0.3](https://github.com/synadia-ai/synadia-agent-sdk-docs).

## What lives here

| Adapter | Agent | Status | Upstream |
|---|---|---|---|
| `claude-nats-channel/` | Claude Code | sesh-flavored fork | Synadia: [`synadia-ai/synadia-agents/agents/claude-code`](https://github.com/synadia-ai/synadia-agents) |
| `pi-nats-channel/` | Pi | sesh-flavored fork | Synadia: same monorepo |
| `omp-nats-channel/` | OMP | sesh-flavored fork | Synadia: same monorepo |
| `gemini-nats-channel/` | Gemini CLI | canonical (nested .git → [danmestas/gemini-nats-channel](https://github.com/danmestas/gemini-nats-channel)) | — |
| `grok-nats-channel/` | Grok | canonical (nested .git → [dmestas/grok-nats-channel](https://github.com/dmestas/grok-nats-channel)) | — |

All adapters depend on [`@agent-ops/sesh-channels`](https://www.npmjs.com/package/@agent-ops/sesh-channels) for the canonical role/class types and validators.

## Layout

Every adapter follows the same shape:

\`\`\`
<adapter>/
├── package.json
├── server.ts (or extensions/nats-channel.ts, or src/cli.ts — varies by adapter)
├── config.ts (or extensions/config.ts, src/config.ts)
└── test/config.test.ts
\`\`\`

The `config.ts` reads `SESH_ROLE` / `SESH_CLASS` via `@agent-ops/sesh-channels`'s `readRoleClass()`, applies the canonical rules, and surfaces an `AdapterConfig` for the rest of the adapter to consume.

## Running an adapter

\`\`\`bash
cd <adapter>
bun install
SESH_ROLE=implementer SESH_CLASS=active SESH_SESSION=mysess bun run server.ts
\`\`\`

## Synadia-owned upstream

claude / pi / omp adapters also exist canonically at [`synadia-ai/synadia-agents`](https://github.com/synadia-ai/synadia-agents). The forks here add sesh-specific features (role/class metadata, future coordination subjects) without breaking Synadia v0.3 wire compatibility (additive metadata only — vanilla `@synadia-ai/agents` callers continue to discover and prompt unchanged).

## Tests

\`\`\`bash
# Per adapter
cd <adapter> && bun test

# All adapters
for d in claude-nats-channel pi-nats-channel omp-nats-channel gemini-nats-channel grok-nats-channel; do
  (cd "$d" && bun test) || echo "FAIL: $d"
done
\`\`\`

## License

Apache-2.0 (per-adapter LICENSE files for any adapter forks with different upstream licenses — check each).
```

- [ ] **Step 2: Checkpoint**

Commit: `docs: top-level README for the sesh-channels family`.

---

## Task 10: CI workflow

**Files:**
- Create: `~/projects/sesh-channels/.github/workflows/ci.yml`

- [ ] **Step 1: Write the workflow**

```yaml
name: ci
on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  test:
    name: bun test (all adapters)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          submodules: recursive   # noop if Option B/C of Task 6.5 was chosen
      - uses: oven-sh/setup-bun@v2
        with:
          bun-version: 1.2.x
      - name: Install + test every adapter
        run: |
          set -e
          for d in claude-nats-channel pi-nats-channel omp-nats-channel gemini-nats-channel grok-nats-channel; do
            echo "::group::$d"
            (cd "$d" && bun install --frozen-lockfile && bun test)
            echo "::endgroup::"
          done
```

Single-job iteration. Each adapter's test suite is ~50ms — matrix-level parallelism adds queueing and checkout overhead that dwarfs the actual work. Promote to a matrix only if per-adapter wall time exceeds ~30s.

- [ ] **Step 2: Push branch and confirm CI fires**

Run:

```bash
cd ~/projects/sesh-channels
git add .github/workflows/ci.yml
git commit -m "ci: bun test matrix across all 5 adapters"
git push origin feat/migrate-to-agent-meta-sdk
```

Then check:

```bash
gh run list --limit 1 --repo danmestas/sesh-channels
```

Expected: a run for the push event, status `queued` / `in_progress`. Watch it land.

- [ ] **Step 3: Confirm all 5 matrix legs pass**

Run:

```bash
gh run watch --repo danmestas/sesh-channels
```

Expected: 5 green checks (one per adapter). If any fail, fix and re-push.

---

## Task 11: Open the PR and merge on green

**Files:** none — PR-level orchestration.

- [ ] **Step 1: Open the PR**

```bash
cd ~/projects/sesh-channels
gh pr create --title "refactor: migrate all 5 adapters to @agent-ops/sesh-channels SDK" \
  --body "Collapses the 5 duplicated inline validators in each adapter's config.ts into a single import from @agent-ops/sesh-channels (just published from sesh's agents/sdk-ts/, v0.1.0). The canonical role/class rules now live in one place; adapter configs are ~15 LoC each instead of ~58.

Per-adapter: dep added, config.ts rewritten, test/config.test.ts trimmed to the 4 readConfig-level tests (validator unit tests now live in @agent-ops/sesh-channels's own suite), bun test green.

Wire-compat unchanged. Canonical-rules drift surface eliminated."
```

- [ ] **Step 2: Wait for CI green**

```bash
gh pr checks $(gh pr view --json number -q .number) --watch
```

Expected: 5 green legs.

- [ ] **Step 3: Squash-merge with branch delete**

```bash
gh pr merge --squash --delete-branch
```

- [ ] **Step 4: Local cleanup**

```bash
cd ~/projects/sesh-channels
git checkout main
git pull --ff-only
git remote prune origin
```

- [ ] **Step 5: Final report**

Print:
- npm URL: `https://www.npmjs.com/package/@agent-ops/sesh-channels`
- sesh-channels URL: `https://github.com/danmestas/sesh-channels`
- LoC saved: actual diff stat (run `git log --shortstat -1` on the squash commit)
- Nested-repo migration status: gemini-nats-channel + grok-nats-channel have local-only commits on their own `main`s; operator decides when to push them upstream.

---

## Acceptance

- [x] `github.com/danmestas/sesh-channels` exists, public, with the 5 adapters and a top-level README — Tasks 1, 9
- [x] Local dir renamed `agent-channels` → `sesh-channels` — Task 2
- [x] Each adapter depends on `@agent-ops/sesh-channels@^0.1.0` — Tasks 4-8
- [x] Each adapter's `config.ts` imports `readRoleClass` from `@agent-ops/sesh-channels` — Tasks 4-8
- [x] Each adapter's `test/config.test.ts` is the trimmed 4-test version — Tasks 4-8
- [x] CI runs `bun test` per adapter on PR + push to main — Task 10
- [x] PR merged on green — Task 11
- [x] gemini and grok nested-repo migrations committed locally on their `main`s (not pushed without operator) — Tasks 7, 8

---

## Out of scope

- **Pushing the nested gemini/grok commits to their own GitHub remotes.** Operator decides timing.
- **Synadia-side filing.** Per CLAUDE.md no-third-party-filing rule.
- **Coordination-subject SDK** — separate plan.
- **Capabilities / priority / future metadata fields** — once Plan A's SDK is in use, those additions are minor-version bumps to `@agent-ops/sesh-channels` rather than 5-way duplicate edits. Out of scope for this migration.
