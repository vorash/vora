import { decodeText, ndjson } from "./codec.js";
import {
  ConflictError,
  GatewayTimeoutError,
  PoolDrainingError,
  PoolNotLoadedError,
  PoolNotOnRunnerError,
  PoolRunError,
  PoolSpecConflictError,
  RateLimitedError,
  RequestTooLargeError,
  UnavailableError,
  VoraError,
} from "./errors.js";
import { fetchFailure, isAbort, retrying } from "./http.js";
import type { RetryPolicy, Transport } from "./http.js";
import {
  decodePoolInfo,
  decodeRunChunk,
  decodeRunError,
  decodeSandboxEvent,
  decodeSandboxExec,
  encodeExecRequest,
  encodePoolSpec,
} from "./types.js";
import type {
  DeclarePoolOptions,
  PoolInfo,
  PoolRun,
  PoolRunCallOptions,
  PoolRunDone,
  PoolRunOptions,
  PoolRunResult,
  PoolStats,
  RunChunk,
  RunExit,
  RunStreamError,
  SandboxEvent,
  SandboxExec,
} from "./types.js";

// The API reads this much of a body and parses what it got.
const RUN_BODY_LIMIT = 1 << 20;
const DRAIN_POLL_MS = 1000;

export interface TenantOptions {
  /** A global admin must name the tenant. */
  tenantId?: string;
}

export interface DeletePoolOptions {
  /** How long to keep repeating the delete. Default 600. */
  maxWaitSeconds?: number;
  signal?: AbortSignal;
}

function poolPath(name: string, suffix = "", tenantId?: string): string {
  const query = tenantId === undefined ? "" : `?${new URLSearchParams({ tenant_id: tenantId }).toString()}`;
  return `/v1/pools/${encodeURIComponent(name)}${suffix}${query}`;
}

/** Say which 409 this is from the pool's state, not from the error's words. */
async function namedConflict(
  transport: Transport,
  name: string,
  tenantId: string | undefined,
  error: ConflictError,
  declaring: boolean,
): Promise<ConflictError> {
  let info: PoolInfo;
  try {
    info = decodePoolInfo(await transport.json<unknown>(poolPath(name, "", tenantId)));
  } catch {
    return error;
  }
  const options = { status: error.status, body: error.body };
  switch (info.state) {
    case "POOL_DRAINING":
      return new PoolDrainingError(error.message, options);
    case "POOL_NOT_LOADED":
      return new PoolNotLoadedError(error.message, { ...options, reason: info.notLoadedReason });
    case "POOL_ACTIVE":
      return declaring ? new PoolSpecConflictError(error.message, options) : error;
    case "POOL_NOT_ON_RUNNER":
      return declaring ? error : new PoolNotOnRunnerError(error.message, options);
    default:
      return error;
  }
}

/**
 * Warm pools, reached through `vora.pools`. `declare`, `get`, `list`, `refresh`,
 * `stats`, `execs` and `events` are safe to repeat, so they retry 429 and 503 under the
 * client's `retry` policy. `delete` rides out 429, 503 and 504 for up to
 * `maxWaitSeconds`, whatever the policy.
 */
export class Pools {
  private readonly transport: Transport;
  private readonly retry: RetryPolicy | null;

  constructor(transport: Transport, retry: RetryPolicy | null) {
    this.transport = transport;
    this.retry = retry;
  }

