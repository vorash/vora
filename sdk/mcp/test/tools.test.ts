import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { Vora } from "@vorash/sdk";
import { afterEach, describe, expect, it } from "vitest";

import { createServer } from "../src/server.js";
import { startAPI } from "./helpers.js";
import type { FakeAPI } from "./helpers.js";

const open: FakeAPI[] = [];

afterEach(async () => {
  await Promise.all(open.splice(0).map((api) => api.close()));
});

interface Harness {
  client: Client;
  api: FakeAPI;
  close(): Promise<void>;
}

/** Wire a real MCP client to the server over an in-memory transport. */
async function connect(): Promise<Harness> {
  const api = await startAPI();
  open.push(api);
  const server = createServer(new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" }), { version: "0.0.0-test" });
  const client = new Client({ name: "test-client", version: "0.0.0" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  await server.connect(serverTransport);
  await client.connect(clientTransport);
  return {
    client,
    api,
    close: async () => {
      await client.close();
      await server.close();
    },
  };
}

// callTool returns a union that also covers task-based results, which carry no
// `content`; narrow to the plain content result the tools here always return.
type ToolReply = Awaited<ReturnType<Client["callTool"]>>;

function textOf(result: ToolReply): string {
  if (!("content" in result) || !Array.isArray(result.content)) {
    throw new Error(`expected a content result, got ${JSON.stringify(result)}`);
  }
  const first = result.content[0];
  if (first === undefined || first.type !== "text") {
    throw new Error(`expected a text result, got ${JSON.stringify(result.content)}`);
  }
  return first.text;
}

describe("tool registration", () => {
  it("exposes exactly the master-plan tool set", async () => {
    const harness = await connect();
    const { tools } = await harness.client.listTools();
    expect(tools.map((tool) => tool.name).sort()).toEqual([
      "sandbox_create",
      "sandbox_destroy",
      "sandbox_exec",
      "sandbox_read_file",
      "sandbox_stats",
      "sandbox_write_file",
    ]);
    await harness.close();
  });

  it("publishes an input schema for every tool", async () => {
    const harness = await connect();
    const { tools } = await harness.client.listTools();
    for (const tool of tools) {
      expect(tool.inputSchema, `${tool.name} has no schema`).toBeDefined();
      expect(tool.description, `${tool.name} has no description`).toBeTruthy();
    }
    await harness.close();
  });
});

describe("sandbox_create", () => {
  it("creates a sandbox and returns its id", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({
      name: "sandbox_create",
      arguments: { image: "vora/python", memory: "512Mi", cpu: 1, network: "none", ttl: "1h" },
    });

    expect(result.isError).toBeFalsy();
    expect(textOf(result)).toContain("sbx_test");

    const create = harness.api.requests.find((request) => request.method === "POST" && request.url === "/v1/sandboxes");
    expect(JSON.parse(create?.body ?? "{}")).toEqual({
      image: "vora/python",
      resources: { cpuMillis: "1000", memoryBytes: "536870912", diskBytes: "1073741824", pids: "64" },
      network: { mode: "NETWORK_NONE" },
      ttlSeconds: "3600",
    });
    await harness.close();
  });

  it("does not let model-selected full networking reach the API by default", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({ name: "sandbox_create", arguments: { image: "vora/python", network: "full" } });
    expect(result.isError).toBe(true);
    expect(harness.api.requests.some((request) => request.method === "POST" && request.url === "/v1/sandboxes")).toBe(false);
    await harness.close();
  });

  it("reports an API failure as a tool error", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({ name: "sandbox_create", arguments: { image: "" } });
    expect(result.isError).toBe(true);
    await harness.close();
  });
});

describe("sandbox_exec", () => {
  it("returns stdout and the exit code", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({
      name: "sandbox_exec",
      arguments: { sandbox_id: "sbx_test", command: "echo hi" },
    });

    expect(result.isError).toBeFalsy();
    const text = textOf(result);
    expect(text).toContain("exit 0");
    expect(text).toContain("hello from sandbox");

    const exec = harness.api.requests.find((request) => request.url.endsWith("/exec"));
    expect(JSON.parse(exec?.body ?? "{}")).toEqual({ command: "sh", args: ["-c", "echo hi"] });
    await harness.close();
  });

  it("surfaces a missing sandbox as a tool error", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({
      name: "sandbox_exec",
      arguments: { sandbox_id: "sbx_missing", command: "echo hi" },
    });

    expect(result.isError).toBe(true);
    expect(textOf(result)).toContain("sandbox sbx_missing not found");
    await harness.close();
  });

  it("passes stdin and timeout through", async () => {
    const harness = await connect();
    await harness.client.callTool({
      name: "sandbox_exec",
      arguments: { sandbox_id: "sbx_test", command: "wc -c", stdin: "hello", timeout_seconds: 5 },
    });

    const exec = harness.api.requests.find((request) => request.url.endsWith("/exec"));
    expect(JSON.parse(exec?.body ?? "{}")).toEqual({
      command: "sh",
      args: ["-c", "wc -c"],
      stdin: "aGVsbG8=",
      timeoutSeconds: "5",
    });
    await harness.close();
  });
});

describe("sandbox files", () => {
  it("reads a file as text", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({
      name: "sandbox_read_file",
      arguments: { sandbox_id: "sbx_test", path: "/workspace/a.txt" },
    });

    expect(result.isError).toBeFalsy();
    expect(textOf(result)).toBe("file contents");

    const read = harness.api.requests.find((request) => request.method === "GET" && request.url.includes("/files"));
    expect(read?.url).toBe("/v1/sandboxes/sbx_test/files?path=%2Fworkspace%2Fa.txt");
    await harness.close();
  });

  it("writes a file and reports the byte count", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({
      name: "sandbox_write_file",
      arguments: { sandbox_id: "sbx_test", path: "/workspace/a.txt", content: "hello" },
    });

    expect(result.isError).toBeFalsy();
    expect(textOf(result)).toContain("Wrote 5 bytes");

    const write = harness.api.requests.find((request) => request.method === "PUT");
    expect(write?.url).toBe("/v1/sandboxes/sbx_test/files?path=%2Fworkspace%2Fa.txt&create_parents=true");
    expect(write?.body).toBe("hello");
    await harness.close();
  });
});

describe("sandbox_stats", () => {
  it("summarises resource usage", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({ name: "sandbox_stats", arguments: { sandbox_id: "sbx_test" } });

    expect(result.isError).toBeFalsy();
    expect(textOf(result)).toContain("memory:      1234 bytes");
    expect(textOf(result)).toContain("cpu:         99 millis");
    await harness.close();
  });
});

describe("sandbox_destroy", () => {
  it("destroys the sandbox", async () => {
    const harness = await connect();
    const result = await harness.client.callTool({ name: "sandbox_destroy", arguments: { sandbox_id: "sbx_test" } });

    expect(result.isError).toBeFalsy();
    expect(textOf(result)).toContain("Destroyed sbx_test");
    expect(harness.api.requests.at(-1)?.method).toBe("DELETE");
    await harness.close();
  });
});