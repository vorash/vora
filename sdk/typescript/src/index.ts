/**
 * TypeScript client for the Vora sandbox API.
 *
 * ```ts
 * import { Vora } from "@vorash/sdk";
 *
 * const vora = new Vora({ apiKey: process.env.VORA_API_KEY });
 * const sandbox = await vora.sandboxes.create({ image: "vora/python", memory: "512Mi", cpu: 1 });
 * const result = await sandbox.exec("python -c 'print(\"hello\")'");
 * console.log(result.stdout);
 * await sandbox.destroy();
 * ```
 */
export { Vora, Sandboxes } from "./client.js";
export type { ListSandboxesOptions, VoraOptions } from "./client.js";
export { Sandbox } from "./sandbox.js";
export type { WriteFileOptions } from "./sandbox.js";
export { Snapshots, Snapshot } from "./snapshots.js";
export { VoraError } from "./errors.js";
export { Transport } from "./http.js";
export type { JsonRequestOptions, RequestOptions, TransportOptions } from "./http.js";
export { parseCPU, parseDuration, parseSize } from "./units.js";
export type * from "./types.js";