import { decodeBase64, encodeBase64 } from "./codec.js";
import { VoraError } from "./errors.js";
import { parseCPU, parseDuration, parseSize } from "./units.js";

// The vora-api speaks protojson: camelCase fields, enums as their proto names,
// bytes as base64, and 64-bit integers as *strings* (protojson emits them that
// way to survive JSON's float precision). This module is the single place that
// knows those rules — everything above it works with plain numbers.

type Json = Record<string, unknown>;

export type NetworkMode = "NETWORK_NONE" | "NETWORK_RESTRICTED" | "NETWORK_FULL" | "NETWORK_PRIVATE";

export type SandboxState =
  | "REQUESTED"
  | "SCHEDULING"
  | "CREATING"
  | "STARTING"
  | "RUNNING"
  | "STOPPING"
  | "STOPPED"
  | "FAILED"
  | "EXPIRED"
  | "DESTROYED";

export type ExitReason =
  | "EXITED"
  | "OOM_KILLED"
  | "PIDS_LIMIT_EXCEEDED"
  | "TIMEOUT_EXCEEDED"
  | "POLICY_VIOLATION"
  | "RUNTIME_FAILURE"
  | "DISK_QUOTA_EXCEEDED";

/** Lower-case aliases accepted wherever a `NetworkMode` is. */
export type NetworkShorthand = "none" | "restricted" | "full" | "private";

export interface NetworkPolicy {
  mode?: NetworkMode | NetworkShorthand;
  allowedDomains?: string[];
  brokerAliases?: string[];
}

export type NetworkOption = NetworkMode | NetworkShorthand | NetworkPolicy;

export type SizeInput = number | string;
export type CPUInput = number | string;
export type DurationInput = number | string;

export interface ResourceLimits {
  cpuMillis?: number;
  memoryBytes?: number;
  pids?: number;
  diskBytes?: number;
}

export interface SandboxSpec {
  sandboxId?: string;
  tenantId?: string;
  agentId?: string;
  image?: string;
  backend?: string;
  resources?: ResourceLimits;
  network?: NetworkPolicy;
  env?: string[];
  ttlSeconds?: number;
}

export interface SandboxInfo {
  id: string;
  spec?: SandboxSpec;
  state?: SandboxState;
  runnerId?: string;
  reason?: ExitReason;
  createdAtUnixMs?: number;
  updatedAtUnixMs?: number;
  imageRootfs?: string;
}

export interface Stats {
  memoryBytes?: number;
  memoryPeakBytes?: number;
  cpuMillis?: number;
  pids?: number;
  oomKills?: number;
  pidsLimitHits?: number;
}

export interface SandboxEvent {
  sandboxId?: string;
  from?: string;
  to?: string;
  reason?: string;
  runnerId?: string;
  timestampUnixMs?: number;
}

export interface SandboxExec {
  executionId?: string;
  command?: string;
  args?: string[];
  cwd?: string;
  startedAtUnixMs?: number;
  finishedAtUnixMs?: number;
  exitCode?: number;
  reason?: ExitReason;
  stdoutBytes?: number;
  stderrBytes?: number;
  cpuMillis?: number;
  memoryPeakBytes?: number;
  egressAllowed?: number;
  egressDenied?: number;
}

export interface SandboxEgress {
  sandboxId?: string;
  host?: string;
  remote?: string;
  allowed?: boolean;
  reason?: string;
  timestampUnixMs?: number;
  alias?: string;
}

/** One unit of streamed exec output. */
export type ExecChunk =
  | { type: "stdout"; data: Uint8Array }
  | { type: "stderr"; data: Uint8Array }
  | { type: "exit"; exitCode: number; reason?: ExitReason };

export interface ExecResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  reason?: ExitReason;
}

export interface CreateSandboxOptions {
  image: string;
  backend?: string;
  agentId?: string;
  tenantId?: string;
  /** Bytes, or a suffixed string such as `"512Mi"`. */
  memory?: SizeInput;
  /** Cores (`1`, `0.5`) or millicores (`"500m"`). */
  cpu?: CPUInput;
  pids?: number;
  /** Bytes, or a suffixed string such as `"2Gi"`. */
  disk?: SizeInput;
  network?: NetworkOption;
  env?: string[] | Record<string, string>;
  /** Seconds, or a duration string such as `"1h30m"`. */
  ttl?: DurationInput;
}

export interface ExecOptions {
  command: string;
  args?: string[];
  cwd?: string;
  env?: string[];
  /** Written to the child's stdin, which then sees EOF. */
  stdin?: string | Uint8Array;
  timeoutSeconds?: number;
}

/** Options for the string form of `exec`, where the command is already set. */
export type ExecCallOptions = Omit<ExecOptions, "command" | "args">;

