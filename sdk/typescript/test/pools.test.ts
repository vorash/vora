import { afterEach, describe, expect, it } from "vitest";

import {
  ConflictError,
  GatewayTimeoutError,
  Pool,
  PoolDrainingError,
  PoolNotLoadedError,
  PoolNotOnRunnerError,
  PoolRunError,
  PoolSpecConflictError,
  RateLimitedError,
  RequestTooLargeError,
  UnavailableError,
  Vora,
  VoraError,
} from "../src/index.js";
import type { RetryPolicy, RunChunk } from "../src/index.js";
import { parseRetryAfter, Transport } from "../src/http.js";
import { decodePoolInfo, decodeSandboxExec, decodeSandboxInfo } from "../src/types.js";
import { answerReply, GOLDEN, runReply, startWire } from "./wire.js";
import type { Reply, Sent, WireServer } from "./wire.js";

const open: WireServer[] = [];

afterEach(async () => {
  await Promise.all(open.splice(0).map((server) => server.close()));
});

async function boot(route: (sent: Sent) => Reply, retry: RetryPolicy | null = null): Promise<{ vora: Vora; server: WireServer }> {
  const server = await startWire(route);
  open.push(server);
  return { vora: new Vora({ baseUrl: server.url, apiKey: "vora_test", retry }), server };
}

function handle(server: WireServer, retry: RetryPolicy | null = null, tenantId?: string): Pool {
  const transport = new Transport({ baseUrl: server.url, apiKey: "vora_test" });
  return new Pool(transport, retry, "py", decodePoolInfo({ id: "pool_test", spec: { name: "py" } }), tenantId);
}

function noWait(): { policy: RetryPolicy; waited: number[] } {
  const waited: number[] = [];
  return {
    waited,
    policy: {
      sleep: (ms) => {
        waited.push(ms);
        return Promise.resolve();
      },
    },
  };
}

function sequence(...replies: Reply[]): () => Reply {
  let next = 0;
  return () => {
    const reply = replies[next];
    next += 1;
    if (reply === undefined) {
      throw new Error("more requests than scripted");
    }
    return reply;
  };
}

const run = { timeoutSeconds: 10 };

async function caught<T>(call: () => Promise<unknown>, kind: new (...args: never[]) => T): Promise<T> {
  try {
    await call();
  } catch (error) {
    expect(error).toBeInstanceOf(kind);
    return error as T;
  }
  throw new Error(`expected ${kind.name}`);
}

describe("a run's result carries what the frames carry", () => {
  it("a warm run with a reset", async () => {
    const { server } = await boot(() => runReply("warm_reset"));

    const result = await handle(server).run({ command: "python3", args: ["-c", "print(6*7)"], ...run });

    expect(result).toEqual({
      stdout: "42\n",
      stderr: "warn\n",
      run: {
        poolId: "pool_01M6M3D3FRG49JM5BK3MD9Q0WY",
        sandboxId: "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ",
        executionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
        warm: true,
        startupMs: 12,
        resetMs: 7,
        sandboxRun: 3,
        reuse: "POOL_REUSE_RESET",
      },
      exit: {
        exitCode: 0,
        reason: "EXITED",
        executionId: "exe_01M6M3D3FRG49JM5BK3MD9Q0X0",
        usage: { cpuMillis: 21, memoryPeakBytes: 9437184, memoryPeakScope: "PEAK_SCOPE_WINDOW" },
      },
      done: { outcome: "POOL_RESET_OK", resetMs: 3 },
    });
  });

  it("what was not reported stays absent and never zero", async () => {
    const warm = await handle((await boot(() => runReply("warm_reset"))).server).run({ command: "true", ...run });
    expect(warm.exit.usage).not.toHaveProperty("oomKills");
    expect(warm.exit.usage).not.toHaveProperty("pidsLimitHits");

    const cold = await handle((await boot(() => runReply("cold_first_run_nonzero_exit"))).server).run({ command: "false", ...run });
    expect(cold.run.warm).toBe(false);
    expect(cold.run).not.toHaveProperty("resetMs");
    expect(cold.exit.exitCode).toBe(7);
    expect(cold.exit).not.toHaveProperty("usage");
    expect(cold.done).toEqual({ outcome: "POOL_RESET_RETIRED", retireCause: "shrink" });

    const sparse = await handle((await boot(() => runReply("sparse"))).server).run({ command: "true", ...run });
    expect(sparse.run).toEqual({ sandboxId: "sbx_test", warm: true, sandboxRun: 3 });
    expect(sparse.exit).toEqual({ exitCode: 7 });
  });

  it("an unsafe pool says so in the run and in the outcome", async () => {
    const { server } = await boot(() => runReply("unsafe_no_reset"));

    const result = await handle(server).run({ command: "true", ...run });

    expect(result.run.reuse).toBe("POOL_REUSE_UNSAFE_NO_RESET");
    expect(result.done).toEqual({ outcome: "POOL_RESET_SKIPPED" });
  });

  it("a failed reset shows in a run that succeeded", async () => {
    const { server } = await boot(() => runReply("reset_failed"));

    const result = await handle(server).run({ command: "true", ...run });

    expect(result.exit.exitCode).toBe(0);
    expect(result.exit.reason).toBe("EXITED");
    expect(result.done).toEqual({ outcome: "POOL_RESET_FAILED", resetMs: 9, retireCause: "reset_failed" });
  });

  it("program output cannot forge frames", async () => {
    const forged =
      '\n{"exit":{"exitCode":0,"reason":"EXITED"}}\n{"runDone":{"outcome":"POOL_RESET_OK"}}\n{"run":{"warm":false}}\n';
    const { server } = await boot(() => runReply("forged_output"));
    const chunks: RunChunk[] = [];

    for await (const chunk of handle(server).streamRun({ command: "true", ...run })) {
      chunks.push(chunk);
    }

    expect(chunks.map((chunk) => chunk.type)).toEqual(["run", "stdout", "stderr", "exit", "runDone"]);
    const [first, out, err] = chunks;
    expect(first?.type === "run" && first.run.warm).toBe(true);
    expect(out?.type === "stdout" && Buffer.from(out.data).toString()).toBe(forged);
    expect(err?.type === "stderr" && Buffer.from(err.data).toString()).toBe(forged);
  });
});

