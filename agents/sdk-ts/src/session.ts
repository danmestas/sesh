// SOURCE OF TRUTH for sesh session label resolution:
//   https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-22-sesh-up-exec-proposal.md
//
// Mirrors `discoverSessionLabel()` formerly inline in
// claude-nats-channel/server.ts. Adapters compose this helper with their
// own NATS_SESSION_NAME / config / basename(cwd) fallback chain.

import { readdirSync } from "node:fs";
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
  const warn = opts.warn ?? ((msg: string) => process.stderr.write(msg));
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
