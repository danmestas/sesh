// 03-prompt-claude.ts
//
// Discovers the agents, picks the claude-code one, and sends a tiny
// prompt that asks for the literal word SUCCESS. We accept any reply
// containing that token (case-insensitive) since claude may add
// punctuation or framing.
//
// Per §6.4 the first chunk on the reply stream must be a status "ack".
// We don't strictly fail if it's absent (some adapters short-circuit
// on small prompts), but we record its presence in the detail block.

import type { CaseContext, CaseResult } from "../harness";

const MAX_WAIT_MS = 90_000;

export async function run(ctx: CaseContext): Promise<CaseResult> {
  const found = await ctx.agents.discover({ maxWaitMs: 3000 });
  const target = found.find(a =>
    a.agent === "claude-code" && a.owner === ctx.owner && a.name === ctx.session,
  );
  if (!target) {
    return {
      name: "03-prompt-claude",
      ok: false,
      reason: `no claude-code/${ctx.owner}/${ctx.session} agent visible to discover()`,
      detail: found.map(a => ({ agent: a.agent, owner: a.owner, name: a.name })),
    };
  }

  let sawAck = false;
  let response = "";
  let chunkCount = 0;
  const start = Date.now();
  const stream = await target.prompt("Reply with the single word: SUCCESS.", {
    inactivityTimeoutMs: 60_000,
    maxWaitMs: MAX_WAIT_MS,
  });
  try {
    for await (const chunk of stream) {
      chunkCount++;
      if (chunk.type === "status" && chunk.status === "ack") sawAck = true;
      if (chunk.type === "response") response += chunk.text;
    }
  } catch (e) {
    return {
      name: "03-prompt-claude",
      ok: false,
      reason: `stream error: ${(e as Error).message}`,
      detail: { chunkCount, response, sawAck, elapsedMs: Date.now() - start },
    };
  }

  const ok = /success/i.test(response);
  return {
    name: "03-prompt-claude",
    ok,
    reason: ok
      ? `chunks=${chunkCount} elapsed=${Date.now() - start}ms ack=${sawAck}`
      : `response did not contain "SUCCESS"; got ${JSON.stringify(response.slice(0, 200))}`,
    detail: { chunkCount, response: response.slice(0, 500), sawAck, elapsedMs: Date.now() - start },
  };
}