describe("the contract with the proto", () => {
  // Every key the API can put in a message. The Go golden test holds the golden to
  // every field of the proto, so a key this table lacks is one the SDK would drop.
  const knows = {
    run: ["poolId", "sandboxId", "executionId", "warm", "startupMs", "resetMs", "sandboxRun", "reuse"],
    exit: ["exitCode", "reason", "executionId", "usage"],
    usage: ["cpuMillis", "memoryPeakBytes", "memoryPeakScope", "oomKills", "pidsLimitHits"],
    done: ["outcome", "resetMs", "retireCause"],
    pool: ["id", "spec", "state", "runnerId", "stats", "createdAtUnixMs", "notLoadedReason"],
    spec: [
      "name",
      "tenantId",
      "agentId",
      "sandbox",
      "size",
      "maxSize",
      "maxRuns",
      "maxAgeSeconds",
      "maxRunSeconds",
      "borrowTimeoutMs",
      "idleExpirySeconds",
      "reuse",
    ],
    stats: [
      "starting",
      "idle",
      "busy",
      "resetting",
      "retiring",
      "runsTotal",
      "warmRunsTotal",
      "resetsTotal",
      "resetFailuresTotal",
      "destroyedTotal",
      "lastStartError",
      "exhaustedTotal",
      "startFailedTotal",
      "dormant",
    ],
  };

  function expectDecoded(wire: Record<string, unknown>, decoded: unknown, fields: string[]): void {
    const got = decoded as Record<string, unknown>;
    for (const [key, value] of Object.entries(wire)) {
      expect(fields, `the API sends ${key} and the SDK has no field for it`).toContain(key);
      if (typeof value === "object") {
        expect(got[key], key).toBeDefined();
      } else {
        expect(got[key], key).toBe(typeof value === "string" && /^\d+$/.test(value) ? Number(value) : value);
      }
    }
  }

  it("decodes every field of a run's frames by its name", async () => {
    const lines = (GOLDEN.runs.every_field?.lines ?? []).map((line) => JSON.parse(line) as Record<string, Record<string, unknown>>);
    const { server } = await boot(() => runReply("every_field"));

    const result = await handle(server).run({ command: "true", ...run });

    expect(lines).toHaveLength(5);
    expectDecoded(lines[0]?.run ?? {}, result.run, knows.run);
    expect(result.stdout).toBe("out\n");
    expect(result.stderr).toBe("warn\n");
    expectDecoded(lines[3]?.exit ?? {}, result.exit, knows.exit);
    expectDecoded((lines[3]?.exit?.usage as Record<string, unknown>) ?? {}, result.exit.usage, knows.usage);
    expectDecoded(lines[4]?.runDone ?? {}, result.done, knows.done);
  });

  it("decodes every field of a pool by its name", () => {
    const raw = GOLDEN.messages.pool ?? {};

    const info = decodePoolInfo(raw);

    expectDecoded(raw, info, knows.pool);
    expectDecoded(raw.spec as Record<string, unknown>, info.spec, knows.spec);
    expectDecoded(raw.stats as Record<string, unknown>, info.stats, knows.stats);
    expect(info.spec?.sandbox?.image).toBe("vora/python");
  });
});

