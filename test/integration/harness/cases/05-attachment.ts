// 05-attachment.ts
//
// Sends a tiny 10-byte attachment ("hello sesh") to the claude-code
// agent and asks for the byte length. PASSes if the response mentions
// 10 (case-insensitive). Skipped (recorded as PASS with skip note) if
// the prompt endpoint advertises attachments_ok=false.

import type { CaseContext, CaseResult } from "../harness";

const PAYLOAD = "hello sesh"; // 10 bytes
const MAX_WAIT_MS = 90_000;

export async function run(ctx: CaseContext): Promise<CaseResult> {
  const found = await ctx.agents.discover({ maxWaitMs: 3000 });
  const target = found.find(a =>
    a.agent === "claude-code" && a.owner === ctx.owner && a.name === ctx.session,
  );
  if (!target) {
    return {
      name: "05-attachment",
      ok: false,
      reason: "claude-code agent not discoverable",
      detail: found.map(a => `${a.agent}/${a.owner}/${a.name}`),
    };
  }

  // The Agent's endpoints[] reports attachments_ok parsed to a boolean.
  const prompt = target.endpoints?.find(e => e.name === "prompt");
  if (prompt?.attachmentsOk === false) {
    return {
      name: "05-attachment",
      ok: true,
      reason: "claude-code prompt endpoint declares attachments_ok=false; case skipped",
      detail: { attachmentsOk: prompt?.attachmentsOk },
    };
  }

  let response = "";
  let chunkCount = 0;
  try {
    const stream = await target.prompt(
      "I sent you one attachment. Reply with just the byte length of the attachment as a number.",
      {
        attachments: [
          { filename: "test.txt", content: new TextEncoder().encode(PAYLOAD) },
        ],
        inactivityTimeoutMs: 60_000,
        maxWaitMs: MAX_WAIT_MS,
      },
    );
    for await (const chunk of stream) {
      chunkCount++;
      if (chunk.type === "response") response += chunk.text;
    }
  } catch (e) {
    return {
      name: "05-attachment",
      ok: false,
      reason: `stream error: ${(e as Error).message}`,
      detail: { chunkCount, response },
    };
  }

  const ok = /\b10\b/.test(response);
  return {
    name: "05-attachment",
    ok,
    reason: ok
      ? `response correctly cited 10 bytes`
      : `expected "10" in response; got ${JSON.stringify(response.slice(0, 200))}`,
    detail: { chunkCount, response: response.slice(0, 500) },
  };
}
