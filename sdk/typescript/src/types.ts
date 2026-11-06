import { decodeBase64, encodeBase64 } from "./codec.js";
import { VoraError } from "./errors.js";
import type { RetryPolicy } from "./http.js";
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

/**
 * What a memory peak covers: only this exec (`PEAK_SCOPE_WINDOW`), or the
 * sandbox's lifetime because the kernel could not reset it before the exec
 * (`PEAK_SCOPE_LIFETIME`, Linux before 6.12). Absent when not measured.
 */
export type PeakScope = "PEAK_SCOPE_UNSPECIFIED" | "PEAK_SCOPE_WINDOW" | "PEAK_SCOPE_LIFETIME";

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
  poolId?: string;
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
  /** The peak during this exec's window; `memoryPeakScope` says which window. */
  memoryPeakBytes?: number;
  egressAllowed?: number;
  egressDenied?: number;
  memoryPeakScope?: PeakScope;
  oomKills?: number;
  pidsLimitHits?: number;
  poolId?: string;
  sandboxRun?: number;
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

/** A capture of a sandbox's writable state (ADR 023). */
export interface SnapshotInfo {
  id: string;
  sandboxId?: string;
  tenantId?: string;
  runnerId?: string;
  spec?: SandboxSpec;
  bytes?: number;
  createdAtUnixMs?: number;
}

/**
 * What the sandbox used during one exec. The counters are per-exec deltas. The
 * window is the whole sandbox, so concurrent execs in one sandbox have
 * overlapping windows and count each other's usage.
 */
export interface ExecUsage {
  cpuMillis?: number;
  memoryPeakBytes?: number;
  memoryPeakScope?: PeakScope;
  oomKills?: number;
  pidsLimitHits?: number;
}

/** One unit of streamed exec output. The exit chunk names the exec's audit record and its usage. */
export type ExecChunk =
  | { type: "stdout"; data: Uint8Array }
  | { type: "stderr"; data: Uint8Array }
  | { type: "exit"; exitCode: number; reason?: ExitReason; executionId?: string; usage?: ExecUsage };

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

export type PoolReuse = "POOL_REUSE_RESET" | "POOL_REUSE_UNSAFE_NO_RESET";
export type PoolReuseShorthand = "reset" | "unsafe_no_reset";
export type PoolState = "POOL_ACTIVE" | "POOL_DRAINING" | "POOL_NOT_LOADED";
export type PoolResetOutcome = "POOL_RESET_OK" | "POOL_RESET_FAILED" | "POOL_RESET_SKIPPED" | "POOL_RESET_RETIRED";

/** A pool's definition. Unset limits are absent until the runner fills in defaults. */
export interface PoolSpec {
  name?: string;
  tenantId?: string;
  agentId?: string;
  sandbox?: SandboxSpec;
  size?: number;
  maxSize?: number;
  maxRuns?: number;
  maxAgeSeconds?: number;
  maxRunSeconds?: number;
  borrowTimeoutMs?: number;
  idleExpirySeconds?: number;
  reuse?: PoolReuse;
}

/**
 * A runner's snapshot of one pool. Counters reset when the runner restarts. The
 * runner fills all of them from memory on every answer, so a key protojson left out
 * is a zero the runner did report.
 */
export interface PoolStats {
  starting: number;
  idle: number;
  busy: number;
  resetting: number;
  retiring: number;
  runsTotal: number;
  warmRunsTotal: number;
  resetsTotal: number;
  resetFailuresTotal: number;
  destroyedTotal: number;
  exhaustedTotal: number;
  startFailedTotal: number;
  lastStartError?: string;
  dormant: boolean;
}

/**
 * A pool's record. Read `state` before `stats`. In a listing, a pool whose runner did
 * not answer has neither. A `POOL_NOT_LOADED` pool has all-zero stats, and
 * `notLoadedReason` says why it has no members.
 */
export interface PoolInfo {
  id: string;
  spec?: PoolSpec;
  state?: PoolState;
  runnerId?: string;
  stats?: PoolStats;
  createdAtUnixMs?: number;
  notLoadedReason?: string;
}