describe("a failure after the first frame", () => {
  it.each([
    ["stalled_before_exit", "client_stalled"],
    ["runner_lost_before_exit", "runner_unavailable"],
    ["run_failed_before_exit", "run_failed"],
  ])("%s throws with what arrived", async (scenario, code) => {
    const { server } = await boot(() => runReply(scenario));

    const error = await caught(() => handle(server).run({ command: "true", ...run }), PoolRunError);

    expect(error.code).toBe(code);
    expect(error.stdout).toBe("partial\n");
    expect(error.run?.sandboxId).toBe("sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ");
    expect(error).toBeInstanceOf(VoraError);
  });

  it("is never delivered as a chunk", async () => {
    const { server } = await boot(() => runReply("stalled_before_exit"));
    const kinds: string[] = [];

    await caught(async () => {
      for await (const chunk of handle(server).streamRun({ command: "true", ...run })) {
        kinds.push(chunk.type);
      }
    }, PoolRunError);

    expect(kinds).toEqual(["run", "stdout"]);
  });

  it("after exit leaves the result standing with the reset unknown", async () => {
    const { server } = await boot(() => runReply("runner_lost_after_exit"));

    const result = await handle(server).run({ command: "true", ...run });

    expect(result.stdout).toBe("done\n");
    expect(result.exit.reason).toBe("EXITED");
    expect(result.done).toBeUndefined();
    expect(result.streamError?.code).toBe("runner_unavailable");
  });

  it("a clean end with no exit is not a success", async () => {
    const { server } = await boot(() => runReply("ended_before_exit"));

    const error = await caught(() => handle(server).run({ command: "true", ...run }), PoolRunError);

    expect(error.code).toBe("stream_ended");
    expect(error.stdout).toBe("partial\n");
  });

  it("a clean end after exit with no runDone is reported", async () => {
    const { server } = await boot(() => runReply("ended_after_exit_no_done"));

    const result = await handle(server).run({ command: "true", ...run });

    expect(result.done).toBeUndefined();
    expect(result.streamError?.code).toBe("stream_ended");
  });

  it("a cut connection before exit is a typed error", async () => {
    const { server } = await boot(() => runReply("warm_reset", { cutAfter: 2 }));

    const error = await caught(() => handle(server).run({ command: "true", ...run }), PoolRunError);

    expect(error.code).toBe("stream_ended");
    expect(error.stdout).toBe("42\n");
  });

  it("a cut connection after exit keeps the result", async () => {
    const { server } = await boot(() => runReply("warm_reset", { cutAfter: 4 }));

    const result = await handle(server).run({ command: "true", ...run });

    expect(result.exit.reason).toBe("EXITED");
    expect(result.done).toBeUndefined();
    expect(result.streamError?.code).toBe("stream_ended");
  });

  it("leaving the stream before exit disconnects, so the API cancels the run", async () => {
    const { server } = await boot(() => runReply("warm_reset", { cutAfter: 1, hold: true }));

    for await (const chunk of handle(server).streamRun("sleep 60", { timeoutSeconds: 60 })) {
      expect(chunk.type).toBe("run");
      break;
    }

    await server.clientLeft;
  });
});

describe("retries", () => {
  it("a run is not retried unless the caller asks", async () => {
    const { policy } = noWait();
    const { server } = await boot(() => runReply("pool_full"));

    const error = await caught(() => handle(server, policy).run({ command: "true", ...run }), RateLimitedError);

    expect(server.requests).toHaveLength(1);
    expect(error.status).toBe(429);
    expect(error.retryAfterSeconds).toBe(1);
  });

  it.each(["pool_full", "runner_loading"])("a run retries %s when asked, honouring Retry-After", async (refusal) => {
    const { policy, waited } = noWait();
    const { server } = await boot(sequence(runReply(refusal), runReply(refusal), runReply("warm_reset")));

    const result = await handle(server).run({ command: "true", ...run, retry: policy });

    expect(result.stdout).toBe("42\n");
    expect(server.requests).toHaveLength(3);
    expect(waited).toHaveLength(2);
    for (const ms of waited) {
      expect(ms).toBeGreaterThanOrEqual(1000);
      expect(ms).toBeLessThanOrEqual(1250);
    }
  });

  it("is bounded", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => runReply("runner_loading"));

    await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), UnavailableError);

    expect(server.requests).toHaveLength(4);
    expect(waited).toHaveLength(3);
  });

  it("a Retry-After past the cap throws instead of waiting", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => ({ status: 429, retryAfter: "3600", body: '{"error":"busy"}' }));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), RateLimitedError);

    expect(server.requests).toHaveLength(1);
    expect(waited).toEqual([]);
    expect(error.retryAfterSeconds).toBe(3600);
  });

  it("no Retry-After backs off within the bound", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => ({ status: 503, body: '{"error":"no runner available"}' }));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), UnavailableError);

    expect(error.retryAfterSeconds).toBeUndefined();
    expect(waited).toEqual([250, 500, 1000]);
  });

  it.each(["stalled_before_exit", "runner_lost_before_exit", "run_failed_before_exit", "ended_before_exit"])(
    "a run past its first frame is never repeated: %s",
    async (scenario) => {
      const { policy, waited } = noWait();
      const { server } = await boot(() => runReply(scenario));

      await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), PoolRunError);

      expect(server.requests).toHaveLength(1);
      expect(waited).toEqual([]);
    },
  );

  it("a cut connection past the first frame is never repeated", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => runReply("warm_reset", { cutAfter: 1 }));

    await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), PoolRunError);

    expect(server.requests).toHaveLength(1);
    expect(waited).toEqual([]);
  });

  it("a gateway timeout is typed and a run does not retry it", async () => {
    const { policy } = noWait();
    const { server } = await boot(() => runReply("runner_too_slow"));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), GatewayTimeoutError);

    expect(error.status).toBe(504);
    expect(server.requests).toHaveLength(1);
  });

  it("declare retries by default, as it is idempotent", async () => {
    const { policy, waited } = noWait();
    const { vora, server } = await boot(sequence(runReply("runner_loading"), answerReply("get_active")), policy);

    const pool = await vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi" });

    expect(pool.info.state).toBe("POOL_ACTIVE");
    expect(server.requests).toHaveLength(2);
    expect(waited).toHaveLength(1);
  });
});

