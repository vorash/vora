import { VoraError } from "./errors.js";

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
  return new VoraError(message, { status: response.status, body });
}