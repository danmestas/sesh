import { describe, expect, test, beforeEach, afterEach } from "bun:test";
import { mkdirSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { readSessionLabel } from "../src/session";

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

  test("walks cwd to root for .sesh/sessions/<label>.json", () => {
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
      warn: (m) => {
        warned += m;
      },
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
