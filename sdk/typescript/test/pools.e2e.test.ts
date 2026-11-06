import { describe, expect, it } from "vitest";

import { PoolSpecConflictError, Vora } from "../src/index.js";

// Pools against a real vora-api, which needs a host that can run sandboxes. Skipped
// unless VORA_API_URL and VORA_API_KEY are set. VORA_E2E_IMAGE and VORA_E2E_BACKEND
// choose the pool's image and backend. VORA_E2E_UNSAFE=1 also runs the unsafe pool,
// which needs an admin key and a runner that grants it.
const live = Boolean(process.env.VORA_API_URL && process.env.VORA_API_KEY);
const image = process.env.VORA_E2E_IMAGE ?? "vora/base";
const backend = process.env.VORA_E2E_BACKEND ?? "bwrap";
const template = { image, backend, cpu: "500m", memory: "256Mi", pids: 64 };

function poolName(): string {
  return `sdk-e2e-${Math.random().toString(36).slice(2, 14)}`;
}

describe.skipIf(!live)("pools against a real API", () => {
  it("reuses a sandbox and proves the reset", { timeout: 120_000 }, async () => {
    const vora = new Vora();
    const name = poolName();
    const pool = await vora.pools.declare(name, { ...template, size: 1, maxSize: 1 });
    try {
      const first = await pool.run({
        command: "sh",
        args: ["-c", "echo first > /tmp/left; echo out"],
        timeoutSeconds: 30,
      });
      expect(first.stdout).toBe("out\n");
      expect(first.exit.exitCode).toBe(0);
      expect(first.exit.reason).toBe("EXITED");
      expect(first.exit.usage).toBeDefined();
      expect(first.done?.outcome).toBe("POOL_RESET_OK");

      const second = await pool.run({ command: "sh", args: ["-c", "cat /tmp/left || echo clean"], timeoutSeconds: 30 });
      expect(second.run.sandboxId).toBe(first.run.sandboxId);
      expect(second.run.sandboxRun).toBe(2);
      expect(second.stdout).toContain("clean");
      expect(second.run.resetMs).toBeDefined();

      const execs = await pool.execs(first.run.sandboxId ?? "");
      expect(execs.map((record) => record.sandboxRun)).toEqual([1, 2]);

      const same = await vora.pools.declare(name, { ...template, size: 1, maxSize: 1 });
      expect(same.id).toBe(pool.id);
      await expect(vora.pools.declare(name, { ...template, size: 1, maxSize: 2 })).rejects.toBeInstanceOf(
        PoolSpecConflictError,
      );
    } finally {
      await pool.delete();
    }
  });

  it("starts every run cold on a size-0 pool", { timeout: 120_000 }, async () => {
    const vora = new Vora();
    const pool = await vora.pools.declare(poolName(), { ...template, size: 0, maxSize: 1 });
    try {
      const result = await pool.run({ command: "true", timeoutSeconds: 30 });
      expect(result.run.warm).toBe(false);
      expect(result.run.startupMs).toBeGreaterThan(0);
      expect(result.exit.exitCode).toBe(0);
    } finally {
      await pool.delete();
    }
  });

  it.skipIf(!process.env.VORA_E2E_UNSAFE)("shows an unsafe pool in its results", { timeout: 120_000 }, async () => {
    const vora = new Vora();
    const pool = await vora.pools.declare(poolName(), { ...template, size: 1, maxSize: 1, reuse: "unsafe_no_reset" });
    try {
      const result = await pool.run({ command: "true", timeoutSeconds: 30 });
      expect(result.run.reuse).toBe("POOL_REUSE_UNSAFE_NO_RESET");
      expect(result.done?.outcome).toBe("POOL_RESET_SKIPPED");
    } finally {
      await pool.delete();
    }
  });
});
