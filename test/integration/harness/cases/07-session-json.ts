// 07-session-json.ts
//
// Inspects /workspace/.sesh/sessions/smoke-test.json (the session state
// JSON the sesh agent_watcher keeps current) and asserts both agents
// appear with the role/class flags `sesh up --exec` was given.

import { readFileSync } from "node:fs";
import type { CaseContext, CaseResult } from "../harness";

const PATH = "/workspace/.sesh/sessions/smoke-test.json";

interface AgentRef {
  agent?: string;
  owner?: string;
  instance_id?: string;
  subject?: string;
  role?: string;
  class?: string;
}

export async function run(ctx: CaseContext): Promise<CaseResult> {
  let raw: string;
  try {
    raw = readFileSync(PATH, "utf8");
  } catch (e) {
    return {
      name: "07-session-json",
      ok: false,
      reason: `cannot read ${PATH}: ${(e as Error).message}`,
    };
  }
  let parsed: { agents?: AgentRef[] };
  try {
    parsed = JSON.parse(raw);
  } catch (e) {
    return {
      name: "07-session-json",
      ok: false,
      reason: `not JSON: ${(e as Error).message}`,
      detail: raw.slice(0, 200),
    };
  }
  const agents = parsed.agents ?? [];
  if (agents.length !== 2) {
    return {
      name: "07-session-json",
      ok: false,
      reason: `expected agents.length=2, got ${agents.length}`,
      detail: agents,
    };
  }

  const cc = agents.find(a => a.agent === "claude-code");
  const op = agents.find(a => a.agent === "op");
  const failures: string[] = [];
  if (!cc) failures.push("no claude-code entry");
  if (!op) failures.push("no op entry");
  if (cc) {
    if (cc.role !== "implementer") failures.push(`cc.role=${cc.role} want implementer`);
    if (cc.class !== "active") failures.push(`cc.class=${cc.class}`);
    if (!cc.subject?.startsWith(`agents.prompt.cc.${ctx.owner}.`))
      failures.push(`cc.subject=${cc.subject}`);
  }
  if (op) {
    if (op.role !== "planner") failures.push(`op.role=${op.role} want planner`);
    if (op.class !== "active") failures.push(`op.class=${op.class}`);
    if (!op.subject?.startsWith(`agents.prompt.op.${ctx.owner}.`))
      failures.push(`op.subject=${op.subject}`);
  }

  if (failures.length > 0) {
    return {
      name: "07-session-json",
      ok: false,
      reason: failures.join("; "),
      detail: agents,
    };
  }
  return {
    name: "07-session-json",
    ok: true,
    reason: `agents=[${agents.map(a => `${a.agent}:${a.role}/${a.class}`).join(", ")}]`,
    detail: agents,
  };
}
