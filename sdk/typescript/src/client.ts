import { Transport } from "./http.js";
import { Sandbox } from "./sandbox.js";
import { decodeSandboxInfo, encodeSandboxSpec } from "./types.js";
import type { CreateSandboxOptions, SandboxInfo } from "./types.js";

export interface VoraOptions {
  /** Base URL of vora-api, e.g. `http://127.0.0.1:9300`. Defaults to `VORA_API_URL`. */
  baseUrl?: string;
  /** Bearer API key. Defaults to `VORA_API_KEY`. */
  apiKey?: string;
  /** Override `fetch` (custom transports, tests). */
  fetch?: typeof globalThis.fetch;
}

export interface ListSandboxesOptions {
  includeDestroyed?: boolean;
}

/** Entry point: `const vora = new Vora({ apiKey })`. */
export class Vora {
  readonly sandboxes: Sandboxes;

  constructor(options: VoraOptions = {}) {
    const baseUrl = options.baseUrl ?? env("VORA_API_URL") ?? "http://127.0.0.1:9300";
    const apiKey = options.apiKey ?? env("VORA_API_KEY");
    this.sandboxes = new Sandboxes(new Transport({ baseUrl, apiKey, fetch: options.fetch }));
  }
}

/** Sandbox lifecycle operations, reached through `vora.sandboxes`. */
export class Sandboxes {
  private readonly transport: Transport;

  constructor(transport: Transport) {
    this.transport = transport;
  }

  /** Create and start a sandbox, returning a handle to it. */
  async create(options: CreateSandboxOptions): Promise<Sandbox> {
    const raw = await this.transport.json<unknown>("/v1/sandboxes", {
      method: "POST",
      json: encodeSandboxSpec(options),
    });
    return new Sandbox(this.transport, decodeSandboxInfo(raw));
  }

  async get(id: string): Promise<Sandbox> {
    const raw = await this.transport.json<unknown>(`/v1/sandboxes/${encodeURIComponent(id)}`);
    return new Sandbox(this.transport, decodeSandboxInfo(raw));
  }

  async list(options: ListSandboxesOptions = {}): Promise<Sandbox[]> {
    const query = options.includeDestroyed === true ? "?include_destroyed=true" : "";
    const raw = await this.transport.json<{ sandboxes?: unknown[] }>(`/v1/sandboxes${query}`);
    const infos: SandboxInfo[] = (raw.sandboxes ?? []).map(decodeSandboxInfo);
    return infos.map((info) => new Sandbox(this.transport, info));
  }

  /** Destroy a sandbox by id, without constructing a handle first. */
  async destroy(id: string): Promise<void> {
    await this.transport.request(`/v1/sandboxes/${encodeURIComponent(id)}`, { method: "DELETE" });
  }
}

function env(name: string): string | undefined {
  const value = globalThis.process?.env?.[name];
  return value === undefined || value === "" ? undefined : value;
}