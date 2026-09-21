import { decodeText, ndjson } from "./codec.js";
import type { Transport } from "./http.js";
import {
  decodeExecChunk,
  decodeSandboxEgress,
  decodeSandboxEvent,
  decodeSandboxExec,
  decodeSandboxInfo,
  decodeStats,
  encodeExecRequest,
} from "./types.js";
import type {
  ExecCallOptions,
  ExecChunk,
  ExecOptions,
  ExecResult,
  SandboxEgress,
  SandboxEvent,
  SandboxExec,
  SandboxInfo,
  Stats,
} from "./types.js";

export interface WriteFileOptions {
  /** Create leading directories when they are missing (default true). */
  createParents?: boolean;
}

/** A handle to one sandbox. Methods hit the API; `info` is the last snapshot. */
export class Sandbox {
  /** Latest observed state, refreshed by `refresh()`. */
  info: SandboxInfo;

  private readonly transport: Transport;

  constructor(transport: Transport, info: SandboxInfo) {
    this.transport = transport;
    this.info = info;
  }

  get id(): string {
    return this.info.id;
  }

  /** Re-read the sandbox record from the API. */
  async refresh(): Promise<SandboxInfo> {
    const raw = await this.transport.json<unknown>(`/v1/sandboxes/${this.pathId()}`);
    this.info = decodeSandboxInfo(raw);
    return this.info;
  }

  /**
   * Run a command and collect its output. A string is executed through
   * `/bin/sh -c`, so quoting, pipes and redirection work; pass an object to
   * set `command`/`args` literally.
   */
  async exec(command: string, options?: ExecCallOptions): Promise<ExecResult>;
  async exec(options: ExecOptions): Promise<ExecResult>;
  async exec(commandOrOptions: string | ExecOptions, options?: ExecCallOptions): Promise<ExecResult> {
    const stdout: Uint8Array[] = [];
    const stderr: Uint8Array[] = [];
    let exitCode = 0;
    let reason: ExecResult["reason"];
    for await (const chunk of this.stream(toExecRequest(commandOrOptions, options))) {
      switch (chunk.type) {
        case "stdout":
          stdout.push(chunk.data);
          break;
        case "stderr":
          stderr.push(chunk.data);
          break;
        case "exit":
          exitCode = chunk.exitCode;
          reason = chunk.reason;
          break;
      }
    }
    const result: ExecResult = {
      stdout: decodeText(concat(stdout)),
      stderr: decodeText(concat(stderr)),
      exitCode,
    };
    if (reason !== undefined) {
      result.reason = reason;
    }
    return result;
  }

  /** Stream exec output as chunks, ending with an `exit` chunk. */
  streamExec(command: string, options?: ExecCallOptions): AsyncIterable<ExecChunk>;
  streamExec(options: ExecOptions): AsyncIterable<ExecChunk>;
  streamExec(commandOrOptions: string | ExecOptions, options?: ExecCallOptions): AsyncIterable<ExecChunk> {
    const request = toExecRequest(commandOrOptions, options);
    return this.stream(request);
  }

  async writeFile(path: string, content: string | Uint8Array, options: WriteFileOptions = {}): Promise<number> {
    const query = new URLSearchParams({ path });
    if (options.createParents ?? true) {
      query.set("create_parents", "true");
    }
    const body = typeof content === "string" ? new TextEncoder().encode(content) : content;
    const response = await this.transport.request(`/v1/sandboxes/${this.pathId()}/files?${query.toString()}`, {
      method: "PUT",
      body: body as BodyInit,
      contentType: "application/octet-stream",
    });
    const result = (await response.json()) as { bytes_written?: number };
    return result.bytes_written ?? body.byteLength;
  }

  async readFile(path: string): Promise<Uint8Array> {
    const query = new URLSearchParams({ path });
    return this.transport.bytes(`/v1/sandboxes/${this.pathId()}/files?${query.toString()}`);
  }

  async readText(path: string): Promise<string> {
    return decodeText(await this.readFile(path));
  }

  async stats(): Promise<Stats> {
    return decodeStats(await this.transport.json<unknown>(`/v1/sandboxes/${this.pathId()}/stats`));
  }

  async events(): Promise<SandboxEvent[]> {
    const raw = await this.transport.json<{ events?: unknown[] }>(`/v1/sandboxes/${this.pathId()}/events`);
    return (raw.events ?? []).map(decodeSandboxEvent);
  }

  async execs(): Promise<SandboxExec[]> {
    const raw = await this.transport.json<{ execs?: unknown[] }>(`/v1/sandboxes/${this.pathId()}/execs`);
    return (raw.execs ?? []).map(decodeSandboxExec);
  }

  async egress(): Promise<SandboxEgress[]> {
    const raw = await this.transport.json<{ records?: unknown[] }>(`/v1/sandboxes/${this.pathId()}/egress`);
    return (raw.records ?? []).map(decodeSandboxEgress);
  }

  /** Destroy the sandbox. */
  async destroy(): Promise<void> {
    await this.transport.request(`/v1/sandboxes/${this.pathId()}`, { method: "DELETE" });
  }

  private async *stream(request: Record<string, unknown>): AsyncGenerator<ExecChunk> {
    const response = await this.transport.request(`/v1/sandboxes/${this.pathId()}/exec`, {
      method: "POST",
      body: JSON.stringify(request),
      contentType: "application/json",
    });
    if (response.body === null) {
      return;
    }
    for await (const raw of ndjson<unknown>(response.body)) {
      const chunk = decodeExecChunk(raw);
      if (chunk !== undefined) {
        yield chunk;
      }
    }
  }

  private pathId(): string {
    return encodeURIComponent(this.info.id);
  }
}

function toExecRequest(commandOrOptions: string | ExecOptions, options?: ExecCallOptions): Record<string, unknown> {
  // A bare string is a shell command line; `/bin/sh` exists in every Vora base
  // image and keeps pipelines and quoting working the way callers expect.
  if (typeof commandOrOptions === "string") {
    return encodeExecRequest({ command: "sh", args: ["-c", commandOrOptions], ...options });
  }
  return encodeExecRequest(commandOrOptions);
}

function concat(chunks: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const chunk of chunks) {
    total += chunk.byteLength;
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}