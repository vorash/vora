import type { Transport } from "./http.js";
import { Sandbox } from "./sandbox.js";
import { decodeSandboxInfo, decodeSnapshotInfo } from "./types.js";
import type { SandboxInfo, SnapshotInfo } from "./types.js";

/**
 * Snapshot operations, reached through `vora.snapshots`. A snapshot captures a
 * sandbox's writable state live and crash-consistent (ADR 023); forking it
 * starts independent sandboxes from that state.
 */
export class Snapshots {
  private readonly transport: Transport;

  constructor(transport: Transport) {
    this.transport = transport;
  }

  /** Capture a sandbox's writable state. The sandbox keeps running. */
  async create(sandboxId: string): Promise<Snapshot> {
    const raw = await this.transport.json<unknown>(
      `/v1/sandboxes/${encodeURIComponent(sandboxId)}/snapshot`,
      { method: "POST" },
    );
    return new Snapshot(this.transport, decodeSnapshotInfo(raw));
  }

  async get(id: string): Promise<Snapshot> {
    const raw = await this.transport.json<unknown>(`/v1/snapshots/${encodeURIComponent(id)}`);
    return new Snapshot(this.transport, decodeSnapshotInfo(raw));
  }

  async list(): Promise<Snapshot[]> {
    const raw = await this.transport.json<{ snapshots?: unknown[] }>("/v1/snapshots");
    return (raw.snapshots ?? []).map((item) => new Snapshot(this.transport, decodeSnapshotInfo(item)));
  }

  /** Delete a snapshot and its archive. Sandboxes forked from it are unaffected. */
  async remove(id: string): Promise<void> {
    await this.transport.request(`/v1/snapshots/${encodeURIComponent(id)}`, { method: "DELETE" });
  }

  /** Create and start a new sandbox from a snapshot. */
  async fork(id: string): Promise<Sandbox> {
    const raw = await this.transport.json<unknown>(`/v1/snapshots/${encodeURIComponent(id)}/fork`, {
      method: "POST",
    });
    return new Sandbox(this.transport, decodeSandboxInfo(raw));
  }
}

/** A handle to one snapshot. Methods hit the API; `info` is the last view. */
export class Snapshot {
  /** Latest observed record, refreshed by `refresh()`. */
  info: SnapshotInfo;

  private readonly transport: Transport;

  constructor(transport: Transport, info: SnapshotInfo) {
    this.transport = transport;
    this.info = info;
  }

  get id(): string {
    return this.info.id;
  }

  /** Re-read the snapshot record from the API. */
  async refresh(): Promise<SnapshotInfo> {
    const raw = await this.transport.json<unknown>(`/v1/snapshots/${this.pathId()}`);
    this.info = decodeSnapshotInfo(raw);
    return this.info;
  }

  /** Create and start a new sandbox from this snapshot. */
  async fork(): Promise<Sandbox> {
    const raw = await this.transport.json<unknown>(`/v1/snapshots/${this.pathId()}/fork`, {
      method: "POST",
    });
    return new Sandbox(this.transport, decodeSandboxInfo(raw as unknown));
  }

  /** Delete the snapshot and its archive. Forks are unaffected. */
  async remove(): Promise<void> {
    await this.transport.request(`/v1/snapshots/${this.pathId()}`, { method: "DELETE" });
  }

  private pathId(): string {
    return encodeURIComponent(this.info.id);
  }
}
