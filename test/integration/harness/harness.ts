#!/usr/bin/env bun
//
// harness.ts — runs inside the container after entrypoint.sh's settle period.
//
// 1. Reads the NATS URL from /root/.sesh/hub.nats.url (the same file the
//    sesh-channels adapters resolve).
// 2. Connects via @nats-io/transport-node + constructs an Agents client.
// 3. Imports every case under ./cases/ alphabetically and runs them in
//    sequence — sequential because some cases inspect $SRV state that
//    parallel prompt activity would perturb.
// 4. Prints a markdown PASS/FAIL table + JSON dump.
//
// Exit code: 0 if all cases pass; 1 if any case fails. Cases throwing an
// error are recorded as FAIL with the error message as the reason.

import { connect, type NatsConnection } from "@nats-io/transport-node";
import { Agents } from "@synadia-ai/agents";
import { readFileSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// Hub URL: prefer the live file (it tracks restarts); fall back to the
// snapshot the entrypoint cached at first sighting so a hub that's gone
// idle and exited still leaves us a debug artifact.
const HUB_URL_FILE_PRIMARY = `${process.env.HOME ?? "/home/integ"}/.sesh/hub.nats.url`;
const HUB_URL_FILE_CACHED  = "/var/artifacts/hub.nats.url";
const OWNER = process.env.USER || "integ";
const SESSION = "smoke-test";

const __dirname = dirname(fileURLToPath(import.meta.url));

export interface CaseContext {
  nc: NatsConnection;
  agents: Agents;
  owner: string;
  session: string;
}

export interface CaseResult {
  name: string;
  ok: boolean;
  reason?: string;
  detail?: unknown;
}

export type CaseRunner = (ctx: CaseContext) => Promise<CaseResult>;

function readHubUrl(): string {
  for (const p of [HUB_URL_FILE_PRIMARY, HUB_URL_FILE_CACHED]) {
    try {
      const url = readFileSync(p, "utf8").trim();
      if (url) return url;
    } catch {
      // fall through
    }
  }
  throw new Error(`no hub URL at ${HUB_URL_FILE_PRIMARY} or ${HUB_URL_FILE_CACHED}`);
}

async function loadCases(): Promise<Array<{ file: string; run: CaseRunner }>> {
  const dir = join(__dirname, "cases");
  const files = readdirSync(dir).filter(f => /^\d+-.*\.ts$/.test(f)).sort();
  const out: Array<{ file: string; run: CaseRunner }> = [];
  for (const file of files) {
    const mod = (await import(join(dir, file))) as { run?: CaseRunner };
    if (typeof mod.run !== "function") {
      throw new Error(`case ${file} does not export 'run'`);
    }
    out.push({ file, run: mod.run });
  }
  return out;
}

function printMarkdown(results: CaseResult[]): void {
  console.log("\n## Integration rig — case results\n");
  console.log("| # | Case | Status | Reason |");
  console.log("|---|------|--------|--------|");
  results.forEach((r, i) => {
    const status = r.ok ? "PASS" : "FAIL";
    const reason = (r.reason ?? "").replace(/\|/g, "\\|").slice(0, 200);
    console.log(`| ${i + 1} | ${r.name} | ${status} | ${reason} |`);
  });
  const passed = results.filter(r => r.ok).length;
  console.log(`\n**Summary:** ${passed}/${results.length} passing\n`);
}

async function main(): Promise<number> {
  const hubUrl = readHubUrl();
  console.log(`[harness] connecting to ${hubUrl}`);

  const nc = await connect({ servers: hubUrl, name: "integ-harness" });
  console.log(`[harness] connected (max_payload=${nc.info?.max_payload})`);

  const agents = new Agents({ nc });

  const ctx: CaseContext = {
    nc,
    agents,
    owner: OWNER,
    session: SESSION,
  };

  const cases = await loadCases();
  console.log(`[harness] loaded ${cases.length} cases`);

  const results: CaseResult[] = [];
  for (const c of cases) {
    const tag = c.file.replace(/\.ts$/, "");
    console.log(`\n[harness] running ${tag}`);
    const t0 = Date.now();
    let r: CaseResult;
    try {
      r = await c.run(ctx);
    } catch (e) {
      r = {
        name: tag,
        ok: false,
        reason: `threw: ${(e as Error).message}`,
        detail: (e as Error).stack,
      };
    }
    const dt = Date.now() - t0;
    console.log(`[harness] ${tag} → ${r.ok ? "PASS" : "FAIL"} (${dt}ms)${r.reason ? " — " + r.reason : ""}`);
    results.push(r);
  }

  printMarkdown(results);
  console.log("\n```json");
  console.log(JSON.stringify(results, null, 2));
  console.log("```");

  await agents.close?.();
  await nc.drain();

  return results.every(r => r.ok) ? 0 : 1;
}

main().then(code => process.exit(code)).catch(e => {
  console.error("[harness] FATAL:", e);
  process.exit(2);
});
