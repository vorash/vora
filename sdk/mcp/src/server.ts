import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { parseCPU, parseDuration, parseSize } from "@vorash/sdk";
import type { ExecResult, Vora } from "@vorash/sdk";
import { z } from "zod";

import { fail, ok } from "./result.js";

export interface ServerOptions {
  name?: string;
  version: string;
}

/**
 * Build the Vora MCP server. The `vora` client is injected so the same wiring
 * can be exercised in tests against a stand-in API.
 */
export function createServer(vora: Vora, options: ServerOptions): McpServer {
  const server = new McpServer({ name: options.name ?? "vora", version: options.version });
  registerTools(server, vora, policyFromEnv());
  return server;
}

const utf8 = new TextDecoder("utf-8", { fatal: true });

interface SandboxPolicy {
  networks: Set<string>;
  images: Set<string>;
  maxMemory: number;
  maxCPU: number;
  maxPids: number;
  maxDisk: number;
  maxTTL: number;
}

function policyFromEnv(): SandboxPolicy {
  const values = (name: string, fallback: string): Set<string> =>
    new Set((process.env[name] ?? fallback).split(",").map((s) => s.trim()).filter(Boolean));
  const cap = (name: string, fallback: string, parse: (v: string) => number): number => {
    const value = parse(process.env[name] ?? fallback);
    if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${name} must be a positive finite value`);
    return value;
  };
  const networks = values("VORA_MCP_ALLOWED_NETWORKS", "none");
  if ([...networks].some((n) => !["none", "restricted", "full", "private"].includes(n))) {
    throw new Error("invalid VORA_MCP_ALLOWED_NETWORKS");
  }
  return {
    networks,
    images: values("VORA_MCP_ALLOWED_IMAGES", "vora/base,vora/python,vora/node"),
    maxMemory: cap("VORA_MCP_MAX_MEMORY", "1Gi", parseSize),
    maxCPU: cap("VORA_MCP_MAX_CPU", "2", parseCPU),
    maxPids: cap("VORA_MCP_MAX_PIDS", "128", Number),
    maxDisk: cap("VORA_MCP_MAX_DISK", "2Gi", parseSize),
    maxTTL: cap("VORA_MCP_MAX_TTL", "1h", parseDuration),
  };
}

function registerTools(server: McpServer, vora: Vora, policy: SandboxPolicy): void {
  server.registerTool(
    "sandbox_create",
    {
      title: "Create a sandbox",
      description:
        "Create and start a Vora sandbox and return its sandbox_id. " +
        "Pass that id to the other sandbox_* tools.",
      inputSchema: {
        image: z.string().describe("Rootfs image, for example vora/python or vora/base"),
        memory: z.string().optional().describe("Memory limit, for example '512Mi', or a byte count"),
        cpu: z.union([z.number(), z.string()]).optional().describe("CPU cores (1, 0.5) or millicores ('500m')"),
        pids: z.number().int().positive().optional().describe("Maximum number of processes"),
        disk: z.string().optional().describe("Disk quota, for example '2Gi'"),
        network: z
          .enum(["none", "restricted", "full", "private"])
          .optional()
          .describe("Egress policy; 'none' is the default-deny mode"),
        env: z.array(z.string()).optional().describe("Environment variables as KEY=VALUE"),
        ttl: z.union([z.number(), z.string()]).optional().describe("Lifetime in seconds or a duration such as '1h30m'"),
        backend: z.string().optional().describe("Isolation backend, for example bwrap or gvisor"),
      },
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    async (args) => {
      try {
        const network = args.network ?? "none";
        const memory = args.memory ?? "512Mi";
        const cpu = args.cpu ?? 1;
        const pids = args.pids ?? 64;
        const disk = args.disk ?? "1Gi";
        const ttl = args.ttl ?? "1h";
        if (!policy.networks.has(network) || !policy.images.has(args.image)) {
          throw new Error("sandbox image or network mode is not allowed by the MCP server policy");
        }
        if (parseSize(memory) > policy.maxMemory || parseCPU(cpu) > policy.maxCPU ||
            pids > policy.maxPids || parseSize(disk) > policy.maxDisk || parseDuration(ttl) > policy.maxTTL) {
          throw new Error("sandbox resources or TTL exceed the MCP server policy");
        }
        const sandbox = await vora.sandboxes.create({
          image: args.image,
          memory,
          cpu,
          pids,
          disk,
          network,
          env: args.env,
          ttl,
          backend: args.backend,
        });
        const state = sandbox.info.state ?? "unknown";
        return ok(`Created sandbox ${sandbox.id} (${state}).`, { sandbox_id: sandbox.id, state });
      } catch (error) {
        return fail(error);
      }
    },
  );

  server.registerTool(
    "sandbox_exec",
    {
      title: "Run a command in a sandbox",
      description:
        "Run a command inside a running sandbox and return its stdout, stderr and exit code. " +
        "The command string is run through /bin/sh -c, so pipes and quoting work.",
      inputSchema: {
        sandbox_id: z.string().describe("Sandbox id returned by sandbox_create"),
        command: z.string().describe("Command line, run through /bin/sh -c"),
        cwd: z.string().optional().describe("Working directory inside the sandbox"),
        env: z.array(z.string()).optional().describe("Extra environment variables as KEY=VALUE"),
        stdin: z.string().optional().describe("Text written to the command's stdin, which then sees EOF"),
        timeout_seconds: z.number().int().positive().optional().describe("Wall-clock timeout for the command"),
      },
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: true },
    },
    async (args) => {
      try {
        const sandbox = await vora.sandboxes.get(args.sandbox_id);
        const result = await sandbox.exec(args.command, {
          cwd: args.cwd,
          env: args.env,
          stdin: args.stdin,
          timeoutSeconds: args.timeout_seconds,
        });
        const structured: Record<string, unknown> = { exit_code: result.exitCode };
        if (result.reason !== undefined) {
          structured.reason = result.reason;
        }
        return ok(renderExec(result), structured);
      } catch (error) {
        return fail(error);
      }
    },
  );

  server.registerTool(
    "sandbox_read_file",
    {
      title: "Read a file from a sandbox",
      description:
        "Read a file's contents from a sandbox. UTF-8 text is returned as-is; " +
        "binary content is returned base64-encoded.",
      inputSchema: {
        sandbox_id: z.string(),
        path: z.string().describe("Absolute path inside the sandbox, for example /workspace/main.py"),
      },
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async (args) => {
      try {
        const sandbox = await vora.sandboxes.get(args.sandbox_id);
        const bytes = await sandbox.readFile(args.path);
        try {
          return ok(utf8.decode(bytes));
        } catch {
          return ok(`[base64] ${Buffer.from(bytes).toString("base64")}`, { encoding: "base64", bytes: bytes.byteLength });
        }
      } catch (error) {
        return fail(error);
      }
    },
  );

  server.registerTool(
    "sandbox_write_file",
    {
      title: "Write a file into a sandbox",
      description: "Write a file into a sandbox, creating parent directories by default.",
      inputSchema: {
        sandbox_id: z.string(),
        path: z.string().describe("Absolute path inside the sandbox"),
        content: z.string().describe("File contents"),
        create_parents: z.boolean().optional().describe("Create missing parent directories (default true)"),
      },
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async (args) => {
      try {
        const sandbox = await vora.sandboxes.get(args.sandbox_id);
        const written = await sandbox.writeFile(args.path, args.content, { createParents: args.create_parents });
        return ok(`Wrote ${written} bytes to ${args.path}.`, { path: args.path, bytes_written: written });
      } catch (error) {
        return fail(error);
      }
    },
  );

  server.registerTool(
    "sandbox_stats",
    {
      title: "Read sandbox resource usage",
      description: "Read the observed resource usage of a sandbox (memory, CPU, processes, OOM kills).",
      inputSchema: { sandbox_id: z.string() },
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async (args) => {
      try {
        const sandbox = await vora.sandboxes.get(args.sandbox_id);
        const stats = await sandbox.stats();
        const lines = [
          `memory:      ${stats.memoryBytes ?? 0} bytes`,
          `peak:        ${stats.memoryPeakBytes ?? 0} bytes`,
          `cpu:         ${stats.cpuMillis ?? 0} millis`,
          `pids:        ${stats.pids ?? 0}`,
          `oom kills:   ${stats.oomKills ?? 0}`,
        ];
        return ok(lines.join("\n"), { ...stats });
      } catch (error) {
        return fail(error);
      }
    },
  );

  server.registerTool(
    "sandbox_destroy",
    {
      title: "Destroy a sandbox",
      description: "Destroy a sandbox and release its resources. This cannot be undone.",
      inputSchema: { sandbox_id: z.string() },
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async (args) => {
      try {
        await vora.sandboxes.destroy(args.sandbox_id);
        return ok(`Destroyed ${args.sandbox_id}.`);
      } catch (error) {
        return fail(error);
      }
    },
  );
}

function renderExec(result: ExecResult): string {
  const header = result.reason === undefined ? `exit ${result.exitCode}` : `exit ${result.exitCode} (${result.reason})`;
  let text = header;
  if (result.stdout !== "") {
    text += `\n\nstdout:\n${result.stdout}`;
  }
  if (result.stderr !== "") {
    text += `\n\nstderr:\n${result.stderr}`;
  }
  return text;
}