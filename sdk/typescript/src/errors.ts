import type { PoolRun } from "./types.js";

export interface VoraErrorOptions {
  status?: number;
  body?: unknown;
  cause?: unknown;
}

/** A failed Vora API call, or a malformed request built by this SDK. */
export class VoraError extends Error {
  /** HTTP status, when the failure came from the API. */
  readonly status?: number;
  /** Parsed response body (`{"error": "..."}`), when the API returned one. */
  readonly body?: unknown;

  constructor(message: string, options: VoraErrorOptions = {}) {
    super(message, options.cause === undefined ? undefined : { cause: options.cause });
    this.name = "VoraError";
    this.status = options.status;
    this.body = options.body;
  }
}

export interface RetryLaterErrorOptions extends VoraErrorOptions {
  retryAfterSeconds?: number | undefined;
}

/** The API refused for now and may name a wait (`Retry-After`). */
export class RetryLaterError extends VoraError {
  /** Seconds to wait, when the API said. */
  readonly retryAfterSeconds?: number;

  constructor(message: string, options: RetryLaterErrorOptions = {}) {
    super(message, options);
    this.name = "RetryLaterError";
    if (options.retryAfterSeconds !== undefined) {
      this.retryAfterSeconds = options.retryAfterSeconds;
    }
  }
}

/** 429: a pool with no free sandbox, or capacity reached. Nothing ran. */
export class RateLimitedError extends RetryLaterError {
  constructor(message: string, options: RetryLaterErrorOptions = {}) {
    super(message, options);
    this.name = "RateLimitedError";
  }
}

/** 503: no sandbox could start, or a runner is down or still starting. Nothing ran. */
export class UnavailableError extends RetryLaterError {
  constructor(message: string, options: RetryLaterErrorOptions = {}) {
    super(message, options);
    this.name = "UnavailableError";
  }
}

/** 504: the runner did not answer in time. It is not known whether anything ran. */
export class GatewayTimeoutError extends RetryLaterError {
  constructor(message: string, options: RetryLaterErrorOptions = {}) {
    super(message, options);
    this.name = "GatewayTimeoutError";
  }
}

/** 409: the resource's state refuses this call. */
export class ConflictError extends VoraError {
  constructor(message: string, options: VoraErrorOptions = {}) {
    super(message, options);
    this.name = "ConflictError";
  }
}

/**
 * The pool is being deleted. Retry later, or declare it again once it is gone.
 * `delete()` also throws this, with status 202, when its wait runs out.
 */
export class PoolDrainingError extends ConflictError {
  constructor(message: string, options: VoraErrorOptions = {}) {
    super(message, options);
    this.name = "PoolDrainingError";
  }
}

/** The runner refused to load this pool at its last boot. Retrying won't help. */
export class PoolNotLoadedError extends ConflictError {
  /** `notLoadedReason` from the pool's record, for example `backend_missing`. */
  readonly reason?: string;

  constructor(message: string, options: VoraErrorOptions & { reason?: string | undefined } = {}) {
    super(message, options);
    this.name = "PoolNotLoadedError";
    if (options.reason !== undefined) {
      this.reason = options.reason;
    }
  }
}

/**
 * The pool's route holds its reservation and name, and its runner has no such pool.
 * It does not come back on its own, so retrying won't help. `declare` creates it again
 * under a new id, and `delete` releases the reservation.
 */
export class PoolNotOnRunnerError extends ConflictError {
  constructor(message: string, options: VoraErrorOptions = {}) {
    super(message, options);
    this.name = "PoolNotOnRunnerError";
  }
}

/** A live pool of this name has a different spec. Specs are immutable. */
export class PoolSpecConflictError extends ConflictError {
  constructor(message: string, options: VoraErrorOptions = {}) {
    super(message, options);
    this.name = "PoolSpecConflictError";
  }
}

/** A run's encoded body is over the API's 1 MiB limit. It was not sent. */
export class RequestTooLargeError extends VoraError {
  constructor(message: string) {
    super(message);
    this.name = "RequestTooLargeError";
  }
}

/**
 * A pool run that started and ended without an `exit` frame. The command may have
 * run, so retrying is the caller's decision. `code` is `client_stalled`,
 * `runner_unavailable` or `run_failed` from the API's last line, or `stream_ended`
 * when the connection ended with no such line.
 */
export class PoolRunError extends VoraError {
  readonly code: string;
  /** The run's first frame, when one arrived. */
  readonly run?: PoolRun;
  /** Output received before the failure. `Pool.run` fills it in. */
  stdout = "";
  /** Output received before the failure. `Pool.run` fills it in. */
  stderr = "";

  constructor(code: string, message: string, options: { run?: PoolRun | undefined; cause?: unknown } = {}) {
    super(message, { cause: options.cause });
    this.name = "PoolRunError";
    this.code = code;
    if (options.run !== undefined) {
      this.run = options.run;
    }
  }
}
