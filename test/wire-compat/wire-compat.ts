// Wire-compat test for `sesh-ref-agent` against the upstream
// `@synadia-ai/agents` TypeScript SDK. This proves the contract in
// `docs/synadia-agents-on-sesh.md` is implementable: the upstream SDK
// — which knows nothing about sesh — drives the Go agent and observes
// the full v0.3 stream lifecycle (ack → response → terminator).
//
// Run via `test/wire-compat/run.sh`. Prerequisites:
//   - A NATS server reachable at $NATS_URL (default: nats://127.0.0.1:4222).
//   - `sesh-ref-agent` running against the same broker.
//
// Exits 0 on success, non-zero on any assertion failure.

import { strict as assert } from "node:assert";
import { connect as natsConnect } from "@nats-io/transport-node";
import { Agents } from "@synadia-ai/agents";

async function main(): Promise<void> {
  const url = process.env["NATS_URL"] ?? "nats://127.0.0.1:4222";
  const prompt = process.argv[2] ?? "hello, world";

  const nc = await natsConnect({ servers: url });
  const agents = new Agents({ nc });

  try {
    const discovered = await agents.discover();
    assert.ok(
      discovered.length > 0,
      "no agents discovered — is sesh-ref-agent running against this broker?",
    );
    const agent = discovered.find((a) => a.metadata.agent === "echo")
      ?? discovered[0];

    // §3.2 metadata required fields.
    assert.equal(agent.metadata.protocol_version, "0.3",
      `protocol_version = ${agent.metadata.protocol_version}, want 0.3`);
    assert.ok(agent.metadata.agent, "metadata.agent required");
    assert.ok(agent.metadata.owner, "metadata.owner required");

    // §3 + §4 endpoints — discovered.subject is the prompt subject.
    assert.match(
      agent.subject ?? "",
      /^agents\.prompt\./,
      `prompt subject does not match agents.prompt.*: ${agent.subject}`,
    );

    // Drive the prompt endpoint and observe the §6 lifecycle.
    const collected: string[] = [];
    let sawAck = false;
    let sawDone = false;

    for await (const msg of await agent.prompt(prompt)) {
      switch (msg.type) {
        case "status":
          if (msg.status === "ack") sawAck = true;
          if (msg.status === "done") sawDone = true;
          break;
        case "response":
          collected.push(msg.text ?? "");
          break;
      }
    }

    assert.ok(sawAck, "missing §6.4 ack status chunk");
    assert.ok(sawDone, "missing §6.5 terminator (SDK surfaces as status:done)");

    const body = collected.join("");
    assert.equal(body, prompt,
      `echo body mismatch: got ${JSON.stringify(body)}, want ${JSON.stringify(prompt)}`);

    console.log("wire-compat OK:", { prompt, body, ack: sawAck, done: sawDone });
  } finally {
    await agents.close();
    await nc.close();
  }
}

void main().catch((err: unknown) => {
  console.error("wire-compat FAILED:", err);
  process.exit(1);
});
