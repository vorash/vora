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

## Pools

A pool keeps warm sandboxes for one tenant. Each run gets a sandbox that has never
run anything, or one that a reset proved clean. A sandbox whose reset fails is
destroyed.

```ts
const pool = await vora.pools.declare("py", {
  image: "vora/python",
  backend: "gvisor",
  cpu: "1000m",
  memory: "256Mi",
  pids: 64,
  size: 8,
});
const result = await pool.run({ command: "python3", args: ["-c", "print(6*7)"], timeoutSeconds: 10 });
console.log(result.stdout, result.exit.exitCode, result.exit.reason);
console.log(result.run.sandboxId, result.run.warm, result.run.startupMs, result.run.sandboxRun);
console.log(result.exit.usage, result.done?.outcome ?? "reset outcome unknown");
await pool.delete();
```

`declare` is idempotent for an equal spec. `size: 0` starts every run cold. A global
admin passes `tenantId` to `declare`, `get` and `list`, and the handle keeps it.
`streamRun` takes the same arguments as `run` and yields chunks: `run`, then `stdout`
and `stderr`, then `exit`, then `runDone`. Leaving the loop before `exit` cancels the
run.

What a result tells you:

- `result.run.resetMs` is the reset that prepared this sandbox. `result.done.resetMs` is
  this run's own reset.
- `result.done.outcome === "POOL_RESET_FAILED"` means the reset could not prove the
  sandbox clean, even though the command succeeded. The API destroys that sandbox.
- `result.run.reuse === "POOL_REUSE_UNSAFE_NO_RESET"` and `result.done.outcome ===
  "POOL_RESET_SKIPPED"` mean nothing was cleaned between runs.
- `exit` makes a result valid. If the stream breaks after it, `result.done` is absent and
  `result.streamError` says why.
- protojson leaves out zeros, so an absent duration or usage value was not measured or
  was zero. The SDK does not guess which.

Read `pool.info.state` before `pool.info.stats`. In a listing, a pool whose runner did
not answer has neither. A `POOL_NOT_LOADED` pool has zero counters, and
`notLoadedReason` says why.

Limits on a run:

- The encoded request must fit the API's 1 MiB body limit, which leaves about 786 KiB of
  stdin. A larger one throws `RequestTooLargeError` without being sent.
- A client that reads slower than about 1.5 KiB/s while output is flowing is cut off
  with `client_stalled`.
- Node's `fetch` drops a response body that is silent for 300 s. For a command that may
  stay silent that long, pass a `fetch` that uses an undici `Agent` with a longer
  `bodyTimeout`. Otherwise the run ends in `PoolRunError` with code `stream_ended`.

`delete` returns once the pool is gone. Each request can block for up to about 25 s, and
`delete` repeats it for up to `maxWaitSeconds` (default 600).

### Errors

Every error is a `VoraError` with `status` and `body`. These subclasses say what to do
next:

| Error | Status | Meaning |
| --- | --- | --- |
| `RateLimitedError` | 429 | The pool is full or capacity is reached. Nothing ran. `retryAfterSeconds` has the API's wait. |
| `UnavailableError` | 503 | No sandbox could start, or a runner is down or still starting. Nothing ran. `retryAfterSeconds` has the API's wait. |
| `GatewayTimeoutError` | 504 | The runner did not answer in time. It is not known whether anything ran. |
| `ConflictError` | 409 | The resource's state refuses the call. The next three are its subclasses. |
| `PoolDrainingError` | 409 | The pool is being deleted. Retry later, or declare it again once it is gone. |
| `PoolNotLoadedError` | 409 | The runner refused to load the pool. Do not retry. `reason` is its `notLoadedReason`. |
| `PoolSpecConflictError` | 409 | A live pool of this name has a different spec. |
| `PoolRunError` | none | A run failed after its first frame and before `exit`, so the command may have run. It has `code`, `run`, `stdout` and `stderr`. |
| `RequestTooLargeError` | none | A run's body is over the API's limit. It was not sent. |

The SDK tells the 409s apart by reading the pool's state, which costs one extra `GET`.

### Retries

Pool calls that are safe to repeat retry 429 and 503, honouring `Retry-After`, under the
client's `retry` (a `RetryPolicy`, default 4 attempts; `null` turns it off). A run
retries only if you pass `retry: {}`, because a repeated run isn't idempotent. It then
repeats a 429 or 503, which mean nothing ran, and never repeats a run past its first
frame. A `Retry-After` longer than `maxDelayMs` throws instead of waiting. Sandbox routes
do not retry.

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
| `vora.pools.declare(name, options)` | `PUT /v1/pools/{name}` |
| `vora.pools.get(name, { tenantId })` / `list({ tenantId })` | `GET /v1/pools/{name}`, `GET /v1/pools` |
| `pool.run(commandOrOptions)` | `POST /v1/pools/{name}/run` (NDJSON) |
| `pool.streamRun(commandOrOptions)` | same, chunk by chunk |
| `pool.stats()` / `pool.refresh()` | `GET /v1/pools/{name}` |
| `pool.execs(sandboxId)` / `pool.events(sandboxId)` | `GET /v1/pools/{name}/sandboxes/{id}/execs`, `/events` |
| `pool.delete({ maxWaitSeconds })` | `DELETE /v1/pools/{name}`, repeated until gone |

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