  /**
   * Declare a pool, returning a handle to it. Idempotent for an equal spec.
   * `size: 0` starts every run cold. `reuse: "unsafe_no_reset"` needs an admin key
   * and a runner that grants it. A global admin must pass `tenantId`. A pool the runner
   * lost is created again under a new id, so read `id` from what this returns.
   *
   * Throws `PoolSpecConflictError` when the name has a different spec,
   * `PoolDrainingError` while a pool of the name is being deleted, `PoolNotLoadedError`
   * when the runner refused to load it, and `VoraError` with status 400 for a spec the
   * API refuses, or 404 when another agent of the tenant owns the name.
   */
  async declare(name: string, options: DeclarePoolOptions): Promise<Pool> {
    const spec = encodePoolSpec(name, options);
    try {
      const raw = await retrying(this.retry, () =>
        this.transport.json<unknown>(poolPath(name), { method: "PUT", json: spec }),
      );
      return new Pool(this.transport, this.retry, name, decodePoolInfo(raw), options.tenantId);
    } catch (error) {
      if (error instanceof ConflictError) {
        throw await namedConflict(this.transport, name, options.tenantId, error, true);
      }
      throw error;
    }
  }

  async get(name: string, options: TenantOptions = {}): Promise<Pool> {
    const raw = await retrying(this.retry, () =>
      this.transport.json<unknown>(poolPath(name, "", options.tenantId)),
    );
    return new Pool(this.transport, this.retry, name, decodePoolInfo(raw), options.tenantId);
  }

  /** Every pool the key may see. Read each `info.state`: see `PoolInfo`. */
  async list(options: TenantOptions = {}): Promise<Pool[]> {
    const query =
      options.tenantId === undefined ? "" : `?${new URLSearchParams({ tenant_id: options.tenantId }).toString()}`;
    const raw = await retrying(this.retry, () =>
      this.transport.json<{ pools?: unknown[] }>(`/v1/pools${query}`),
    );
    return (raw.pools ?? []).map((item) => {
      const info = decodePoolInfo(item);
      // A global admin may list every tenant, and its handles must name their own.
      return new Pool(this.transport, this.retry, info.spec?.name ?? "", info, options.tenantId ?? info.spec?.tenantId);
    });
  }
}

/** A handle to one pool. `info` is the last snapshot. */
export class Pool {
  readonly name: string;
  /** The pool's record as of the last call. */
  info: PoolInfo;

  private readonly transport: Transport;
  private readonly retry: RetryPolicy | null;
  private readonly tenantId: string | undefined;

  constructor(
    transport: Transport,
    retry: RetryPolicy | null,
    name: string,
    info: PoolInfo,
    tenantId: string | undefined,
  ) {
    this.transport = transport;
    this.retry = retry;
    this.name = name;
    this.info = info;
    this.tenantId = tenantId;
  }

  /** The id as of the last call. A handle reaches its pool by name, not by this. */
  get id(): string {
    return this.info.id;
  }

  async refresh(): Promise<PoolInfo> {
    this.info = decodePoolInfo(await retrying(this.retry, () => this.transport.json<unknown>(this.path())));
    return this.info;
  }

  /** Live stats, or undefined when the API's answer has none. Read `info.state` too. */
  async stats(): Promise<PoolStats | undefined> {
    return (await this.refresh()).stats;
  }

  /**
   * Run a command and collect its output, including what its reset found. Takes what
   * `streamRun` takes. Throws `PoolRunError`, with the output so far, when the stream
   * fails before `exit`.
   */
  async run(command: string, options: PoolRunCallOptions): Promise<PoolRunResult>;
  async run(options: PoolRunOptions): Promise<PoolRunResult>;
  async run(commandOrOptions: string | PoolRunOptions, options?: PoolRunCallOptions): Promise<PoolRunResult> {
    const stdout: Uint8Array[] = [];
    const stderr: Uint8Array[] = [];
    let run: PoolRun | undefined;
    let exit: RunExit | undefined;
    let done: PoolRunDone | undefined;
    let streamError: RunStreamError | undefined;
    try {
      for await (const chunk of this.stream(toRunOptions(commandOrOptions, options))) {
        switch (chunk.type) {
          case "run":
            run = chunk.run;
            break;
          case "stdout":
            stdout.push(chunk.data);
            break;
          case "stderr":
            stderr.push(chunk.data);
            break;
          case "exit":
            exit = chunk.exit;
            break;
          case "runDone":
            done = chunk.done;
            break;
          case "streamError":
            streamError = chunk.error;
            break;
        }
      }
    } catch (error) {
      if (error instanceof PoolRunError) {
        error.stdout = decodeText(concat(stdout));
        error.stderr = decodeText(concat(stderr));
      }
      throw error;
    }
    if (run === undefined || exit === undefined) {
      throw new PoolRunError("stream_ended", "pool run: the stream had no run frame");
    }
    const result: PoolRunResult = { run, exit, stdout: decodeText(concat(stdout)), stderr: decodeText(concat(stderr)) };
    if (done !== undefined) result.done = done;
    if (streamError !== undefined) result.streamError = streamError;
    return result;
  }