/**
 * How a run got its sandbox: the first frame of every pool run. protojson leaves out
 * zeros, so an absent duration is either not measured or under a millisecond, and
 * the API does not say which. `resetMs` here is the reset that prepared this sandbox,
 * absent on its first run. This run's own reset is `PoolRunDone.resetMs`.
 */
export interface PoolRun {
  poolId?: string;
  sandboxId?: string;
  executionId?: string;
  warm: boolean;
  startupMs?: number;
  resetMs?: number;
  sandboxRun?: number;
  reuse?: PoolReuse;
}

/** How the command ended. `usage` fields stay absent when not reported. */
export interface RunExit {
  exitCode: number;
  reason?: ExitReason;
  executionId?: string;
  usage?: ExecUsage;
}

/**
 * What happened to the sandbox after the run. `outcome` is `POOL_RESET_FAILED` when a
 * reset could not prove the sandbox clean, and `POOL_RESET_SKIPPED` on an unsafe pool,
 * where nothing was cleaned. `retireCause` is set when the sandbox is being destroyed.
 */
export interface PoolRunDone {
  outcome?: PoolResetOutcome;
  resetMs?: number;
  retireCause?: string;
}

/** Why a stream ended after `exit` with no `runDone`. */
export interface RunStreamError {
  code: string;
  message: string;
}

/**
 * One unit of a pool run: `run`, output, `exit`, then `runDone`. `streamError` comes
 * only after `exit`, in place of `runDone`: the run's result stands, and the reset's
 * outcome is unknown. A failure before `exit` throws `PoolRunError` instead.
 */
export type RunChunk =
  | { type: "run"; run: PoolRun }
  | { type: "stdout"; data: Uint8Array }
  | { type: "stderr"; data: Uint8Array }
  | { type: "exit"; exit: RunExit }
  | { type: "runDone"; done: PoolRunDone }
  | { type: "streamError"; error: RunStreamError };

/**
 * A finished pool run. `done` is absent when the stream ended before the reset's
 * outcome arrived, and `streamError` says why. The result is still valid: the command
 * had finished.
 */
export interface PoolRunResult {
  run: PoolRun;
  exit: RunExit;
  stdout: string;
  stderr: string;
  done?: PoolRunDone;
  streamError?: RunStreamError;
}

export interface DeclarePoolOptions extends Omit<CreateSandboxOptions, "ttl" | "network"> {
  /** Pools support `"none"` only in this version, which is the default. */
  network?: NetworkOption;
  /** Warm target; `0` starts every run cold. */
  size?: number;
  maxSize?: number;
  maxRuns?: number;
  maxAgeSeconds?: number;
  maxRunSeconds?: number;
  borrowTimeoutMs?: number;
  idleExpirySeconds?: number;
  /** `"unsafe_no_reset"` needs an admin key and a runner that grants it. */
  reuse?: PoolReuse | PoolReuseShorthand;
}

export interface PoolRunOptions extends ExecOptions {
  timeoutSeconds: number;
  /**
   * Repeat a 429 or 503, which mean nothing ran. Absent means no retry, as a repeated
   * run isn't idempotent. A failure after the first frame is never retried.
   */
  retry?: RetryPolicy;
  /** Aborting before `exit` cancels the run. */
  signal?: AbortSignal;
}

/** Options for the string form of `run`, where the command is already set. */
export type PoolRunCallOptions = Omit<PoolRunOptions, "command" | "args">;

const REUSE_SHORTHANDS: Record<PoolReuseShorthand, PoolReuse> = {
  reset: "POOL_REUSE_RESET",
  unsafe_no_reset: "POOL_REUSE_UNSAFE_NO_RESET",
};

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

