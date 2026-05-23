// 06-cross-adapter.ts — caller-mediated multi-round conversation between
// claude (agent A) and OMP (agent B), with the harness brokering the relay.
//
// Three rounds of chained arithmetic — easy to verify the bus carries
// substantive content (not just ack/terminator) and that both adapters
// engage the model on each turn:
//
//   Round 1 (claude): "7 + 5 = ?"             → must contain "12"
//   Round 2 (omp):    "square 12"             → must contain "144"
//   Round 3 (claude): "reverse digits of 144" → must contain "441"
//
// If any round fails (timeout, error, missing expected substring) the case
// fails with the round number, what we sent, what we got, and the running
// context (all prior round texts truncated to 200 chars).
//
// Timing budget: 60s inactivity / 120s overall per round; the whole case
// has a soft budget of 360s, capped by the harness's per-case timeout
// (currently the runner's overall budget, not per-case).

import type { CaseContext, CaseResult } from "../harness";

const MAX_WAIT_MS = 120_000;
const INACTIVITY_TIMEOUT_MS = 60_000;

async function promptOne(
  ctx: CaseContext,
  kind: "claude-code" | "op",
  text: string,
  inactivityTimeoutMs = INACTIVITY_TIMEOUT_MS,
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

interface Round {
  n: number;
  target: "claude-code" | "op";
  prompt: string;
  expect: string;          // case-insensitive substring the reply must contain
  reply?: string;
  ok?: boolean;
  errorMessage?: string;
}

async function runMultiRound(ctx: CaseContext, rounds: Round[]): Promise<{
  rounds: Round[];
  firstFailure?: Round;
}> {
  for (const r of rounds) {
    try {
      r.reply = await promptOne(ctx, r.target, r.prompt);
      r.ok = r.reply.toLowerCase().includes(r.expect.toLowerCase());
      if (!r.ok) {
        r.errorMessage = `expected reply to contain "${r.expect}" but got "${r.reply.slice(0, 200)}"`;
        return { rounds, firstFailure: r };
      }
    } catch (e) {
      r.ok = false;
      r.errorMessage = `prompt threw: ${(e as Error).message}`;
      return { rounds, firstFailure: r };
    }
  }
  return { rounds };
}

export async function run(ctx: CaseContext): Promise<CaseResult> {
  // Three-round chained-arithmetic conversation. Each round's text refers
  // back to the prior round's result, so a working relay produces three
  // self-consistent answers; a broken bus surfaces at whichever round
  // first fails.
  const rounds: Round[] = [
    {
      n: 1,
      target: "claude-code",
      prompt: "You're agent A. I need help with a 3-step calculation that involves another agent. Step 1 of 3: what is 7 + 5? Reply with just the number.",
      expect: "12",
    },
    {
      n: 2,
      target: "op",
      prompt: "You're agent B. Agent A computed the answer 12 for 7+5. Step 2 of 3: square that number. Reply with just the resulting number.",
      expect: "144",
    },
    {
      n: 3,
      target: "claude-code",
      prompt: "You're agent A again. Agent B got 144 by squaring 12. Step 3 of 3: reverse the digits of 144. Reply with just the resulting number.",
      expect: "441",
    },
  ];

  const start = Date.now();
  const result = await runMultiRound(ctx, rounds);
  const elapsedMs = Date.now() - start;

  // detail: per-round reply (truncated) + ok flag + any error.
  const detail = {
    elapsedMs,
    rounds: result.rounds.map(r => ({
      n: r.n,
      target: r.target,
      prompt: r.prompt.slice(0, 200),
      expect: r.expect,
      reply: r.reply?.slice(0, 200),
      ok: r.ok,
      errorMessage: r.errorMessage,
    })),
  };

  if (result.firstFailure) {
    const f = result.firstFailure;
    return {
      name: "06-cross-adapter",
      ok: false,
      reason: `round ${f.n} (${f.target}) failed: ${f.errorMessage}`,
      detail,
    };
  }

  return {
    name: "06-cross-adapter",
    ok: true,
    reason: `3 rounds OK in ${elapsedMs}ms — A:"${rounds[0]!.reply?.slice(0, 40)}" B:"${rounds[1]!.reply?.slice(0, 40)}" A:"${rounds[2]!.reply?.slice(0, 40)}"`,
    detail,
  };
}
