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

const SANDBOX = {
  id: "sbx_test",
  state: "RUNNING",
  spec: { image: "vora/python", resources: { cpuMillis: "1000", memoryBytes: "536870912" } },
  createdAtUnixMs: "1758000000000",
};

export function b64(value: string): string {
  return Buffer.from(value, "utf8").toString("base64");
}

/** A stand-in vora-api that speaks the same protojson shapes as the server. */
export async function startAPI(): Promise<FakeAPI> {
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
        json(201, SANDBOX);
        return;
      }
      if (req.method === "POST" && url.endsWith("/exec")) {
        res.setHeader("Content-Type", "application/x-ndjson");
        res.write(`${JSON.stringify({ stdout: b64("hello from sandbox\n") })}\n`);
        res.end(`${JSON.stringify({ exit: { exitCode: 0, reason: "EXITED" } })}\n`);
        return;
      }
      if (req.method === "GET" && url.includes("/files")) {
        res.setHeader("Content-Type", "application/octet-stream");
        res.end(Buffer.from("file contents", "utf8"));
        return;
      }
      if (req.method === "PUT" && url.includes("/files")) {
        json(200, { path: "/workspace/a.txt", bytes_written: 5 });
        return;
      }
      if (req.method === "GET" && url.endsWith("/stats")) {
        json(200, { memoryBytes: "1234", memoryPeakBytes: "2345", cpuMillis: "99", pids: "3", oomKills: "1" });
        return;
      }
      if (req.method === "DELETE") {
        res.statusCode = 204;
        res.end();
        return;
      }
      if (req.method === "GET" && url === "/v1/sandboxes/sbx_missing") {
        json(404, { error: "sandbox sbx_missing not found" });
        return;
      }
      if (req.method === "GET" && /^\/v1\/sandboxes\/[^/?]+$/.test(url)) {
        json(200, SANDBOX);
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