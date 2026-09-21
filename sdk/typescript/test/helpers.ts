import { createServer } from "node:http";
import type { IncomingHttpHeaders, IncomingMessage, Server } from "node:http";
import type { AddressInfo } from "node:net";

export interface RecordedRequest {
  method: string;
  url: string;
  headers: IncomingHttpHeaders;
  body: import("node:buffer").Buffer;
}

export interface FakeResponse {
  status?: number;
  contentType?: string;
  /** JSON body (serialised, `application/json` unless overridden). */
  json?: unknown;
  /** Raw text body. */
  text?: string;
  /** Raw bytes body. */
  bytes?: Uint8Array;
  /** One JSON object per line, streamed as NDJSON. */
  ndjson?: unknown[];
}

export type FakeHandler = (request: RecordedRequest) => FakeResponse | undefined;

export interface FakeAPI {
  baseUrl: string;
  requests: RecordedRequest[];
  close(): Promise<void>;
}

/**
 * Start a throwaway vora-api stand-in that records every request. A handler may
 * return `undefined` to fall back to the default route behaviour, which answers
 * `GET /v1/sandboxes/{id}` with a running sandbox record.
 */
export async function startAPI(handler: FakeHandler): Promise<FakeAPI> {
  const requests: RecordedRequest[] = [];
  const server: Server = createServer((req: IncomingMessage, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const recorded: RecordedRequest = {
        method: req.method ?? "GET",
        url: req.url ?? "/",
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(recorded);
      const response = handler(recorded) ?? defaultResponse(recorded);
      res.statusCode = response.status ?? 200;
      if (response.ndjson !== undefined) {
        res.setHeader("Content-Type", "application/x-ndjson");
        for (const item of response.ndjson) {
          res.write(`${JSON.stringify(item)}\n`);
        }
        res.end();
        return;
      }
      if (response.bytes !== undefined) {
        res.setHeader("Content-Type", response.contentType ?? "application/octet-stream");
        res.end(Buffer.from(response.bytes));
        return;
      }
      if (response.json !== undefined) {
        res.setHeader("Content-Type", response.contentType ?? "application/json");
        res.end(JSON.stringify(response.json));
        return;
      }
      res.setHeader("Content-Type", response.contentType ?? "text/plain");
      res.end(response.text ?? "");
    });
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as AddressInfo;
  return {
    baseUrl: `http://127.0.0.1:${address.port}`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
      }),
  };
}

const SANDBOX_PATH = /^\/v1\/sandboxes\/([^/?]+)$/;

function defaultResponse(request: RecordedRequest): FakeResponse {
  if (request.method === "GET") {
    const match = SANDBOX_PATH.exec(request.url);
    if (match !== null) {
      return { json: sandboxJSON({ id: decodeURIComponent(match[1] ?? "") }) };
    }
  }
  return { status: 404, json: { error: `no route for ${request.method} ${request.url}` } };
}

/** The most recent request matching a method (and optionally an exact URL). */
export function lastRequest(api: FakeAPI, method: string, url?: string): RecordedRequest | undefined {
  return api.requests
    .filter((request) => request.method === method && (url === undefined || request.url === url))
    .at(-1);
}

/** protojson encodes int64 as a string; a realistic sandbox record. */
export function sandboxJSON(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "sbx_01M2NVHE1KRD8JT3D6QG6R0EW3",
    spec: {
      image: "vora/python",
      backend: "bwrap",
      resources: { cpuMillis: "1000", memoryBytes: "536870912", pids: "128" },
      network: { mode: "NETWORK_NONE" },
      ttlSeconds: "3600",
    },
    state: "RUNNING",
    runnerId: "runner-1",
    createdAtUnixMs: "1758000000000",
    updatedAtUnixMs: "1758000000001",
    imageRootfs: "/var/lib/vora/rootfs/sbx_01M2NVHE1KRD8JT3D6QG6R0EW3",
    ...overrides,
  };
}

/** base64 of a UTF-8 string, as protojson would encode a `bytes` field. */
export function b64(value: string): string {
  return Buffer.from(value, "utf8").toString("base64");
}