describe("409s, told apart by the pool's state", () => {
  const runThenGet = (refusal: string, record: Reply) => (sent: Sent) => (sent.method === "POST" ? runReply(refusal) : record);

  it("a draining pool is typed and not retried", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(runThenGet("draining", answerReply("get_draining")));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), PoolDrainingError);

    expect(error.status).toBe(409);
    expect(server.requests.map((sent) => sent.method)).toEqual(["POST", "GET"]);
    expect(waited).toEqual([]);
  });

  it("a not-loaded pool names its reason and is not retried", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(runThenGet("not_loaded", answerReply("get_not_loaded")));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), PoolNotLoadedError);

    expect(error.reason).toBe("backend_missing");
    expect(server.requests.map((sent) => sent.method)).toEqual(["POST", "GET"]);
    expect(waited).toEqual([]);
  });

  it("declaring over a different spec is typed", async () => {
    const { vora } = await boot((sent) => answerReply(sent.method === "PUT" ? "declare_different_spec" : "get_active"));

    const error = await caught(() => vora.pools.declare("py", { image: "vora/other", cpu: "100m", memory: "1Mi" }), PoolSpecConflictError);

    expect(error.status).toBe(409);
    expect(error.message).toContain("different spec");
  });

  it("declaring over a draining pool is typed", async () => {
    const { vora } = await boot((sent) =>
      sent.method === "PUT" ? { status: 409, body: '{"error":"pool py is draining; retry once it is gone"}' } : answerReply("get_draining"),
    );

    await caught(() => vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi" }), PoolDrainingError);
  });

  it("a conflict the record cannot explain stays a plain conflict", async () => {
    const { vora } = await boot((sent) =>
      sent.method === "PUT"
        ? { status: 409, body: '{"error":"pool: unsafe reuse is not allowed on this runner"}' }
        : { status: 404, body: '{"error":"pool not found"}' },
    );

    const error = await caught(() => vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi", reuse: "unsafe_no_reset" }), ConflictError);

    expect(error.constructor).toBe(ConflictError);
    expect(error.message).toContain("unsafe reuse");
  });
});

describe("a pool its runner lost", () => {
  it("reads as its state with nothing measured", async () => {
    const { vora } = await boot(() => answerReply("get_not_on_runner"));

    const pool = await vora.pools.get("py");

    expect(pool.info.state).toBe("POOL_NOT_ON_RUNNER");
    expect(pool.info).not.toHaveProperty("stats");
    expect(await pool.stats()).toBeUndefined();
    expect(pool.info.spec?.name).toBe("py");
    expect(pool.info.spec).not.toHaveProperty("sandbox");
    expect(pool.info).not.toHaveProperty("createdAtUnixMs");
  });

  it("a run is typed and never retried", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot((sent) => answerReply(sent.method === "POST" ? "run_not_on_runner" : "get_not_on_runner"));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), PoolNotOnRunnerError);

    expect(error).toBeInstanceOf(ConflictError);
    expect(error.status).toBe(409);
    expect(error.message).toContain("declare it again");
    expect(server.requests.map((sent) => sent.method)).toEqual(["POST", "GET"]);
    expect(waited).toEqual([]);
  });

  it("redeclaring moves to the new id, and no id is cached", async () => {
    const reads = sequence(answerReply("get_not_on_runner"), answerReply("declare_lost_pool"));
    const { vora, server } = await boot((sent) => {
      if (sent.method === "PUT") return answerReply("declare_lost_pool");
      if (sent.method === "POST") return runReply("warm_reset");
      return reads();
    });

    const held = await vora.pools.get("py");
    const oldId = held.id;

    const declared = await vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi" });
    expect(declared.id).not.toBe(oldId);
    expect(declared.info.state).toBe("POOL_ACTIVE");

    await held.run({ command: "true", ...run });
    expect(await held.refresh()).toEqual(declared.info);
    expect(held.id).toBe(declared.id);

    for (const sent of server.requests) {
      expect(sent.url).not.toContain(oldId);
      expect(sent.url).not.toContain(declared.id);
    }
  });

  it("a conflict on declaring it stays a plain conflict", async () => {
    const { vora } = await boot((sent) =>
      sent.method === "PUT" ? { status: 409, body: '{"error":"pool id is taken"}' } : answerReply("get_not_on_runner"),
    );

    const error = await caught(() => vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi" }), ConflictError);

    expect(error.constructor).toBe(ConflictError);
  });

  it("deleting it needs one call", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => answerReply("delete_not_on_runner"));

    await handle(server, policy).delete();

    expect(server.requests).toHaveLength(1);
    expect(waited).toEqual([]);
  });

  it("stays listed with nothing measured", async () => {
    const { vora } = await boot(() => answerReply("listing_not_on_runner"));

    const pools = await vora.pools.list();

    expect(pools.map((pool) => pool.name)).toEqual(["py"]);
    expect(pools[0]?.info.state).toBe("POOL_NOT_ON_RUNNER");
    expect(pools[0]?.info).not.toHaveProperty("stats");
  });
});