  /**
   * Run a command, yielding `run`, output, `exit`, then `runDone`. A string is a
   * shell line run via `/bin/sh -c`. A run with `exit` is complete; `runDone` reports
   * the reset that follows. Leaving the loop before `exit` cancels the run. A client
   * reading slower than about 1.5 KiB/s while output is flowing is cut off with
   * `client_stalled`.
   *
   * `retry` repeats a 429 or 503, which come before the first frame. A 429 means nothing
   * ran. A 503 usually means the same, but a runner lost just after the program started
   * also answers 503, so a retry can run the program twice. Use it only for a program
   * that is safe to repeat. The default is not to retry. A failure after the first frame
   * is never retried.
   *
   * Throws `RateLimitedError` when the pool is full, `UnavailableError` when no sandbox
   * could start, `GatewayTimeoutError` when no first frame came within the API's 7
   * minutes, `PoolDrainingError`, `PoolNotLoadedError`, `PoolNotOnRunnerError`,
   * `RequestTooLargeError` for a body over the API's 1 MiB limit, and `PoolRunError` for
   * a failure after the first frame and before `exit`.
   */
  streamRun(command: string, options: PoolRunCallOptions): AsyncIterable<RunChunk>;
  streamRun(options: PoolRunOptions): AsyncIterable<RunChunk>;
  streamRun(commandOrOptions: string | PoolRunOptions, options?: PoolRunCallOptions): AsyncIterable<RunChunk> {
    return this.stream(toRunOptions(commandOrOptions, options));
  }

  /** The audit trail of one of this pool's sandboxes. */
  async execs(sandboxId: string): Promise<SandboxExec[]> {
    const path = this.path(`/sandboxes/${encodeURIComponent(sandboxId)}/execs`);
    const raw = await retrying(this.retry, () => this.transport.json<{ execs?: unknown[] }>(path));
    return (raw.execs ?? []).map(decodeSandboxExec);
  }

  /** The lifecycle and reset trail of one of this pool's sandboxes. */
  async events(sandboxId: string): Promise<SandboxEvent[]> {
    const path = this.path(`/sandboxes/${encodeURIComponent(sandboxId)}/events`);
    const raw = await retrying(this.retry, () => this.transport.json<{ events?: unknown[] }>(path));
    return (raw.events ?? []).map(decodeSandboxEvent);
  }

  /**
   * Drain and delete the pool, returning once it is gone. Idempotent. Each call to
   * the API waits up to about 25 s for running commands to finish, and the wait is
   * repeated until the pool is gone or `maxWaitSeconds` has passed, which throws
   * `PoolDrainingError`. A runner that is down, or whose row the API no longer has, is
   * waited out the same way, and its last refusal is thrown. Runs in progress finish.
   * Calling `delete` again picks up the same drain. A pool its runner lost has nothing to
   * drain.
   */
  async delete(options: DeletePoolOptions = {}): Promise<void> {
    const sleep = this.retry?.sleep ?? defaultSleep;
    const maxWaitSeconds = options.maxWaitSeconds ?? 600;
    const deadline = Date.now() + maxWaitSeconds * 1000;
    for (;;) {
      let delayMs = DRAIN_POLL_MS;
      try {
        const response = await this.transport.request(this.path(), { method: "DELETE", signal: options.signal });
        if (response.status !== 202) {
          return;
        }
        if (Date.now() + delayMs > deadline) {
          throw new PoolDrainingError(`pool ${this.name} is still draining after ${maxWaitSeconds} s; call delete again`, {
            status: 202,
          });
        }
      } catch (error) {
        if (error instanceof VoraError && error.status === 404) {
          return;
        }
        if (
          !(error instanceof RateLimitedError || error instanceof UnavailableError || error instanceof GatewayTimeoutError)
        ) {
          throw error;
        }
        delayMs = (error.retryAfterSeconds ?? DRAIN_POLL_MS / 1000) * 1000;
        if (Date.now() + delayMs > deadline) {
          throw error;
        }
      }
      await sleep(delayMs);
    }
  }

