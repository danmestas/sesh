// 02-heartbeats.ts
//
// Subscribes to agents.hb.> for 20 seconds. Asserts that at least two
// heartbeats arrive per agent (claude-code + op) in that window — both
// channels pin cadence at 5 seconds locally so 20s ≥ 3 ticks per agent
// with comfortable headroom for boot jitter.
//
// Per §8.1 v0.3 the heartbeat subject shape is
// `agents.hb.<subjectToken>.<owner>.<name>` (5 tokens).

import type { CaseContext, CaseResult } from "../harness";

const WINDOW_MS = 20_000;

interface HeartbeatPayload {
  agent?: string;
  owner?: string;
  session?: string;
  instance_id?: string;
  protocol_version?: string;
  role?: string;
  class?: string;
}

export async function run(ctx: CaseContext): Promise<CaseResult> {
  const sub = ctx.nc.subscribe("agents.hb.>");
  const counts = new Map<string, number>(); // agentKind → count
  const subjects = new Set<string>();
  let lastBySubject = new Map<string, HeartbeatPayload>();

  (async () => {
    for await (const m of sub) {
      subjects.add(m.subject);
      const tokens = m.subject.split(".");
      // agents.hb.<subjectToken>.<owner>.<name>
      if (tokens.length === 5 && tokens[3] === ctx.owner && tokens[4] === ctx.session) {
        const subjectToken = tokens[2];
        counts.set(subjectToken!, (counts.get(subjectToken!) ?? 0) + 1);
        try {
          lastBySubject.set(m.subject, JSON.parse(new TextDecoder().decode(m.data)));
        } catch {
          // non-JSON heartbeat; record presence but no payload
        }
      }
    }
  })();

  await new Promise(r => setTimeout(r, WINDOW_MS));
  sub.unsubscribe();

  const cc = counts.get("cc") ?? 0;
  const op = counts.get("oh-my-pi") ?? 0;
  const failures: string[] = [];
  if (cc < 2) failures.push(`cc heartbeats=${cc} (want ≥2)`);
  if (op < 2) failures.push(`op heartbeats=${op} (want ≥2)`);
  for (const [subj, p] of lastBySubject) {
    if (p.session !== ctx.session) failures.push(`${subj}.session=${p.session}`);
    if (p.owner !== ctx.owner) failures.push(`${subj}.owner=${p.owner}`);
  }
  // 5-token shape check
  for (const s of subjects) {
    const t = s.split(".");
    if (t.length !== 5) failures.push(`subject ${s} not 5-token`);
  }
  if (failures.length > 0) {
    return {
      name: "02-heartbeats",
      ok: false,
      reason: failures.join("; "),
      detail: { counts: Object.fromEntries(counts), subjects: [...subjects] },
    };
  }
  return {
    name: "02-heartbeats",
    ok: true,
    reason: `cc=${cc} op=${op} heartbeats in ${WINDOW_MS}ms`,
    detail: { counts: Object.fromEntries(counts), subjects: [...subjects] },
  };
}
