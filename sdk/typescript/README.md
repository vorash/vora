# @vorash/sdk

TypeScript client for the [Vora](https://github.com/vorash/vora) sandbox API.

Wraps [`vora-api`](../../docs/architecture/overview.md) over HTTP/JSON with the
API key, and exposes sandbox lifecycle, exec (streaming), files, stats and the
audit surfaces (`events`, `execs`, `egress`). It has **no runtime
dependencies** and runs on Node 18+ and in browsers that provide `fetch`.

## Install

```sh
npm install @vorash/sdk
```

## Quick start

```ts
import { Vora } from "@vorash/sdk";

const vora = new Vora({
  baseUrl: "http://127.0.0.1:9300", // or VORA_API_URL
  apiKey: process.env.VORA_API_KEY, // or VORA_API_KEY
});

const sandbox = await vora.sandboxes.create({
  image: "vora/python",
  cpu: 1,         // cores, or "500m"
  memory: "512Mi", // or a byte count
  pids: 128,
  network: "none", // or "restricted" | "full" | "private"
  ttl: "10m",
});

await sandbox.writeFile("/workspace/main.py", 'print("hello")');

const result = await sandbox.exec("python /workspace/main.py");
console.log(result.stdout);  // "hello\n"
console.log(result.exitCode); // 0

await sandbox.destroy();
```

`new Vora()` with no arguments reads `VORA_API_URL` and `VORA_API_KEY`.

## Streaming

`exec` collects output; `streamExec` yields it as it arrives:

```ts
for await (const chunk of sandbox.streamExec("apt-get update")) {
  if (chunk.type === "stdout") process.stdout.write(chunk.data);
  if (chunk.type === "stderr") process.stderr.write(chunk.data);
  if (chunk.type === "exit") console.log(`exit ${chunk.exitCode} (${chunk.reason})`);
}
```

The exit chunk also carries `executionId` (the exec's audit record) and
`usage`: `cpuMillis`, `memoryPeakBytes` with its `memoryPeakScope`, `oomKills`
and `pidsLimitHits`, all measured over that exec only.

## stdin

Pass `stdin` to feed the child; it sees EOF once the input is consumed.

```ts
const result = await sandbox.exec("wc -c", { stdin: "hello" });
```

A **string** command runs through `/bin/sh -c`, so quoting and pipes work. Pass
an object (`{ command: "python", args: ["main.py"] }`) to run without a shell.

## Surface

| API | Endpoint |
| --- | --- |
| `vora.sandboxes.create(options)` | `POST /v1/sandboxes` |
| `vora.sandboxes.get(id)` | `GET /v1/sandboxes/{id}` |
| `vora.sandboxes.list({ includeDestroyed })` | `GET /v1/sandboxes` |
| `vora.sandboxes.destroy(id)` | `DELETE /v1/sandboxes/{id}` |
| `sandbox.exec(commandOrOptions)` | `POST /v1/sandboxes/{id}/exec` (NDJSON) |
| `sandbox.streamExec(commandOrOptions)` | same, chunk by chunk |
| `sandbox.readFile(path)` / `readText(path)` | `GET /v1/sandboxes/{id}/files` |
| `sandbox.writeFile(path, content, { createParents })` | `PUT /v1/sandboxes/{id}/files` |
| `sandbox.stats()` | `GET /v1/sandboxes/{id}/stats` |
| `sandbox.events()` | `GET /v1/sandboxes/{id}/events` |
| `sandbox.execs()` | `GET /v1/sandboxes/{id}/execs` |
| `sandbox.egress()` | `GET /v1/sandboxes/{id}/egress` |
| `sandbox.refresh()` / `sandbox.destroy()` | record re-read / teardown |

Admin endpoints (`/v1/keys`, `/v1/secrets`, `/v1/runners`) are not wrapped; use
the `vora` CLI for those.

## Wire notes

`vora-api` speaks [protojson](https://protobuf.dev/programming-guides/json/):
camelCase fields, enums as names (`"RUNNING"`), bytes as base64, and **int64 as
strings** (`"cpuMillis": "1000"`). This SDK is the only layer that knows those
rules — everything above it uses plain numbers and `Uint8Array`s. Sizes and
durations are converted for you (`"512Mi"` → bytes, `"1h30m"` → seconds).

Errors are thrown as `VoraError` with `status` and the parsed `body`
(`{"error": "..."}` from the API).

## Development

This package is an npm workspace member, so install from the repository root:

```sh
npm install                                    # from the repository root
npm run build --workspace @vorash/sdk          # ESM + CJS + type declarations into dist/
npm run typecheck --workspace @vorash/sdk      # run after build; the example imports the built output
npm test --workspace @vorash/sdk               # vitest, against a local HTTP stand-in
npm run example --workspace @vorash/sdk        # runs examples/basic.ts against a live vora-api
```

`npm run build` emits `dist/esm` (ESM) and `dist/cjs` (CommonJS, marked with a
`package.json` so Node reads it correctly), plus `.d.ts` for both.