  private path(suffix = ""): string {
    return poolPath(this.name, suffix, this.tenantId);
  }

  private async open(options: PoolRunOptions): Promise<Response> {
    const body = JSON.stringify(encodeExecRequest(options));
    const size = new TextEncoder().encode(body).byteLength;
    if (size > RUN_BODY_LIMIT) {
      throw new RequestTooLargeError(
        `pool run: the encoded request is ${size} bytes, and the API takes ${RUN_BODY_LIMIT}. ` +
          "stdin is base64 inside it, so about 768 KiB of stdin fits.",
      );
    }
    try {
      return await retrying(options.retry ?? null, () =>
        this.transport.request(this.path("/run"), {
          method: "POST",
          body,
          contentType: "application/json",
          signal: options.signal,
        }),
      );
    } catch (error) {
      if (error instanceof ConflictError) {
        throw await namedConflict(this.transport, this.name, this.tenantId, error, false);
      }
      throw error;
    }
  }

  private async *stream(options: PoolRunOptions): AsyncGenerator<RunChunk> {
    const response = await this.open(options);

    // Past the status line the command may have run, so nothing below retries.
    let run: PoolRun | undefined;
    let sawExit = false;
    let sawDone = false;
    let failure: RunStreamError | undefined;
    let cause: unknown;
    try {
      if (response.body !== null) {
        for await (const raw of ndjson<unknown>(response.body)) {
          failure = decodeRunError(raw);
          if (failure !== undefined) {
            break;
          }
          const chunk = decodeRunChunk(raw);
          if (chunk === undefined) {
            continue;
          }
          if (chunk.type === "run") {
            run = chunk.run;
          } else if (chunk.type === "exit") {
            sawExit = true;
          } else if (chunk.type === "runDone") {
            sawDone = true;
          }
          yield chunk;
        }
      }
    } catch (error) {
      // Past `exit` the command has finished, and a caller who retries on an abort would
      // run it again, so the result stands, as it does when the stream breaks.
      if (!sawExit && isAbort(error, options.signal)) {
        throw error;
      }
      cause = error;
      failure = {
        code: "stream_ended",
        message: `pool run: the stream broke: ${fetchFailure(error)}`,
      };
    } finally {
      // A plain `break` releases the reader without closing the connection, and only
      // closing it tells the API to cancel the run.
      await response.body?.cancel().catch(() => undefined);
    }

    if (sawExit && sawDone) {
      return;
    }
    const final = failure ?? { code: "stream_ended", message: "pool run: the stream ended early" };
    if (sawExit) {
      yield { type: "streamError", error: final };
      return;
    }
    throw new PoolRunError(final.code, final.message, { run, cause });
  }
}

function defaultSleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function toRunOptions(commandOrOptions: string | PoolRunOptions, options?: PoolRunCallOptions): PoolRunOptions {
  // A bare string is a shell command line, as in `Sandbox.exec`.
  const merged =
    typeof commandOrOptions === "string"
      ? ({ command: "sh", args: ["-c", commandOrOptions], ...options } as PoolRunOptions)
      : commandOrOptions;
  if (typeof merged.timeoutSeconds !== "number") {
    throw new VoraError("pool run: timeoutSeconds is required");
  }
  return merged;
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