const NETWORK_SHORTHANDS: Record<NetworkShorthand, NetworkMode> = {
  none: "NETWORK_NONE",
  restricted: "NETWORK_RESTRICTED",
  full: "NETWORK_FULL",
  private: "NETWORK_PRIVATE",
};

export function encodeSandboxSpec(options: CreateSandboxOptions): Json {
  if (typeof options.image !== "string" || options.image === "") {
    throw new VoraError("create: image is required");
  }
  const spec: Json = { image: options.image };
  if (options.backend !== undefined) spec.backend = options.backend;
  if (options.agentId !== undefined) spec.agentId = options.agentId;
  if (options.tenantId !== undefined) spec.tenantId = options.tenantId;

  const resources: Json = {};
  if (options.cpu !== undefined) resources.cpuMillis = String(parseCPU(options.cpu));
  if (options.memory !== undefined) resources.memoryBytes = String(parseSize(options.memory));
  if (options.disk !== undefined) resources.diskBytes = String(parseSize(options.disk));
  if (options.pids !== undefined) resources.pids = String(options.pids);
  if (Object.keys(resources).length > 0) spec.resources = resources;

  if (options.network !== undefined) spec.network = encodeNetwork(options.network);
  if (options.env !== undefined) spec.env = encodeEnv(options.env);
  if (options.ttl !== undefined) spec.ttlSeconds = String(parseDuration(options.ttl));
  return spec;
}

function encodeNetwork(network: NetworkOption): Json {
  if (typeof network === "string") {
    return { mode: NETWORK_SHORTHANDS[network as NetworkShorthand] ?? network };
  }
  const out: Json = {};
  if (network.mode !== undefined) {
    out.mode = NETWORK_SHORTHANDS[network.mode as NetworkShorthand] ?? network.mode;
  }
  if (network.allowedDomains !== undefined && network.allowedDomains.length > 0) {
    out.allowedDomains = network.allowedDomains;
  }
  if (network.brokerAliases !== undefined && network.brokerAliases.length > 0) {
    out.brokerAliases = network.brokerAliases;
  }
  return out;
}

function encodeEnv(env: string[] | Record<string, string>): string[] {
  if (Array.isArray(env)) {
    return env;
  }
  return Object.entries(env).map(([key, value]) => `${key}=${value}`);
}

export function encodeExecRequest(options: ExecOptions): Json {
  if (typeof options.command !== "string" || options.command === "") {
    throw new VoraError("exec: command is required");
  }
  const request: Json = { command: options.command };
  if (options.args !== undefined && options.args.length > 0) request.args = options.args;
  if (options.cwd !== undefined) request.cwd = options.cwd;
  if (options.env !== undefined && options.env.length > 0) request.env = options.env;
  if (options.timeoutSeconds !== undefined) request.timeoutSeconds = String(options.timeoutSeconds);
  if (options.stdin !== undefined) request.stdin = encodeBase64(options.stdin);
  return request;
}

// ---------------------------------------------------------------------------
// Decoders: protojson wire shape -> plain-number values.
// ---------------------------------------------------------------------------

function asString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function asBool(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

/** protojson writes int64 as a string; a number is also accepted on input. */
function asNumber(value: unknown): number | undefined {
  if (value === undefined || value === null || value === "") {
    return undefined;
  }
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function asObject(value: unknown): Json | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value) ? (value as Json) : undefined;
}

function asArray<T>(value: unknown, item: (raw: unknown) => T | undefined): T[] | undefined {
  if (!Array.isArray(value)) {
    return undefined;
  }
  const out: T[] = [];
  for (const raw of value) {
    const parsed = item(raw);
    if (parsed !== undefined) {
      out.push(parsed);
    }
  }
  return out;
}

export function decodeSandboxInfo(raw: unknown): SandboxInfo {
  const source = asObject(raw) ?? {};
  const info: SandboxInfo = { id: asString(source.id) ?? "" };
  const spec = decodeSandboxSpec(source.spec);
  if (spec !== undefined) info.spec = spec;
  assign(info, "state", asString(source.state) as SandboxState | undefined);
  assign(info, "runnerId", asString(source.runnerId));
  assign(info, "reason", asString(source.reason) as ExitReason | undefined);
  assign(info, "createdAtUnixMs", asNumber(source.createdAtUnixMs));
  assign(info, "updatedAtUnixMs", asNumber(source.updatedAtUnixMs));
  assign(info, "imageRootfs", asString(source.imageRootfs));
  return info;
}

