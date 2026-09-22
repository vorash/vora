import { TypeSafeClient } from "@typesafe-ai/sdk";
import type { Fetch } from "@typesafe-ai/sdk";

import { JevDecisionClient } from "../src/index.js";
import type { JevDecisionClientOptions } from "../src/index.js";

export interface RecordedRequest {
  url: string;
  headers: Record<string, string>;
  body: Record<string, unknown>;
}

export interface FakeFetch {
  fetch: Fetch;
  requests: RecordedRequest[];
}

export function fakeFetch(
  respond: (body: Record<string, unknown>) => { status?: number; json: unknown },
): FakeFetch {
  const requests: RecordedRequest[] = [];
  const fetch: Fetch = async (input, init) => {
    const raw = init?.body === undefined ? "{}" : String(init.body);
    const body = JSON.parse(raw) as Record<string, unknown>;
    requests.push({
      url: String(input),
      headers: Object.fromEntries(new Headers(init?.headers).entries()),
      body,
    });
    const { status = 200, json } = respond(body);
    return new Response(JSON.stringify(json), {
      status,
      headers: { "content-type": "application/json" },
    });
  };
  return { fetch, requests };
}

export function firstRequest(fake: FakeFetch): RecordedRequest {
  const request = fake.requests[0];
  if (request === undefined) {
    throw new Error("no request was recorded");
  }
  return request;
}

export function clientFor(
  fake: FakeFetch,
  options: Partial<JevDecisionClientOptions> = {},
): JevDecisionClient {
  return new JevDecisionClient({
    client: new TypeSafeClient({
      apiKey: "jev_test",
      baseURL: "https://api.typesafe.ai",
      fetch: fake.fetch,
      retry: { maxRetries: 0 },
    }),
    ...options,
  });
}

export const usage = { input_tokens: 120, output_tokens: 30 };
