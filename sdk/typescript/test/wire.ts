import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { IncomingHttpHeaders, IncomingMessage, Server, ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

export interface Scenario {
  status: number;
  retryAfter?: string;
  body?: string;
  lines?: string[];
}

interface Golden {
  limits: Record<string, number>;
  runs: Record<string, Scenario>;
  answers: Record<string, Scenario>;
  messages: Record<string, Record<string, unknown>>;
}

/** The bytes the real API sends, written by internal/api's TestSDKWireGolden. */
export const GOLDEN = JSON.parse(
  readFileSync(new URL("../../testdata/pool_wire.json", import.meta.url), "utf8"),
) as Golden;

export interface Sent {
  method: string;
  url: string;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

export interface Reply extends Scenario {
  /** Split the stream's bytes into pieces this long, so a frame crosses reads. */
  piece?: number;
  /** Cut the connection after this many lines. */
  cutAfter?: number;
  /** Answer these lines, then wait for the client to leave. */
  hold?: boolean;
}

export function runReply(name: string, overrides: Partial<Reply> = {}): Reply {
  const scenario = GOLDEN.runs[name];
  if (scenario === undefined) {
    throw new Error(`no run scenario ${name}`);
  }
  return { ...scenario, ...overrides };
}

export function answerReply(name: string): Reply {
  const scenario = GOLDEN.answers[name];
  if (scenario === undefined) {
    throw new Error(`no answer scenario ${name}`);
  }
  return scenario;
}

export interface WireServer {
  url: string;
  requests: Sent[];
  /** Resolves when a held response's client disconnects. */
  clientLeft: Promise<void>;
  close(): Promise<void>;
}

function delay(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

async function write(reply: Reply, res: ServerResponse, onLeft: () => void): Promise<void> {
  res.statusCode = reply.status;
  if (reply.retryAfter !== undefined) {
    res.setHeader("Retry-After", reply.retryAfter);
  }
  if (reply.lines === undefined) {
    res.setHeader("Content-Type", "application/json");
    res.end(reply.body ?? "");
    return;
  }
  res.setHeader("Content-Type", "application/x-ndjson");
  res.flushHeaders();
  const lines = reply.cutAfter === undefined ? reply.lines : reply.lines.slice(0, reply.cutAfter);
  const stream = Buffer.from(lines.map((line) => `${line}\n`).join(""));
  const piece = reply.piece ?? 7;
  for (let start = 0; start < stream.length; start += piece) {
    res.write(stream.subarray(start, start + piece));
    await delay();
  }
  if (reply.hold === true) {
    res.on("close", onLeft);
    return;
  }
  if (reply.cutAfter !== undefined) {
    res.destroy();
    return;
  }
  res.end();
}

export async function startWire(route: (sent: Sent) => Reply): Promise<WireServer> {
  const requests: Sent[] = [];
  let left: () => void = () => undefined;
  const clientLeft = new Promise<void>((resolve) => {
    left = resolve;
  });
  const server: Server = createServer((req: IncomingMessage, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const sent: Sent = {
        method: req.method ?? "GET",
        url: req.url ?? "/",
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(sent);
      void write(route(sent), res, left);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    clientLeft,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}
