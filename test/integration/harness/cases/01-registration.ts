// 01-registration.ts
//
// Sends $SRV.INFO.agents, collects all replies for ~3s, and asserts that
// exactly two services responded with the expected metadata shape:
//   - one with metadata.agent === "claude-code"
//   - one with metadata.agent === "op"
// Both must carry the session, role, class, and protocol_version we set
// via `sesh up --exec`.
//
// This is the foundational case — every other case depends on adapters
// being reachable on the bus, so a failure here cascades.

import { createInbox } from "@nats-io/transport-node";
import type { CaseContext, CaseResult } from "../harness";

const WINDOW_MS = 3000;

interface ServiceInfo {
  type?: string;
  name?: string;
  id?: string;
  version?: string;
  metadata?: Record<string, string>;
  endpoints?: Array<{ name: string; subject: string; queue_group?: string; metadata?: Record<string, string> }>;
}

async function collectInfo(ctx: CaseContext): Promise<ServiceInfo[]> {
  const inbox = createInbox();
  const sub = ctx.nc.subscribe(inbox);
  ctx.nc.publish("$SRV.INFO.agents", new Uint8Array(0), { reply: inbox });
  const out: ServiceInfo[] = [];
  const stop = Date.now() + WINDOW_MS;
  (async () => {
    for await (const m of sub) {
      try {
        out.push(JSON.parse(new TextDecoder().decode(m.data)) as ServiceInfo);
      } catch {
        // ignore non-JSON; not our concern
      }
    }
  })();
  while (Date.now() < stop) await new Promise(r => setTimeout(r, 100));
  sub.unsubscribe();
  return out;
}

export async function run(ctx: CaseContext): Promise<CaseResult> {
  const infos = await collectInfo(ctx);
  const ours = infos.filter(i =>
    i?.metadata?.session === ctx.session && i?.metadata?.owner === ctx.owner,
  );

  if (ours.length !== 2) {
    return {
      name: "01-registration",
      ok: false,
      reason: `expected 2 services for session=${ctx.session} owner=${ctx.owner}; got ${ours.length} (total seen: ${infos.length})`,
      detail: infos.map(i => ({ agent: i.metadata?.agent, owner: i.metadata?.owner, session: i.metadata?.session, role: i.metadata?.role, class: i.metadata?.class })),
    };
  }

  const cc = ours.find(i => i.metadata?.agent === "claude-code");
  const op = ours.find(i => i.metadata?.agent === "op");
  if (!cc) return { name: "01-registration", ok: false, reason: "no claude-code service", detail: ours };
  if (!op) return { name: "01-registration", ok: false, reason: "no op (omp) service", detail: ours };

  const failures: string[] = [];
  for (const [tag, info] of [["claude-code", cc], ["op", op]] as const) {
    const m = info.metadata ?? {};
    if (m.session !== ctx.session) failures.push(`${tag}.session=${m.session}`);
    if (m.owner !== ctx.owner) failures.push(`${tag}.owner=${m.owner}`);
    if (!m.protocol_version?.startsWith("0.")) failures.push(`${tag}.protocol_version=${m.protocol_version}`);
    if (!m.role) failures.push(`${tag}.role missing`);
    if (!m.class) failures.push(`${tag}.class missing`);
  }
  if (cc.metadata?.role !== "implementer") failures.push(`claude-code.role=${cc.metadata?.role} want implementer`);
  if (op.metadata?.role !== "planner") failures.push(`op.role=${op.metadata?.role} want planner`);
  if (cc.metadata?.class !== "active") failures.push(`claude-code.class=${cc.metadata?.class}`);
  if (op.metadata?.class !== "active") failures.push(`op.class=${op.metadata?.class}`);

  if (failures.length > 0) {
    return { name: "01-registration", ok: false, reason: failures.join("; "), detail: ours };
  }

  return {
    name: "01-registration",
    ok: true,
    reason: `both agents present with correct metadata`,
    detail: { cc: cc.metadata, op: op.metadata },
  };
}