describe("what declare refuses", () => {
  it("cpu and memory are required", async () => {
    const { vora } = await boot(() => answerReply("declare_no_resources"));

    // @ts-expect-error cpu and memory are required, so this cannot compile.
    const error = await caught(() => vora.pools.declare("py", { image: "vora/base" }), VoraError);

    expect(error.status).toBe(400);
    expect(error.message).toContain("cpu_millis and memory_bytes");
  });

  it("a borrow timeout past the cap is the API's 400", async () => {
    const { vora, server } = await boot(() => answerReply("declare_borrow_too_long"));

    const error = await caught(
      () =>
        vora.pools.declare("py", {
          image: "vora/base",
          cpu: "100m",
          memory: "1Mi",
          borrowTimeoutMs: (GOLDEN.limits.borrowTimeoutMaxMs ?? 0) + 1,
        }),
      VoraError,
    );

    expect(error.status).toBe(400);
    expect(error.message).toContain("borrow_timeout");
    expect(JSON.parse(server.requests[0]?.body.toString() ?? "")).toMatchObject({ borrowTimeoutMs: "300001" });
  });

  it("another agent's pool name is a plain 404 with no further calls", async () => {
    const { policy, waited } = noWait();
    const { vora, server } = await boot(() => answerReply("declare_other_agent"), policy);

    const error = await caught(() => vora.pools.declare("owned", { image: "vora/base", cpu: "100m", memory: "1Mi" }), VoraError);

    expect(error.constructor).toBe(VoraError);
    expect(error.status).toBe(404);
    expect(server.requests).toHaveLength(1);
    expect(waited).toEqual([]);
  });

  it("a refusal for now honours Retry-After, or says so with no policy", async () => {
    const { policy, waited } = noWait();
    const { vora, server } = await boot(sequence(answerReply("declare_runner_down"), answerReply("declare_lost_pool")), policy);

    const pool = await vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi" });

    expect(pool.info.state).toBe("POOL_ACTIVE");
    expect(server.requests).toHaveLength(2);
    expect(waited).toHaveLength(1);
    expect(waited[0]).toBeGreaterThanOrEqual(1000);

    const { vora: plain } = await boot(() => answerReply("declare_runner_down"));
    const error = await caught(() => plain.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi" }), UnavailableError);
    expect(error.retryAfterSeconds).toBe(1);
  });
});

describe("when Node's fetch stops waiting before the API answers", () => {
  // The shapes Node 26's fetch rejected with, against a local server silent for 320 s.
  const gaveUp = (message: string, code: string): TypeError =>
    Object.assign(new TypeError(message), { cause: Object.assign(new Error("timed out"), { code }) });
  const poolOver = (fetch: typeof globalThis.fetch): Pool =>
    new Pool(new Transport({ baseUrl: "http://vora.test", fetch }), null, "py", decodePoolInfo({ id: "pool_test" }), undefined);

  it("waiting for the first frame says so, and says nothing is known", async () => {
    const failure = gaveUp("fetch failed", "UND_ERR_HEADERS_TIMEOUT");
    const pool = poolOver(() => Promise.reject(failure));

    const error = await caught(() => pool.run({ command: "true", ...run }), VoraError);

    expect(error.message).toContain("headersTimeout");
    expect(error.message).toContain("not known whether anything ran");
    expect(error.cause).toBe(failure);
    expect(error.status).toBeUndefined();
  });

  it("between frames ends the run with a typed error that names the setting", async () => {
    const failure = gaveUp("terminated", "UND_ERR_BODY_TIMEOUT");
    const encoder = new TextEncoder();
    const lines = GOLDEN.runs.warm_reset?.lines ?? [];
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(`${lines[0] ?? ""}\n`));
      },
      pull(controller) {
        controller.error(failure);
      },
    });
    const pool = poolOver(() => Promise.resolve(new Response(body)));

    const error = await caught(() => pool.run({ command: "sleep 600", timeoutSeconds: 600 }), PoolRunError);

    expect(error.code).toBe("stream_ended");
    expect(error.message).toContain("bodyTimeout");
  });

  it("any other failure keeps its own words", async () => {
    const pool = poolOver(() => Promise.reject(new TypeError("fetch failed")));

    const error = await caught(() => pool.run({ command: "true", ...run }), VoraError);

    expect(error.message).toBe("pool run: fetch failed");
  });
});

