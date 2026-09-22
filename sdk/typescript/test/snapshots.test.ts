import { afterEach, describe, expect, it } from "vitest";

import { Vora } from "../src/index.js";
import { b64, lastRequest, sandboxJSON, startAPI } from "./helpers.js";
import type { FakeAPI, RecordedRequest } from "./helpers.js";

const open: FakeAPI[] = [];

afterEach(async () => {
  await Promise.all(open.splice(0).map((api) => api.close()));
});

async function boot(handler: Parameters<typeof startAPI>[0]): Promise<FakeAPI> {
  const api = await startAPI(handler);
  open.push(api);
  return api;
}

function snapshotJSON(): Record<string, unknown> {
  return {
    id: "snap_01TEST",
    sandboxId: "sbx_01TEST",
    tenantId: "t1",
    runnerId: "run_01TEST",
    spec: { sandboxId: "sbx_01TEST", image: "vora/python", ttlSeconds: "1800" },
    bytes: "18432",
    createdAtUnixMs: "1700000000000",
  };
}

function decode(request: RecordedRequest | undefined): Record<string, unknown> {
  const raw = request?.body.toString("utf8") ?? "";
  return raw === "" ? {} : (JSON.parse(raw) as Record<string, unknown>);
}

describe("snapshots", () => {
  it("creates a snapshot for a sandbox", async () => {
    const api = await boot(() => ({ status: 201, json: snapshotJSON() }));
    const vora = new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" });

    const snap = await vora.snapshots.create("sbx_01TEST");

    expect(lastRequest(api, "POST")?.url).toBe("/v1/sandboxes/sbx_01TEST/snapshot");
    // protojson writes int64 as strings; the SDK surfaces numbers.
    expect(snap.id).toBe("snap_01TEST");
    expect(snap.info.bytes).toBe(18432);
    expect(snap.info.sandboxId).toBe("sbx_01TEST");
  });

  it("lists, gets and removes snapshots", async () => {
    const api = await boot((request) => {
      if (request.method === "GET" && request.url === "/v1/snapshots") {
        return { json: { snapshots: [snapshotJSON()] } };
      }
      if (request.method === "GET") {
        return { json: snapshotJSON() };
      }
      return { status: 200, json: {} };
    });
    const vora = new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" });

    const snaps = await vora.snapshots.list();
    expect(snaps).toHaveLength(1);
    expect(snaps[0]?.id).toBe("snap_01TEST");

    const one = await vora.snapshots.get("snap_01TEST");
    expect(one.info.bytes).toBe(18432);

    await one.remove();
    expect(lastRequest(api, "DELETE")?.url).toBe("/v1/snapshots/snap_01TEST");
  });

  it("forks a snapshot into a fresh sandbox", async () => {
    const api = await boot((request) => {
      if (request.url?.endsWith("/fork") === true) {
        return { status: 201, json: sandboxJSON({ id: "sbx_01FORK" }) };
      }
      return { json: snapshotJSON() };
    });
    const vora = new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" });

    const snap = await vora.snapshots.get("snap_01TEST");
    const fork = await snap.fork();

    const request = lastRequest(api, "POST");
    expect(request?.url).toBe("/v1/snapshots/snap_01TEST/fork");
    expect(decode(request)).toEqual({});
    expect(fork.id).toBe("sbx_01FORK");
    expect(fork.info.state).toBe("RUNNING");
  });

  it("propagates API errors as VoraError", async () => {
    const api = await boot(() => ({ status: 404, json: { error: "snapshot snap_gone not found" } }));
    const vora = new Vora({ baseUrl: api.baseUrl, apiKey: "vora_test" });

    await expect(vora.snapshots.get("snap_gone")).rejects.toMatchObject({
      name: "VoraError",
      status: 404,
    });
  });

});
