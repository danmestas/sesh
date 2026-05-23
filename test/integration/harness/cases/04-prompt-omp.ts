// 04-prompt-omp.ts
//
// Same shape as 03-prompt-claude.ts but targets the OMP agent
// (metadata.agent === "op"). OMP can be slower to bootstrap a model
// call, so the timeout is longer.

import type { CaseContext, CaseResult } from "../harness";

const MAX_WAIT_MS = 120_000;

export async function run(ctx: CaseContext): Promise<CaseResult> {
  const found = await ctx.agents.discover({ maxWaitMs: 3000 });
  const target = found.find(a =>
    a.agent === "op" && a.owner === ctx.owner && a.name === ctx.session,
  );
  if (!target) {
    return {
      name: "04-prompt-omp",
      ok: false,
      reason: `no op/${ctx.owner}/${ctx.session} agent visible to discover()`,
      detail: found.map(a => ({ agent: a.agent, owner: a.owner, name: a.name })),
    };
  }

  let sawAck = false;
  let response = "";
  let chunkCount = 0;
  const start = Date.now();
  try {
    const stream = await target.prompt("Reply with the single word: SUCCESS.", {
      inactivityTimeoutMs: 90_000,
      maxWaitMs: MAX_WAIT_MS,
    });
    for await (const chunk of stream) {
      chunkCount++;
      if (chunk.type === "status" && chunk.status === "ack") sawAck = true;
      if (chunk.type === "response") response += chunk.text;
    }
  } catch (e) {
    return {
      name: "04-prompt-omp",
      ok: false,
      reason: `stream error: ${(e as Error).message}`,
      detail: { chunkCount, response, sawAck, elapsedMs: Date.now() - start },
    };
  }

  const ok = /success/i.test(response);
  return {
    name: "04-prompt-omp",
    ok,
    reason: ok
      ? `chunks=${chunkCount} elapsed=${Date.now() - start}ms ack=${sawAck}`
      : `response did not contain "SUCCESS"; got ${JSON.stringify(response.slice(0, 200))}`,
    detail: { chunkCount, response: response.slice(0, 500), sawAck, elapsedMs: Date.now() - start },
  };
}
