import { describe, expect, test, afterEach } from "bun:test";
import { ConfigError, readAdapterConfig, readRoleClass } from "../src/index";

describe("readRoleClass", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
  });

  test("reads SESH_ROLE and SESH_CLASS from env", () => {
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const got = readRoleClass();
    expect(got.role).toBe("implementer");
    expect(got.class).toBe("active");
  });

  test("applies defaults when env unset", () => {
    delete process.env.SESH_ROLE;
    delete process.env.SESH_CLASS;
    const got = readRoleClass();
    expect(got.role).toBe("worker");
    expect(got.class).toBe("active");
  });

  test("trims whitespace before defaulting and validating", () => {
    process.env.SESH_ROLE = "  worker  ";
    process.env.SESH_CLASS = "  observer  ";
    const got = readRoleClass();
    expect(got.role).toBe("worker");
    expect(got.class).toBe("observer");
  });

  test("throws ConfigError on invalid role (uppercase, space, slash, oversize)", () => {
    process.env.SESH_CLASS = "active";
    for (const bad of ["Worker", "im plementer", "im/plementer", "a".repeat(64)]) {
      process.env.SESH_ROLE = bad;
      expect(() => readRoleClass()).toThrow(ConfigError);
    }
  });

  test("throws ConfigError on invalid class", () => {
    process.env.SESH_ROLE = "worker";
    for (const bad of ["passive", "ACTIVE", "spy"]) {
      process.env.SESH_CLASS = bad;
      expect(() => readRoleClass()).toThrow(ConfigError);
    }
  });
});

describe("readAdapterConfig", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
  });

  test("composes NATS / agent / owner / session / role / class", () => {
    process.env.NATS_URL = "nats://example:4222";
    process.env.SESH_OWNER = "dmestas";
    process.env.SESH_SESSION = "rc-test";
    process.env.SESH_ROLE = "implementer";
    process.env.SESH_CLASS = "active";
    const c = readAdapterConfig({ defaultAgent: "claude-code" });
    expect(c.natsUrl).toBe("nats://example:4222");
    expect(c.agent).toBe("claude-code");
    expect(c.owner).toBe("dmestas");
    expect(c.session).toBe("rc-test");
    expect(c.role).toBe("implementer");
    expect(c.class).toBe("active");
  });

  test("SESH_AGENT env var overrides defaultAgent", () => {
    process.env.SESH_AGENT = "custom-bot";
    const c = readAdapterConfig({ defaultAgent: "claude-code" });
    expect(c.agent).toBe("custom-bot");
  });

  test("defaults NATS_URL, owner, session when env unset", () => {
    delete process.env.NATS_URL;
    delete process.env.SESH_OWNER;
    delete process.env.SESH_SESSION;
    delete process.env.USER;
    delete process.env.SESH_AGENT;
    const c = readAdapterConfig({ defaultAgent: "pi" });
    expect(c.natsUrl).toBe("nats://localhost:4222");
    expect(c.owner).toBe("anon");
    expect(c.session).toBe("");
    expect(c.agent).toBe("pi");
  });
});
