import { createServer } from "node:http";
import type { IncomingMessage, Server } from "node:http";
import type { AddressInfo } from "node:net";

export interface RecordedRequest {
  method: string;
  url: string;
  body: string;
}

export interface FakeAPI {
  baseUrl: string;
  requests: RecordedRequest[];
  close(): Promise<void>;
}

export interface FakeOptions {
  /** Replace the POST /v1/sandboxes response (status + JSON body). */
  create?: { status: number; json: unknown };
  /** Replace the exec response. */
  exec?: { status: number; ndjson?: unknown[]; json?: unknown };
}

export function b64(value: string): string {
  return Buffer.from(value, "utf8").toString("base64");
}

export function execChunks(stdout: string, stderr = "", exitCode = 0): unknown[] {
  const chunks: unknown[] = [];
  if (stdout !== "") {
    chunks.push({ stdout: b64(stdout) });
  }
  if (stderr !== "") {
    chunks.push({ stderr: b64(stderr) });
  }
  chunks.push({ exit: { exitCode, reason: "EXITED" } });
  return chunks;
}

export async function startAPI(options: FakeOptions = {}): Promise<FakeAPI> {
  const requests: RecordedRequest[] = [];
  const server: Server = createServer((req: IncomingMessage, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const url = req.url ?? "/";
      requests.push({ method: req.method ?? "GET", url, body: Buffer.concat(chunks).toString("utf8") });

      const json = (status: number, payload: unknown): void => {
        res.statusCode = status;
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify(payload));
      };

      if (req.method === "POST" && url === "/v1/sandboxes") {
        if (options.create !== undefined) {
          json(options.create.status, options.create.json);
          return;
        }
        json(201, { id: "sbx_agent", state: "RUNNING", spec: { image: "vora/python" } });
        return;
      }
      if (req.method === "POST" && url.endsWith("/exec")) {
        if (options.exec !== undefined && options.exec.status !== 200) {
          json(options.exec.status, options.exec.json ?? { error: "exec failed" });
          return;
        }
        res.setHeader("Content-Type", "application/x-ndjson");
        const ndjson = options.exec?.ndjson ?? execChunks("42\n");
        for (const chunk of ndjson) {
          res.write(`${JSON.stringify(chunk)}\n`);
        }
        res.end();
        return;
      }
      if (req.method === "DELETE") {
        res.statusCode = 204;
        res.end();
        return;
      }
      if (req.method === "GET" && /^\/v1\/sandboxes\/[^/?]+$/.test(url)) {
        json(200, { id: url.split("/").pop(), state: "RUNNING" });
        return;
      }
      json(404, { error: `no route for ${req.method} ${url}` });
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