/** The `uint32` limits go as JSON numbers, the `int64` ones as strings. */
export function encodePoolSpec(name: string, options: DeclarePoolOptions): Json {
  const { tenantId, agentId, network, ...template } = options;
  const spec: Json = { name, sandbox: encodeSandboxSpec({ ...template, network: network ?? "none" }) };
  if (tenantId !== undefined) spec.tenantId = tenantId;
  if (agentId !== undefined) spec.agentId = agentId;
  if (options.size !== undefined) spec.size = options.size;
  if (options.maxSize !== undefined) spec.maxSize = options.maxSize;
  if (options.maxRuns !== undefined) spec.maxRuns = options.maxRuns;
  if (options.maxAgeSeconds !== undefined) spec.maxAgeSeconds = String(options.maxAgeSeconds);
  if (options.maxRunSeconds !== undefined) spec.maxRunSeconds = String(options.maxRunSeconds);
  if (options.borrowTimeoutMs !== undefined) spec.borrowTimeoutMs = String(options.borrowTimeoutMs);
  if (options.idleExpirySeconds !== undefined) spec.idleExpirySeconds = String(options.idleExpirySeconds);
  if (options.reuse !== undefined) {
    spec.reuse = REUSE_SHORTHANDS[options.reuse as PoolReuseShorthand] ?? options.reuse;
  }
  return spec;
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

export function decodeSnapshotInfo(raw: unknown): SnapshotInfo {
  const source = asObject(raw) ?? {};
  const info: SnapshotInfo = { id: asString(source.id) ?? "" };
  const spec = decodeSandboxSpec(source.spec);
  if (spec !== undefined) info.spec = spec;
  assign(info, "sandboxId", asString(source.sandboxId));
  assign(info, "tenantId", asString(source.tenantId));
  assign(info, "runnerId", asString(source.runnerId));
  assign(info, "bytes", asNumber(source.bytes));
  assign(info, "createdAtUnixMs", asNumber(source.createdAtUnixMs));
  return info;
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
  assign(info, "poolId", asString(source.poolId));
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
  assign(exec, "memoryPeakScope", asString(source.memoryPeakScope) as PeakScope | undefined);
  assign(exec, "oomKills", asNumber(source.oomKills));
  assign(exec, "pidsLimitHits", asNumber(source.pidsLimitHits));
  assign(exec, "poolId", asString(source.poolId));
  assign(exec, "sandboxRun", asNumber(source.sandboxRun));
  return exec;
}

function decodeExecUsage(raw: unknown): ExecUsage | undefined {
  const source = asObject(raw);
  if (source === undefined) {
    return undefined;
  }
  const usage: ExecUsage = {};
  assign(usage, "cpuMillis", asNumber(source.cpuMillis));
  assign(usage, "memoryPeakBytes", asNumber(source.memoryPeakBytes));
  assign(usage, "memoryPeakScope", asString(source.memoryPeakScope) as PeakScope | undefined);
  assign(usage, "oomKills", asNumber(source.oomKills));
  assign(usage, "pidsLimitHits", asNumber(source.pidsLimitHits));
  return usage;
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
    assign(chunk, "executionId", asString(exit.executionId));
    assign(chunk, "usage", decodeExecUsage(exit.usage));
    return chunk;
  }
  return undefined;
}

function decodePoolSpec(raw: unknown): PoolSpec | undefined {
  const source = asObject(raw);
  if (source === undefined) {
    return undefined;
  }
  const spec: PoolSpec = {};
  assign(spec, "name", asString(source.name));
  assign(spec, "tenantId", asString(source.tenantId));
  assign(spec, "agentId", asString(source.agentId));
  assign(spec, "sandbox", decodeSandboxSpec(source.sandbox));
  assign(spec, "size", asNumber(source.size));
  assign(spec, "maxSize", asNumber(source.maxSize));
  assign(spec, "maxRuns", asNumber(source.maxRuns));
  assign(spec, "maxAgeSeconds", asNumber(source.maxAgeSeconds));
  assign(spec, "maxRunSeconds", asNumber(source.maxRunSeconds));
  assign(spec, "borrowTimeoutMs", asNumber(source.borrowTimeoutMs));
  assign(spec, "idleExpirySeconds", asNumber(source.idleExpirySeconds));
  assign(spec, "reuse", asString(source.reuse) as PoolReuse | undefined);
  return spec;
}