describe("what a later consumer relies on", () => {
  it("an admin declares for a tenant, whose key then runs and reads each frame", async () => {
    const runs = sequence(runReply("unsafe_no_reset"), runReply("warm_reset"), runReply("cold_first_run_nonzero_exit"));
    const { vora: admin, server } = await boot((sent) => {
      if (sent.method === "PUT") return { status: 201, body: answerReply("get_active").body ?? "" };
      if (sent.method === "GET") return answerReply("get_active");
      return runs();
    });

    await admin.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi", tenantId: "tenant-a", reuse: "unsafe_no_reset" });
    expect(JSON.parse(server.requests[0]?.body.toString() ?? "")).toMatchObject({
      tenantId: "tenant-a",
      reuse: "POOL_REUSE_UNSAFE_NO_RESET",
    });

    const tenant = new Vora({ baseUrl: server.url, apiKey: "vora_tenant" });
    const pool = await tenant.pools.get("py");
    expect(server.requests.at(-1)?.url).toBe("/v1/pools/py");
    expect(server.requests.at(-1)?.headers.authorization).toBe("Bearer vora_tenant");

    const seen: unknown[] = [];
    for (let i = 0; i < 3; i += 1) {
      const frames: Partial<Record<RunChunk["type"], RunChunk>> = {};
      for await (const chunk of pool.streamRun({ command: "true", ...run })) {
        frames[chunk.type] = chunk;
      }
      const started = frames.run?.type === "run" ? frames.run.run : undefined;
      const exited = frames.exit?.type === "exit" ? frames.exit.exit : undefined;
      const done = frames.runDone?.type === "runDone" ? frames.runDone.done : undefined;
      seen.push([started?.warm, exited?.executionId, exited?.exitCode, started?.resetMs, done?.resetMs, done?.outcome]);
    }

    const executionId = "exe_01M6M3D3FRG49JM5BK3MD9Q0X0";
    expect(seen).toEqual([
      [true, executionId, 0, undefined, undefined, "POOL_RESET_SKIPPED"],
      [true, executionId, 0, 7, 3, "POOL_RESET_OK"],
      [false, executionId, 7, undefined, undefined, "POOL_RESET_RETIRED"],
    ]);
  });
});

describe("delete", () => {
  it("repeats through 202 until 204", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(sequence(answerReply("delete_draining"), answerReply("delete_draining"), answerReply("delete_gone")));

    await handle(server, policy).delete();

    expect(server.requests.map((sent) => sent.method)).toEqual(["DELETE", "DELETE", "DELETE"]);
    expect(waited).toHaveLength(2);
  });

  it("gives up after its wait with a typed error", async () => {
    const { policy } = noWait();
    const { server } = await boot(() => answerReply("delete_draining"));

    const error = await caught(() => handle(server, policy).delete({ maxWaitSeconds: 0 }), PoolDrainingError);

    expect(error.status).toBe(202);
    expect(server.requests).toHaveLength(1);
  });

  it("of a pool already gone succeeds", async () => {
    const { server } = await boot(() => ({ status: 404, body: '{"error":"pool not found"}' }));

    await handle(server).delete();
  });

  it("waits out a missing runner row, and then says so", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => answerReply("delete_runner_row_gone"));

    const error = await caught(() => handle(server, policy).delete({ maxWaitSeconds: 0 }), UnavailableError);

    expect(error.retryAfterSeconds).toBe(1);
    expect(server.requests).toHaveLength(1);

    const { server: back } = await boot(sequence(answerReply("delete_runner_row_gone"), answerReply("delete_gone")));
    await handle(back, policy).delete();
    expect(back.requests).toHaveLength(2);
    expect(waited).toEqual([1000]);
  });

  it("rides out a runner that is down", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(sequence(runReply("runner_loading"), runReply("runner_too_slow"), answerReply("delete_gone")));

    await handle(server, policy).delete();

    expect(server.requests).toHaveLength(3);
    expect(waited).toEqual([1000, 1000]);
  });
});

