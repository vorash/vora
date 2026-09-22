import { RunContext } from "@openai/agents";
import { Vora } from "@vorash/sdk";
import { afterEach, describe, expect, it } from "vitest";

import { RUN_CODE_TOOL_NAME, VoraSandboxClient } from "../src/index.js";
import { b64, execChunks, startAPI } from "./helpers.js";
import type { FakeAPI, FakeOptions } from "./helpers.js";

const open: FakeAPI[] = [];

afterEach(async () => {
  await Promise.all(open.splice(0).map((api) => api.close()));
});

async function boot(options?: FakeOptions): Promise<FakeAPI> {
  const api = await startAPI(options);
  open.push(api);
  return api;
}

function clientFor(api: FakeAPI, overrides: Partial<ConstructorParameters<typeof VoraSandboxClient>[0]> = {}): VoraSandboxClient {
  return new VoraSandboxClient({ client: new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" }), ...overrides });
}

function execBody(api: FakeAPI): Record<string, unknown> {
  const request = api.requests.find((candidate) => candidate.url.endsWith("/exec"));
  return JSON.parse(request?.body ?? "{}") as Record<string, unknown>;
}

describe("runCode", () => {
  it("runs python through the sandbox and returns its output", async () => {
    const api = await boot({ exec: { status: 200, ndjson: execChunks("42\n") } });
    const client = clientFor(api);

    const result = await client.runCode({ code: "print(6*7)" });

    expect(result.stdout).toBe("42\n");
    expect(result.exitCode).toBe(0);
    expect(result.output).toContain("exit 0");
    expect(result.output).toContain("42");
    expect(execBody(api)).toEqual({
      command: "python3",
      stdin: b64("print(6*7)"),
      cwd: "/workspace",
    });
  });

  it("maps each language to its interpreter", async () => {
    const api = await boot();
    const client = clientFor(api);

    await client.runCode({ code: "console.log(1)", language: "javascript" });
    expect(execBody(api).command).toBe("node");

    await client.runCode({ code: "echo hi", language: "shell" });
    const shellExec = api.requests.filter((request) => request.url.endsWith("/exec")).at(-1);
    expect((JSON.parse(shellExec?.body ?? "{}") as Record<string, unknown>).command).toBe("sh");

    await client.runCode({ code: "print(1)", language: "python" });
    const pythonExec = api.requests.filter((request) => request.url.endsWith("/exec")).at(-1);
    expect((JSON.parse(pythonExec?.body ?? "{}") as Record<string, unknown>).command).toBe("python3");
  });

  it("reports stderr and a failing exit code", async () => {
    const api = await boot({ exec: { status: 200, ndjson: execChunks("", "boom\n", 2) } });
    const client = clientFor(api);

    const result = await client.runCode({ code: "raise SystemExit(2)" });

    expect(result.exitCode).toBe(2);
    expect(result.stderr).toBe("boom\n");
    expect(result.output).toContain("boom");
  });

  it("passes the working directory and timeout through", async () => {
    const api = await boot();
    const client = clientFor(api, { workdir: "/tmp", image: "vora/base" });

    await client.runCode({ code: "pwd", language: "shell", timeoutSeconds: 15 });

    expect(execBody(api)).toMatchObject({ cwd: "/tmp", timeoutSeconds: "15" });
  });
});

describe("sandbox lifecycle", () => {
  it("creates one sandbox and reuses it across calls", async () => {
    const api = await boot();
    const client = clientFor(api, { image: "vora/python", memory: "256Mi", network: "none" });

    await client.runCode({ code: "a" });
    await client.runCode({ code: "b" });

    const creates = api.requests.filter((request) => request.method === "POST" && request.url === "/v1/sandboxes");
    expect(creates).toHaveLength(1);
    expect(JSON.parse(creates[0]?.body ?? "{}")).toEqual({
      image: "vora/python",
      resources: { memoryBytes: "268435456" },
      network: { mode: "NETWORK_NONE" },
    });
    expect(client.sandbox?.id).toBe("sbx_agent");
  });

  it("shares one creation between concurrent callers", async () => {
    const api = await boot();
    const client = clientFor(api);

    await Promise.all([client.start(), client.start(), client.start()]);

    expect(api.requests.filter((request) => request.method === "POST" && request.url === "/v1/sandboxes")).toHaveLength(1);
  });

  it("destroys an owned sandbox and is idempotent", async () => {
    const api = await boot();
    const client = clientFor(api);

    await client.runCode({ code: "a" });
    await client.dispose();
    await client.dispose();

    const deletes = api.requests.filter((request) => request.method === "DELETE");
    expect(deletes).toHaveLength(1);
    expect(deletes[0]?.url).toBe("/v1/sandboxes/sbx_agent");
    expect(client.sandbox).toBeUndefined();
  });

  it("adopts an existing sandbox without creating or destroying it", async () => {
    const api = await boot();
    const client = clientFor(api, { sandboxId: "sbx_existing" });

    await client.runCode({ code: "a" });
    await client.dispose();

    expect(api.requests.filter((request) => request.method === "POST" && request.url === "/v1/sandboxes")).toHaveLength(0);
    expect(api.requests.filter((request) => request.method === "DELETE")).toHaveLength(0);
    expect(api.requests.some((request) => request.url === "/v1/sandboxes/sbx_existing")).toBe(true);
  });
});

describe("tool", () => {
  it("exposes run_code with a description and parameter schema", () => {
    const client = new VoraSandboxClient({ client: new Vora({ baseUrl: "http://127.0.0.1:1" }) });
    const tool = client.tool();

    expect(tool.name).toBe(RUN_CODE_TOOL_NAME);
    expect(tool.description).toMatch(/sandbox/i);
    expect(tool.type).toBe("function");
    const properties = (tool.parameters as { properties?: Record<string, unknown> }).properties ?? {};
    expect(Object.keys(properties)).toEqual(expect.arrayContaining(["code", "language", "timeout_seconds"]));
  });

  it("runs code when the model invokes it", async () => {
    const api = await boot({ exec: { status: 200, ndjson: execChunks("hello\n") } });
    const client = clientFor(api);

    const output = await client.tool().invoke(new RunContext(), JSON.stringify({ code: "print('hello')" }));

    expect(output).toContain("hello");
    expect(output).toContain("exit 0");
    expect(execBody(api).command).toBe("python3");
  });

  it("returns a readable error instead of throwing", async () => {
    const api = await boot({ create: { status: 503, json: { error: "no runner available" } } });
    const client = clientFor(api);

    const output = await client.tool().invoke(new RunContext(), JSON.stringify({ code: "print(1)" }));

    expect(String(output)).toContain("error:");
    expect(String(output)).toContain("no runner available");
  });

  it("surfaces exec failures to direct callers", async () => {
    const api = await boot({ exec: { status: 500, json: { error: "runtime failure" } } });
    const client = clientFor(api);

    await expect(client.runCode({ code: "print(1)" })).rejects.toThrow(/runtime failure/);
  });
});