function decodePoolStats(raw: unknown): PoolStats | undefined {
  const source = asObject(raw);
  if (source === undefined) {
    return undefined;
  }
  const stats: PoolStats = {
    starting: asNumber(source.starting) ?? 0,
    idle: asNumber(source.idle) ?? 0,
    busy: asNumber(source.busy) ?? 0,
    resetting: asNumber(source.resetting) ?? 0,
    retiring: asNumber(source.retiring) ?? 0,
    runsTotal: asNumber(source.runsTotal) ?? 0,
    warmRunsTotal: asNumber(source.warmRunsTotal) ?? 0,
    resetsTotal: asNumber(source.resetsTotal) ?? 0,
    resetFailuresTotal: asNumber(source.resetFailuresTotal) ?? 0,
    destroyedTotal: asNumber(source.destroyedTotal) ?? 0,
    exhaustedTotal: asNumber(source.exhaustedTotal) ?? 0,
    startFailedTotal: asNumber(source.startFailedTotal) ?? 0,
    dormant: asBool(source.dormant) ?? false,
  };
  assign(stats, "lastStartError", asString(source.lastStartError));
  return stats;
}

export function decodePoolInfo(raw: unknown): PoolInfo {
  const source = asObject(raw) ?? {};
  const info: PoolInfo = { id: asString(source.id) ?? "" };
  assign(info, "spec", decodePoolSpec(source.spec));
  assign(info, "state", asString(source.state) as PoolState | undefined);
  assign(info, "runnerId", asString(source.runnerId));
  assign(info, "stats", decodePoolStats(source.stats));
  assign(info, "createdAtUnixMs", asNumber(source.createdAtUnixMs));
  assign(info, "notLoadedReason", asString(source.notLoadedReason));
  return info;
}

function decodePoolRun(source: Json): PoolRun {
  const run: PoolRun = { warm: source.warm === true };
  assign(run, "poolId", asString(source.poolId));
  assign(run, "sandboxId", asString(source.sandboxId));
  assign(run, "executionId", asString(source.executionId));
  assign(run, "startupMs", asNumber(source.startupMs));
  assign(run, "resetMs", asNumber(source.resetMs));
  assign(run, "sandboxRun", asNumber(source.sandboxRun));
  assign(run, "reuse", asString(source.reuse) as PoolReuse | undefined);
  return run;
}

/** One frame of a run stream. The `{"error": ...}` last line is not a frame. */
export function decodeRunChunk(raw: unknown): RunChunk | undefined {
  const source = asObject(raw);
  if (source === undefined) {
    return undefined;
  }
  const run = asObject(source.run);
  if (run !== undefined) {
    return { type: "run", run: decodePoolRun(run) };
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
    // An exit frame's code is always real: protojson leaves out only a zero.
    const decoded: RunExit = { exitCode: asNumber(exit.exitCode) ?? 0 };
    assign(decoded, "reason", asString(exit.reason) as ExitReason | undefined);
    assign(decoded, "executionId", asString(exit.executionId));
    assign(decoded, "usage", decodeExecUsage(exit.usage));
    return { type: "exit", exit: decoded };
  }
  const done = asObject(source.runDone);
  if (done !== undefined) {
    const decoded: PoolRunDone = {};
    assign(decoded, "outcome", asString(done.outcome) as PoolResetOutcome | undefined);
    assign(decoded, "resetMs", asNumber(done.resetMs));
    assign(decoded, "retireCause", asString(done.retireCause));
    return { type: "runDone", done: decoded };
  }
  return undefined;
}

/** The API's last line for a run that failed after its first frame. */
export function decodeRunError(raw: unknown): RunStreamError | undefined {
  const error = asObject(asObject(raw)?.error);
  if (error === undefined) {
    return undefined;
  }
  return {
    code: asString(error.code) ?? "run_failed",
    message: asString(error.message) ?? "the run ended without a result",
  };
}

function assign<T extends object, K extends keyof T>(target: T, key: K, value: T[K] | undefined): void {
  if (value !== undefined) {
    target[key] = value;
  }
}