describe("what a request looks like", () => {
  it("declare encodes the spec the way protojson expects", async () => {
    const { vora, server } = await boot(() => ({ status: 201, body: answerReply("get_active").body ?? "" }));

    const pool = await vora.pools.declare("py", {
      image: "vora/python",
      backend: "bwrap",
      cpu: "1000m",
      memory: "256Mi",
      pids: 16,
      size: 0,
      maxSize: 4,
      maxRuns: 100,
      maxAgeSeconds: 1800,
      maxRunSeconds: 300,
      borrowTimeoutMs: 2000,
      idleExpirySeconds: 3600,
      reuse: "unsafe_no_reset",
    });

    const sent = server.requests[0];
    expect(sent?.method).toBe("PUT");
    expect(sent?.url).toBe("/v1/pools/py");
    expect(sent?.headers.authorization).toBe("Bearer vora_test");
    expect(JSON.parse(sent?.body.toString() ?? "")).toEqual({
      name: "py",
      sandbox: {
        image: "vora/python",
        backend: "bwrap",
        resources: { cpuMillis: "1000", memoryBytes: "268435456", pids: "16" },
        network: { mode: "NETWORK_NONE" },
      },
      size: 0,
      maxSize: 4,
      maxRuns: 100,
      maxAgeSeconds: "1800",
      maxRunSeconds: "300",
      borrowTimeoutMs: "2000",
      idleExpirySeconds: "3600",
      reuse: "POOL_REUSE_UNSAFE_NO_RESET",
    });
    expect(pool.id).toBe("pool_01M6M3D3FRG49JM5BK3MD9Q0WY");
  });

  it("a run encodes stdin and the timeout", async () => {
    const { server } = await boot(() => runReply("warm_reset"));

    await handle(server).run({ command: "cat", stdin: "hello", cwd: "/tmp", env: ["A=1"], ...run });

    expect(JSON.parse(server.requests[0]?.body.toString() ?? "")).toEqual({
      command: "cat",
      cwd: "/tmp",
      env: ["A=1"],
      timeoutSeconds: "10",
      stdin: Buffer.from("hello").toString("base64"),
    });
  });

  it("no first frame is typed, and a run does not retry it", async () => {
    const { policy, waited } = noWait();
    const { server } = await boot(() => runReply("no_first_frame"));

    const error = await caught(() => handle(server).run({ command: "true", ...run, retry: policy }), GatewayTimeoutError);

    expect(error.status).toBe(504);
    expect(error.retryAfterSeconds).toBe(1);
    expect(server.requests).toHaveLength(1);
    expect(waited).toEqual([]);
  });

  it("a bare string is a shell line, as in exec", async () => {
    const { server } = await boot(() => runReply("warm_reset"));

    await handle(server).run("ls | wc -l", run);

    expect((JSON.parse(server.requests[0]?.body.toString() ?? "") as { args: string[] }).args).toEqual(["-c", "ls | wc -l"]);
  });

  it("a run has to name its timeout", async () => {
    const { server } = await boot(() => runReply("warm_reset"));

    await caught(async () => handle(server).run({ command: "true" } as never), VoraError);

    expect(server.requests).toEqual([]);
  });

  it("a global admin names the tenant on every route", async () => {
    const { vora, server } = await boot((sent) => {
      if (sent.method === "POST") return runReply("warm_reset");
      if (sent.method === "DELETE") return answerReply("delete_gone");
      if (sent.method === "PUT") return { status: 201, body: answerReply("get_active").body ?? "" };
      if (sent.url.startsWith("/v1/pools/py/sandboxes")) return { status: 200, body: '{"execs":[],"events":[]}' };
      if (sent.url.startsWith("/v1/pools?")) return { status: 200, body: '{"pools":[]}' };
      return answerReply("get_active");
    });

    const pool = await vora.pools.declare("py", { image: "vora/base", cpu: "100m", memory: "1Mi", tenantId: "tenant-a" });
    await vora.pools.get("py", { tenantId: "tenant-a" });
    await vora.pools.list({ tenantId: "tenant-a" });
    await pool.stats();
    await pool.run({ command: "true", timeoutSeconds: 5 });
    await pool.execs("sbx_1");
    await pool.events("sbx_1");
    await pool.delete();

    const [declare, ...rest] = server.requests;
    const spec = JSON.parse(declare?.body.toString() ?? "") as { tenantId: string; sandbox: Record<string, unknown> };
    expect(spec.tenantId).toBe("tenant-a");
    expect(spec.sandbox).not.toHaveProperty("tenantId");
    expect(rest).toHaveLength(7);
    for (const sent of rest) {
      expect(sent.url).toContain("tenant_id=tenant-a");
    }
  });

  it("a body over the API's limit is refused before it is sent", async () => {
    const { server } = await boot(() => runReply("warm_reset"));

    await caught(() => handle(server).run({ command: "cat", stdin: new Uint8Array(1 << 20), ...run }), RequestTooLargeError);

    expect(server.requests).toEqual([]);
  });

  it("the most stdin the limit allows is sent", async () => {
    const { server } = await boot(() => runReply("warm_reset"));
    const overhead = JSON.stringify({ command: "cat", timeoutSeconds: "10", stdin: "" }).length;
    const fits = Math.floor(((1 << 20) - overhead) / 4) * 3;

    await handle(server).run({ command: "cat", stdin: new Uint8Array(fits), ...run });

    expect(server.requests).toHaveLength(1);
    expect(server.requests[0]?.body.length).toBeLessThanOrEqual(1 << 20);
  });
});

