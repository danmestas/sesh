// 08-steady-state.ts
//
// 60-second steady-state stability check (the plan's 90s was scaled down
// to make the full suite finish closer to a CI budget). Subscribes to
// agents.hb.>, queries $SRV.INFO.agents every 15s, and asserts:
//   - heartbeat count grows monotonically across snapshots
//   - the set of agents returned by INFO is stable across snapshots
//   - both adapters survive without disappearing once

import type { CaseContext, CaseResult } from "../harness";

const WINDOW_MS = 60_000;
const SAMPLE_EVERY_MS = 15_000;

async function snapshotAgents(ctx: CaseContext): Promise<Set<string>> {
  const inbox = ctx.nc.newInbox();
  const sub = ctx.nc.subscribe(inbox);
  ctx.nc.publish("$SRV.INFO.agents", new Uint8Array(0), { reply: inbox });
  const ids = new Set<string>();
  const stop = Date.now() + 2000;
  (async () => {
    for await (const m of sub) {
      try {
        const info = JSON.parse(new TextDecoder().decode(m.data)) as { id?: string; metadata?: Record<string, string> };
        if (info.metadata?.session === ctx.session && info.metadata?.owner === ctx.owner && info.id) {
          ids.add(`${info.metadata?.agent}/${info.id}`);
        }
      } catch { /* skip */ }
    }
  })();
  while (Date.now() < stop) await new Promise(r => setTimeout(r, 100));
  sub.unsubscribe();
  return ids;
}

export async function run(ctx: CaseContext): Promise<CaseResult> {
  // Heartbeat counter
  const sub = ctx.nc.subscribe("agents.hb.>");
  const hbCounts: number[] = [];
  let hbTotal = 0;
  (async () => {
    for await (const m of sub) {
      const t = m.subject.split(".");
      if (t.length === 5 && t[3] === ctx.owner && t[4] === ctx.session) hbTotal++;
    }
  })();

  const startedAt = Date.now();
  const snapshots: Array<{ tMs: number; agents: string[] }> = [];

  while (Date.now() - startedAt < WINDOW_MS) {
    const ids = await snapshotAgents(ctx);
    snapshots.push({ tMs: Date.now() - startedAt, agents: [...ids].sort() });
    hbCounts.push(hbTotal);
    await new Promise(r => setTimeout(r, SAMPLE_EVERY_MS));
  }

  sub.unsubscribe();

  // Assertions.
  const failures: string[] = [];

  // Heartbeats should strictly increase across snapshots (≥ previous; we
  // allow equality only when no time has passed, but 15s windows ensure
  // multiple ticks per gap).
  for (let i = 1; i < hbCounts.length; i++) {
    if (hbCounts[i]! <= hbCounts[i - 1]!) {
      failures.push(`hb count regressed: snapshot[${i - 1}]=${hbCounts[i - 1]} → [${i}]=${hbCounts[i]}`);
    }
  }

  // Agent set should be identical across all snapshots.
  const baseline = snapshots[0]?.agents.join("|") ?? "";
  for (let i = 1; i < snapshots.length; i++) {
    const cur = snapshots[i]!.agents.join("|");
    if (cur !== baseline) {
      failures.push(`snapshot ${i} set drift: was ${baseline} now ${cur}`);
    }
  }
  if (!baseline) failures.push("no agents in any snapshot");

  if (failures.length > 0) {
    return {
      name: "08-steady-state",
      ok: false,
      reason: failures.join("; "),
      detail: { hbCounts, snapshots },
    };
  }

  return {
    name: "08-steady-state",
    ok: true,
    reason: `hb total=${hbTotal} across ${snapshots.length} snapshots; agent set stable`,
    detail: { hbCounts, snapshots },
  };
}
