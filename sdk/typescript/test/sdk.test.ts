import { afterEach, describe, expect, it } from "vitest";

import { Vora, VoraError } from "../src/index.js";
import type { ExecChunk } from "../src/index.js";
import { b64, lastRequest, sandboxJSON, startAPI } from "./helpers.js";
import type { FakeAPI, RecordedRequest } from "./helpers.js";

const open: FakeAPI[] = [];

afterEach(async () => {
  await Promise.all(open.splice(0).map((api) => api.close()));
});

async function boot(handler: Parameters<typeof startAPI>[0]): Promise<FakeAPI> {
  const api = await startAPI(handler);
  open.push(api);
  return api;
}

function body(request: RecordedRequest | undefined): Record<string, unknown> {
  return JSON.parse(request?.body.toString("utf8") ?? "{}") as Record<string, unknown>;
}

describe("sandboxes.create", () => {
  it("encodes the create request the way protojson expects", async () => {
    const api = await boot(() => ({ status: 201, json: sandboxJSON() }));
    const vora = new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" });

    const sandbox = await vora.sandboxes.create({
      image: "vora/python",
      cpu: 1,
      memory: "512Mi",
      disk: "2Gi",
      pids: 128,
      network: "none",
      env: { A: "B" },
      ttl: "1h",
    });

    const request = lastRequest(api, "POST");
    expect(request?.url).toBe("/v1/sandboxes");
    expect(request?.headers.authorization).toBe("Bearer vora_test");
    expect(body(request)).toEqual({
      image: "vora/python",
      resources: { cpuMillis: "1000", memoryBytes: "536870912", diskBytes: "2147483648", pids: "128" },
      network: { mode: "NETWORK_NONE" },
      env: ["A=B"],
      ttlSeconds: "3600",
    });

    // protojson writes int64 as strings; the SDK surfaces them as numbers.
    expect(sandbox.id).toBe("sbx_01M2NVHE1KRD8JT3D6QG6R0EW3");
    expect(sandbox.info.state).toBe("RUNNING");
    expect(sandbox.info.createdAtUnixMs).toBe(1758000000000);
    expect(sandbox.info.spec?.resources?.memoryBytes).toBe(536870912);
    expect(sandbox.info.spec?.resources?.cpuMillis).toBe(1000);
    expect(sandbox.info.spec?.ttlSeconds).toBe(3600);
    expect(sandbox.info.spec?.network?.mode).toBe("NETWORK_NONE");
  });

  it("accepts a full network policy and an env array", async () => {
    const api = await boot(() => ({ status: 201, json: sandboxJSON() }));
    const vora = new Vora({ baseUrl: api.baseUrl });

    await vora.sandboxes.create({
      image: "vora/base",
      network: { mode: "restricted", allowedDomains: ["github.com"], brokerAliases: ["github"] },
      env: ["LOG=debug"],
    });

    expect(body(lastRequest(api, "POST"))).toEqual({
      image: "vora/base",
      network: { mode: "NETWORK_RESTRICTED", allowedDomains: ["github.com"], brokerAliases: ["github"] },
      env: ["LOG=debug"],
    });
    expect(lastRequest(api, "POST")?.headers.authorization).toBeUndefined();
  });

  it("requires an image", async () => {
    const vora = new Vora({ baseUrl: "http://127.0.0.1:1" });
    await expect(vora.sandboxes.create({ image: "" })).rejects.toThrow(VoraError);
  });
});