function decodeSandboxSpec(raw: unknown): SandboxSpec | undefined {
  const source = asObject(raw);
  if (source === undefined) {
    return undefined;
  }
  const spec: SandboxSpec = {};
  assign(spec, "sandboxId", asString(source.sandboxId));
  assign(spec, "tenantId", asString(source.tenantId));
  assign(spec, "agentId", asString(source.agentId));
  assign(spec, "image", asString(source.image));
  assign(spec, "backend", asString(source.backend));
  assign(spec, "env", asArray(source.env, asString));
  assign(spec, "ttlSeconds", asNumber(source.ttlSeconds));

  const resources = asObject(source.resources);
  if (resources !== undefined) {
    const limits: ResourceLimits = {};
    assign(limits, "cpuMillis", asNumber(resources.cpuMillis));
    assign(limits, "memoryBytes", asNumber(resources.memoryBytes));
    assign(limits, "pids", asNumber(resources.pids));
    assign(limits, "diskBytes", asNumber(resources.diskBytes));
    spec.resources = limits;
  }

  const network = asObject(source.network);
  if (network !== undefined) {
    const policy: NetworkPolicy = {};
    assign(policy, "mode", asString(network.mode) as NetworkMode | undefined);
    assign(policy, "allowedDomains", asArray(network.allowedDomains, asString));
    assign(policy, "brokerAliases", asArray(network.brokerAliases, asString));
    spec.network = policy;
  }
  return spec;
}

export function decodeStats(raw: unknown): Stats {
  const source = asObject(raw) ?? {};
  const stats: Stats = {};
  assign(stats, "memoryBytes", asNumber(source.memoryBytes));
  assign(stats, "memoryPeakBytes", asNumber(source.memoryPeakBytes));
  assign(stats, "cpuMillis", asNumber(source.cpuMillis));
  assign(stats, "pids", asNumber(source.pids));
  assign(stats, "oomKills", asNumber(source.oomKills));
  assign(stats, "pidsLimitHits", asNumber(source.pidsLimitHits));
  return stats;
}

export function decodeSandboxEvent(raw: unknown): SandboxEvent {
  const source = asObject(raw) ?? {};
  const event: SandboxEvent = {};
  assign(event, "sandboxId", asString(source.sandboxId));
  assign(event, "from", asString(source.from));
  assign(event, "to", asString(source.to));
  assign(event, "reason", asString(source.reason));
  assign(event, "runnerId", asString(source.runnerId));
  assign(event, "timestampUnixMs", asNumber(source.timestampUnixMs));
  return event;
}

export function decodeSandboxExec(raw: unknown): SandboxExec {
  const source = asObject(raw) ?? {};
  const exec: SandboxExec = {};
  assign(exec, "executionId", asString(source.executionId));
  assign(exec, "command", asString(source.command));
  assign(exec, "args", asArray(source.args, asString));
  assign(exec, "cwd", asString(source.cwd));
  assign(exec, "startedAtUnixMs", asNumber(source.startedAtUnixMs));
  assign(exec, "finishedAtUnixMs", asNumber(source.finishedAtUnixMs));
  assign(exec, "exitCode", asNumber(source.exitCode));
  assign(exec, "reason", asString(source.reason) as ExitReason | undefined);
  assign(exec, "stdoutBytes", asNumber(source.stdoutBytes));
  assign(exec, "stderrBytes", asNumber(source.stderrBytes));
  assign(exec, "cpuMillis", asNumber(source.cpuMillis));
  assign(exec, "memoryPeakBytes", asNumber(source.memoryPeakBytes));
  assign(exec, "egressAllowed", asNumber(source.egressAllowed));
  assign(exec, "egressDenied", asNumber(source.egressDenied));
  return exec;
}

export function decodeSandboxEgress(raw: unknown): SandboxEgress {
  const source = asObject(raw) ?? {};
  const record: SandboxEgress = {};
  assign(record, "sandboxId", asString(source.sandboxId));
  assign(record, "host", asString(source.host));
  assign(record, "remote", asString(source.remote));
  assign(record, "allowed", asBool(source.allowed));
  assign(record, "reason", asString(source.reason));
  assign(record, "timestampUnixMs", asNumber(source.timestampUnixMs));
  assign(record, "alias", asString(source.alias));
  return record;
}

export function decodeExecChunk(raw: unknown): ExecChunk | undefined {
  const source = asObject(raw);
  if (source === undefined) {
    return undefined;
  }
  const stdout = asString(source.stdout);
  if (stdout !== undefined) {
    return { type: "stdout", data: decodeBase64(stdout) };
  }
  const stderr = asString(source.stderr);
  if (stderr !== undefined) {
    return { type: "stderr", data: decodeBase64(stderr) };
  }
  const exit = asObject(source.exit);
  if (exit !== undefined) {
    const chunk: ExecChunk = { type: "exit", exitCode: asNumber(exit.exitCode) ?? 0 };
    const reason = asString(exit.reason) as ExitReason | undefined;
    if (reason !== undefined) {
      chunk.reason = reason;
    }
    return chunk;
  }
  return undefined;
}

function assign<T extends object, K extends keyof T>(target: T, key: K, value: T[K] | undefined): void {
  if (value !== undefined) {
    target[key] = value;
  }
}