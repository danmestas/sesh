// SOURCE OF TRUTH for these rules:
//   https://github.com/danmestas/sesh/blob/main/docs/proposals/2026-05-21-agent-role-registration.md
//   ("Canonical role/class rules" section)
//
// TypeScript port of github.com/danmestas/sesh/internal/agentmeta. Keep in
// lockstep with the Go side — drift between adapters and the hub corrupts
// the agents[] view in the session manifest.

export type AgentClass = "active" | "observer";

export interface AdapterRoleClass {
  role: string;
  class: AgentClass;
}

export interface AdapterConfig {
  natsUrl: string;
  agent: string;
  owner: string;
  session: string;
  role: string;
  class: AgentClass;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

const ROLE_RE = /^[a-z0-9_-]+$/;
const DEFAULT_ROLE = "worker";
const DEFAULT_CLASS: AgentClass = "active";

function validateRole(role: string): void {
  if (role.length === 0) throw new ConfigError("role is empty");
  if (role.length > 63) throw new ConfigError(`role ${JSON.stringify(role)} is ${role.length} bytes; max 63`);
  if (!ROLE_RE.test(role)) throw new ConfigError(`role ${JSON.stringify(role)} must match ^[a-z0-9_-]+$`);
}

function validateClass(c: string): asserts c is AgentClass {
  if (c !== "active" && c !== "observer") {
    throw new ConfigError(`class ${JSON.stringify(c)} must be "active" or "observer"`);
  }
}

/**
 * Read role and class from process.env (SESH_ROLE / SESH_CLASS), apply
 * defaults, then validate. Throws ConfigError on invalid input.
 */
export function readRoleClass(): AdapterRoleClass {
  const role = ((process.env.SESH_ROLE ?? "").trim()) || DEFAULT_ROLE;
  const cls = ((process.env.SESH_CLASS ?? "").trim()) || DEFAULT_CLASS;
  validateRole(role);
  validateClass(cls);
  return { role, class: cls };
}

/**
 * Read the full adapter config (NATS_URL, agent, owner, session, role, class)
 * from process.env. `defaultAgent` is the adapter's canonical agent name —
 * the only knob that differs across adapters.
 *
 * Throws ConfigError on invalid role/class. NATS_URL / owner / session fall
 * back to defaults when unset; nothing else throws.
 */
export function readAdapterConfig(opts: { defaultAgent: string }): AdapterConfig {
  const { role, class: cls } = readRoleClass();
  return {
    natsUrl: process.env.NATS_URL ?? "nats://localhost:4222",
    agent: process.env.SESH_AGENT ?? opts.defaultAgent,
    owner: process.env.SESH_OWNER ?? process.env.USER ?? "anon",
    session: process.env.SESH_SESSION ?? "",
    role,
    class: cls,
  };
}