describe("exec", () => {
  it("streams stdout, stderr and the exit code", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/exec")
        ? {
            ndjson: [
              { stdout: b64("hello ") },
              { stderr: b64("warn\n") },
              { stdout: b64("world\n") },
              { exit: { exitCode: 3, reason: "EXITED" } },
            ],
          }
        : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const result = await sandbox.exec("python main.py");

    expect(result.stdout).toBe("hello world\n");
    expect(result.stderr).toBe("warn\n");
    expect(result.exitCode).toBe(3);
    expect(result.reason).toBe("EXITED");
    expect(body(lastRequest(api, "POST"))).toEqual({
      command: "sh",
      args: ["-c", "python main.py"],
    });
  });

  it("exposes the chunks in order", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/exec") ? { ndjson: [{ stdout: b64("a") }, { exit: { exitCode: 0 } }] } : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const seen: ExecChunk[] = [];
    for await (const chunk of sandbox.streamExec({ command: "true" })) {
      seen.push(chunk);
    }

    expect(seen).toHaveLength(2);
    expect(seen[0]).toMatchObject({ type: "stdout" });
    expect(seen[1]).toEqual({ type: "exit", exitCode: 0 });
  });

  it("passes stdin, cwd, env and timeout through", async () => {
    const api = await boot((request) => (request.url.endsWith("/exec") ? { ndjson: [{ exit: { exitCode: 0 } }] } : undefined));
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    await sandbox.exec({ command: "cat", cwd: "/workspace", env: ["A=B"], stdin: "piped", timeoutSeconds: 5 });

    expect(body(lastRequest(api, "POST"))).toEqual({
      command: "cat",
      cwd: "/workspace",
      env: ["A=B"],
      stdin: b64("piped"),
      timeoutSeconds: "5",
    });
  });

  it("surfaces a non-zero exit without throwing", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/exec") ? { ndjson: [{ exit: { exitCode: 7, reason: "TIMEOUT_EXCEEDED" } }] } : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const result = await sandbox.exec("sleep 100");
    expect(result.exitCode).toBe(7);
    expect(result.reason).toBe("TIMEOUT_EXCEEDED");
  });
});

describe("files", () => {
  it("writes a file and reports the byte count", async () => {
    const api = await boot((request) =>
      request.method === "PUT" ? { json: { path: "/workspace/a.txt", bytes_written: 5 } } : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const written = await sandbox.writeFile("/workspace/a.txt", "hello");

    expect(written).toBe(5);
    const request = lastRequest(api, "PUT");
    expect(request?.url).toBe("/v1/sandboxes/sbx_x/files?path=%2Fworkspace%2Fa.txt&create_parents=true");
    expect(request?.headers["content-type"]).toBe("application/octet-stream");
    expect(request?.body.toString("utf8")).toBe("hello");
  });

  it("reads a file back as bytes and text", async () => {
    const api = await boot((request) =>
      request.url.includes("/files") ? { bytes: Buffer.from("content", "utf8") } : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    expect(await sandbox.readText("/workspace/a.txt")).toBe("content");
    expect(lastRequest(api, "GET")?.url).toBe("/v1/sandboxes/sbx_x/files?path=%2Fworkspace%2Fa.txt");
  });
});

describe("observability", () => {
  it("parses stats written as protojson strings", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/stats")
        ? { json: { memoryBytes: "1234", memoryPeakBytes: "2345", cpuMillis: "99", pids: "3", oomKills: "1" } }
        : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    expect(await sandbox.stats()).toEqual({
      memoryBytes: 1234,
      memoryPeakBytes: 2345,
      cpuMillis: 99,
      pids: 3,
      oomKills: 1,
    });
  });

  it("parses lifecycle events", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/events")
        ? {
            json: {
              events: [
                { sandboxId: "sbx_x", from: "CREATING", to: "RUNNING", reason: "", runnerId: "r1", timestampUnixMs: "1758000000000" },
              ],
            },
          }
        : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const events = await sandbox.events();
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ from: "CREATING", to: "RUNNING", timestampUnixMs: 1758000000000 });
  });

  it("parses exec records", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/execs")
        ? { json: { execs: [{ executionId: "exec_1", command: "id", exitCode: 0, cpuMillis: "12", egressDenied: "2" }] } }
        : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const execs = await sandbox.execs();
    expect(execs[0]).toMatchObject({ executionId: "exec_1", command: "id", exitCode: 0, cpuMillis: 12, egressDenied: 2 });
  });

  it("parses egress decisions", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/egress")
        ? {
            json: {
              records: [{ sandboxId: "sbx_x", host: "github.com", remote: "140.82.121.4:443", allowed: true, timestampUnixMs: "1" }],
            },
          }
        : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    const records = await sandbox.egress();
    expect(records[0]).toMatchObject({ host: "github.com", allowed: true, timestampUnixMs: 1 });
  });
});

