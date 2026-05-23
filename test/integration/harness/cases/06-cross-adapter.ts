// 06-cross-adapter.ts — caller-mediated cross-talk (interpretation (a)
// per the plan §"Cross-adapter"). Asks claude for a short instruction,
// then passes claude's response to omp and asks omp to echo it back.
// The two streams sharing one harness verify both bus halves work
// concurrently. True agent-to-agent (interpretations b/c) requires
// adapter MCP-tool support that isn't shipped today.

import type { CaseContext, CaseResult } from "../harness";

const MAX_WAIT_MS = 120_000;

async function promptOne(
  ctx: CaseContext,
  kind: "claude-code" | "op",
  text: string,
  inactivityTimeoutMs = 60_000,
): Promise<string> {
  const found = await ctx.agents.discover({ maxWaitMs: 3000 });
  const target = found.find(a =>
    a.agent === kind && a.owner === ctx.owner && a.name === ctx.session,
  );
  if (!target) throw new Error(`no ${kind} agent reachable`);
  const stream = await target.prompt(text, { inactivityTimeoutMs, maxWaitMs: MAX_WAIT_MS });
  let out = "";
  for await (const chunk of stream) {
    if (chunk.type === "response") out += chunk.text;
  }
  return out.trim();
}

export async function run(ctx: CaseContext): Promise<CaseResult> {
  let claudeOut = "";
  let ompOut = "";
  try {
    claudeOut = await promptOne(ctx, "claude-code",
      "Reply with exactly one short word (no punctuation): a tasty fruit.");
  } catch (e) {
    return {
      name: "06-cross-adapter",
      ok: false,
      reason: `claude leg failed: ${(e as Error).message}`,
    };
  }

  try {
    ompOut = await promptOne(ctx, "op",
      `Reply with just this word, capitalised, no other text: ${claudeOut}`);
  } catch (e) {
    return {
      name: "06-cross-adapter",
      ok: false,
      reason: `omp leg failed: ${(e as Error).message}`,
      detail: { claudeOut },
    };
  }

  // Loose match — accept if omp's reply contains any non-empty substring
  // of claude's reply. Models can be picky about formatting; the goal is
  // to verify the bus carries both calls successfully, not exact echo.
  const ok = !!claudeOut && !!ompOut && ompOut.toLowerCase().includes(claudeOut.split(/\s+/)[0]!.toLowerCase().slice(0, 4));
  return {
    name: "06-cross-adapter",
    ok,
    reason: ok
      ? `claude said "${claudeOut.slice(0, 40)}"; omp echoed "${ompOut.slice(0, 40)}"`
      : `weak echo — claude="${claudeOut.slice(0, 40)}" omp="${ompOut.slice(0, 40)}"`,
    detail: { claudeOut: claudeOut.slice(0, 200), ompOut: ompOut.slice(0, 200) },
  };
}
