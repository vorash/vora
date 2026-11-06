import { ConflictError, GatewayTimeoutError, RateLimitedError, UnavailableError, VoraError } from "./errors.js";

export interface TransportOptions {
  baseUrl: string;
  apiKey?: string | undefined;
  fetch?: typeof globalThis.fetch | undefined;
}

export interface RequestOptions {
  method?: string;
  body?: BodyInit | undefined;
  contentType?: string | undefined;
  signal?: AbortSignal | undefined;
}

/**
 * How to repeat a call the API refused with 429 or 503. A `Retry-After` longer than
 * `maxDelayMs` throws instead of waiting, so the whole wait stays under
 * `maxAttempts * maxDelayMs`.
 */
export interface RetryPolicy {
  /** Attempts in total, the first included. Default 4. */
  maxAttempts?: number;
  /** Before the first retry when the API names no wait; doubles each time. Default 250. */
  baseDelayMs?: number;
  /** Default 10000. */
  maxDelayMs?: number;
  sleep?: (ms: number) => Promise<void>;
}

/** Run `call`, repeating it on 429 and 503 as `policy` allows. */
export async function retrying<T>(policy: RetryPolicy | null, call: () => Promise<T>): Promise<T> {
  const maxAttempts = policy?.maxAttempts ?? 4;
  const baseDelayMs = policy?.baseDelayMs ?? 250;
  const maxDelayMs = policy?.maxDelayMs ?? 10000;
  const sleep = policy?.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
  for (let attempt = 1; ; attempt += 1) {
    try {
      return await call();
    } catch (error) {
      if (!(error instanceof RateLimitedError || error instanceof UnavailableError)) {
        throw error;
      }
      if (policy === null || attempt >= maxAttempts) {
        throw error;
      }
      const delayMs = retryDelayMs(error.retryAfterSeconds, attempt, baseDelayMs, maxDelayMs);
      if (delayMs === undefined) {
        throw error;
      }
      await sleep(delayMs);
    }
  }
}

function retryDelayMs(
  retryAfterSeconds: number | undefined,
  attempt: number,
  baseDelayMs: number,
  maxDelayMs: number,
): number | undefined {
  if (retryAfterSeconds !== undefined) {
    const asked = retryAfterSeconds * 1000;
    if (asked > maxDelayMs) {
      return undefined;
    }
    // Added, never subtracted: many callers refused together must not all return at
    // the same instant, and none may come back before the API asked.
    return asked + Math.random() * baseDelayMs;
  }
  return Math.min(baseDelayMs * 2 ** (attempt - 1), maxDelayMs);
}

/** Thin HTTP layer: base URL, bearer auth, and error normalisation. */
export class Transport {
  readonly baseUrl: string;

  private readonly apiKey: string | undefined;
  private readonly fetchImpl: typeof globalThis.fetch;

  constructor(options: TransportOptions) {
    this.baseUrl = options.baseUrl.replace(/\/+$/, "");
    this.apiKey = options.apiKey;
    this.fetchImpl = options.fetch ?? globalThis.fetch;
    if (typeof this.fetchImpl !== "function") {
      throw new VoraError("fetch is unavailable; pass VoraOptions.fetch on Node < 18");
    }
  }

  /** Perform a request, throwing a `VoraError` on any non-2xx response. */
  async request(path: string, options: RequestOptions = {}): Promise<Response> {
    const headers: Record<string, string> = {};
    if (this.apiKey !== undefined && this.apiKey !== "") {
      headers.Authorization = `Bearer ${this.apiKey}`;
    }
    if (options.contentType !== undefined) {
      headers["Content-Type"] = options.contentType;
    }
    const response = await this.fetchImpl(`${this.baseUrl}${path}`, {
      method: options.method ?? "GET",
      headers,
      body: options.body,
      signal: options.signal,
    });
    if (!response.ok) {
      throw await toVoraError(response);
    }
    return response;
  }

  async json<T>(path: string, options: JsonRequestOptions = {}): Promise<T> {
    const body = options.json === undefined ? undefined : JSON.stringify(options.json);
    const response = await this.request(path, {
      method: options.method,
      body,
      contentType: body === undefined ? undefined : "application/json",
      signal: options.signal,
    });
    return (await response.json()) as T;
  }

  async bytes(path: string, options: RequestOptions = {}): Promise<Uint8Array> {
    const response = await this.request(path, options);
    return new Uint8Array(await response.arrayBuffer());
  }
}

export interface JsonRequestOptions {
  method?: string;
  json?: unknown;
  signal?: AbortSignal | undefined;
}

async function toVoraError(response: Response): Promise<VoraError> {
  const text = await response.text().catch(() => "");
  let message = `${response.status} ${response.statusText}`.trim();
  let body: unknown = text === "" ? undefined : text;
  if (text !== "") {
    try {
      const parsed: unknown = JSON.parse(text);
      body = parsed;
      const detail = (parsed as { error?: unknown }).error;
      if (typeof detail === "string" && detail !== "") {
        message = detail;
      }
    } catch {
      message = text.slice(0, 300);
    }
  }
  const options = { status: response.status, body };
  const retryAfterSeconds = parseRetryAfter(response.headers.get("retry-after"));
  switch (response.status) {
    case 429:
      return new RateLimitedError(message, { ...options, retryAfterSeconds });
    case 503:
      return new UnavailableError(message, { ...options, retryAfterSeconds });
    case 504:
      return new GatewayTimeoutError(message, { ...options, retryAfterSeconds });
    case 409:
      return new ConflictError(message, options);
    default:
      return new VoraError(message, options);
  }
}

/** Seconds, or an HTTP date from a proxy in front of the API. */
export function parseRetryAfter(value: string | null): number | undefined {
  if (value === null || value.trim() === "") {
    return undefined;
  }
  const seconds = Number(value);
  if (Number.isFinite(seconds)) {
    return Math.max(seconds, 0);
  }
  const when = Date.parse(value);
  return Number.isNaN(when) ? undefined : Math.max((when - Date.now()) / 1000, 0);
}