describe("lifecycle", () => {
  it("lists sandboxes, optionally including destroyed ones", async () => {
    const api = await boot((request) =>
      request.url.startsWith("/v1/sandboxes?")
        ? { json: { sandboxes: [sandboxJSON(), sandboxJSON({ id: "sbx_y", state: "DESTROYED" })] } }
        : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });

    const all = await vora.sandboxes.list({ includeDestroyed: true });

    expect(api.requests[0]?.url).toBe("/v1/sandboxes?include_destroyed=true");
    expect(all.map((sandbox) => sandbox.id)).toEqual(["sbx_01M2NVHE1KRD8JT3D6QG6R0EW3", "sbx_y"]);
  });

  it("destroys a sandbox", async () => {
    const api = await boot((request) => (request.method === "DELETE" ? { status: 204, text: "" } : undefined));
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    await sandbox.destroy();

    const request = lastRequest(api, "DELETE");
    expect(request?.url).toBe("/v1/sandboxes/sbx_x");
  });
});

describe("errors", () => {
  it("turns an API error body into a VoraError", async () => {
    const api = await boot((request) =>
      request.url === "/v1/sandboxes/sbx_missing" ? { status: 404, json: { error: "sandbox sbx_missing not found" } } : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });

    await expect(vora.sandboxes.get("sbx_missing")).rejects.toMatchObject({
      name: "VoraError",
      message: "sandbox sbx_missing not found",
      status: 404,
    });
  });

  it("reports malformed streams", async () => {
    const api = await boot((request) =>
      request.url.endsWith("/exec") ? { text: "not json\n", contentType: "application/x-ndjson" } : undefined,
    );
    const vora = new Vora({ baseUrl: api.baseUrl });
    const sandbox = await vora.sandboxes.get("sbx_x");

    await expect(sandbox.exec("true")).rejects.toThrow(/malformed stream line/);
  });
});

describe("environment defaults", () => {
  it("reads VORA_API_URL and VORA_API_KEY", async () => {
    const api = await boot(() => undefined);
    const previousUrl = process.env.VORA_API_URL;
    const previousKey = process.env.VORA_API_KEY;
    process.env.VORA_API_URL = api.baseUrl;
    process.env.VORA_API_KEY = "vora_from_env";
    try {
      await new Vora().sandboxes.get("sbx_x");
    } finally {
      process.env.VORA_API_URL = previousUrl;
      process.env.VORA_API_KEY = previousKey;
    }

    expect(api.requests[0]?.headers.authorization).toBe("Bearer vora_from_env");
  });
});

describe("exec", () => {
  it("fails loudly when an exec stream ends without an exit record", async () => {
    // A killed run (timeout, OOM, agent death) closes the stream with no exit
    // chunk. Reporting that as exit 0 would make every gate fail open.
    const api = await boot((request) => {
      if (request.url?.includes("/exec") === true) {
        return {
          contentType: "application/x-ndjson",
          ndjson: [{ stdout: b64("partial output") }],
        };
      }
      return undefined;
    });
    const vora = new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" });
    const sandbox = await vora.sandboxes.get("sbx_01TEST");

    await expect(sandbox.exec("sh -c 'while :; do :; done'")).rejects.toMatchObject({
      name: "VoraError",
      message: expect.stringContaining("without an exit record"),
      body: { stdout: "partial output" },
    });
  });
});