describe("listings", () => {
  it("a pool whose runner did not answer has no state and no counters", async () => {
    const { vora } = await boot(() => answerReply("listing_runner_silent"));

    const pools = await vora.pools.list();

    expect(pools).toHaveLength(1);
    expect(pools[0]?.name).toBe("py");
    expect(pools[0]?.info).not.toHaveProperty("state");
    expect(pools[0]?.info).not.toHaveProperty("stats");
  });

  it("an unanswered entry still says whether it resets", async () => {
    const { vora } = await boot(() => answerReply("listing_unsafe_unanswered"));

    const pools = await vora.pools.list();
    const unsafe = pools.find((pool) => pool.name === "unsafe")?.info;

    expect(unsafe).not.toHaveProperty("state");
    expect(unsafe).not.toHaveProperty("stats");
    expect(unsafe?.spec?.reuse).toBe("POOL_REUSE_UNSAFE_NO_RESET");
    expect(pools.find((pool) => pool.name === "py")?.info.spec).not.toHaveProperty("reuse");
  });

  it("a handle from a listing of every tenant reaches its own tenant", async () => {
    const { vora, server } = await boot((sent) => answerReply(sent.url === "/v1/pools" ? "listing_runner_silent" : "get_active"));

    const [pool] = await vora.pools.list();
    await pool?.refresh();

    expect(server.requests.at(-1)?.url).toBe("/v1/pools/py?tenant_id=tenant-a");
  });

  it("a not-loaded pool has no stats, though the wire sends an empty one", async () => {
    const { vora } = await boot(() => answerReply("get_not_loaded"));
    expect(JSON.parse(GOLDEN.answers.get_not_loaded?.body ?? "")).toMatchObject({ stats: {} });

    const pool = await vora.pools.get("py");

    expect(pool.info.state).toBe("POOL_NOT_LOADED");
    expect(pool.info.notLoadedReason).toBe("backend_missing");
    expect(pool.info).not.toHaveProperty("stats");
    expect(await pool.stats()).toBeUndefined();
  });

  it("counters a runner reported read as numbers", async () => {
    const { vora } = await boot(() => answerReply("get_active"));

    const stats = await (await vora.pools.get("py")).stats();

    expect(stats?.idle).toBe(1);
    expect(stats?.busy).toBe(0);
    expect(stats?.dormant).toBe(false);
    expect(stats).not.toHaveProperty("lastStartError");
  });

  it("a global admin without a tenant gets the API's answer", async () => {
    const { vora } = await boot(() => answerReply("global_admin_without_tenant"));

    const error = await caught(() => vora.pools.get("py"), VoraError);

    expect(error.status).toBe(400);
    expect(error.message).toContain("must pass tenant_id");
  });
});

describe("typed errors keep what callers already rely on", () => {
  it.each([
    [429, RateLimitedError],
    [503, UnavailableError],
    [504, GatewayTimeoutError],
    [409, ConflictError],
  ] as const)("a plain route's %i", async (status, kind) => {
    const { vora } = await boot(() => ({ status, body: '{"error":"refused"}' }));

    const error = await caught(() => vora.sandboxes.get("sbx_1"), kind as new (...args: never[]) => VoraError);

    expect(error).toBeInstanceOf(VoraError);
    expect(error.status).toBe(status);
    expect(error.body).toEqual({ error: "refused" });
    expect(error.message).toBe("refused");
  });

  it("an audit record and a sandbox name their pool", () => {
    expect(decodeSandboxExec({ poolId: "pool_1", sandboxRun: 2 })).toEqual({ poolId: "pool_1", sandboxRun: 2 });
    expect(decodeSandboxInfo({ id: "sbx_1", poolId: "pool_1" }).poolId).toBe("pool_1");
    expect(decodeSandboxInfo({ id: "sbx_1" })).not.toHaveProperty("poolId");
  });

  it("understands a Retry-After date from a proxy", () => {
    expect(parseRetryAfter("2")).toBe(2);
    expect(parseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT")).toBe(0);
    expect(parseRetryAfter("soon")).toBeUndefined();
    expect(parseRetryAfter(null)).toBeUndefined();
  });
});
