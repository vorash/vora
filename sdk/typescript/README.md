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

Read `pool.info.state` before `pool.info.stats`. Only `POOL_ACTIVE` and `POOL_DRAINING`
have stats. Otherwise `stats` is absent, because nothing was measured:

| `state` | Meaning | What to do |
| --- | --- | --- |
| `POOL_NOT_ON_RUNNER` | The route holds the reservation and the name, and the runner has no such pool. A run throws `PoolNotOnRunnerError`. | `declare` creates it again under a new id. `delete` releases the reservation. |
| `POOL_NOT_LOADED` | The runner refused it at boot. `notLoadedReason` says why. | Fix the cause, or `delete` it. |
| absent, in a listing | The runner did not answer. | Read `spec.reuse` if it matters. It comes from the route. |

Both `POOL_NOT_ON_RUNNER` and an unanswered entry have only the route's part of `spec`:
the name, tenant, agent and reuse. Lost pools stay listed until deleted, as they hold a
reservation. Keep the pool's name, not its `id`, across a `declare`.

Limits on a run:

- The encoded request must fit the API's 1 MiB body limit, which leaves about 768 KiB of
  stdin. A larger one throws `RequestTooLargeError` without being sent.
- A client that reads slower than about 1.5 KiB/s while output is flowing is cut off
  with `client_stalled`.
- A run can wait up to 7 minutes for its first frame, which then throws
  `GatewayTimeoutError`. Node's `fetch` stops waiting at 300 s, both for that answer and
  between frames of a silent command. The first throws a `VoraError` that says so,
  without the API's status, and the second a `PoolRunError` with code `stream_ended`. A
  pool with a long `borrowTimeoutMs`, or a command that stays silent that long, needs a
  `fetch` that waits:

  ```ts
  import { Agent, fetch as undiciFetch } from "undici";

  const patient = new Agent({ headersTimeout: 0, bodyTimeout: 0 });
  const vora = new Vora({
    fetch: ((url, init) => undiciFetch(url, { ...init, dispatcher: patient })) as typeof fetch,
  });
  ```

`declare` needs `cpu` and `memory`, as the API has no default for them.
`borrowTimeoutMs` is at most 300000. Another agent's pool name answers 404.

`delete` returns once the pool is gone. Each request can block for up to about 25 s, and
`delete` repeats it for up to `maxWaitSeconds` (default 600).

### Errors

Every error is a `VoraError`. `status` and `body` are set when the API answered. A
network failure has neither, and keeps the original error as `cause`. Aborting a `signal`
throws the abort's own error instead, unwrapped. These subclasses say what to do next:

| Error | Status | Meaning |
| --- | --- | --- |
| `RateLimitedError` | 429 | The pool is full or capacity is reached. Nothing ran. `retryAfterSeconds` has the API's wait. |
| `UnavailableError` | 503 | No sandbox could start, or a runner is down or still starting. On a run, probably nothing ran, but it isn't certain: the runner may have been lost after the program started. `retryAfterSeconds` has the API's wait. |
| `GatewayTimeoutError` | 504 | The runner did not answer in time. It is not known whether anything ran. |
| `ConflictError` | 409 | The resource's state refuses the call. The next three are its subclasses. |
| `PoolDrainingError` | 409 | The pool is being deleted. Retry later, or declare it again once it is gone. |
| `PoolNotLoadedError` | 409 | The runner refused to load the pool. Do not retry. `reason` is its `notLoadedReason`. |
| `PoolNotOnRunnerError` | 409 | The runner has no such pool, and the route still holds its reservation. Don't retry: `declare` it again, or `delete` it. |
| `PoolSpecConflictError` | 409 | A live pool of this name has a different spec. |
| `PoolRunError` | none | A run failed after its first frame and before `exit`, so the command may have run. It has `code`, `run`, `stdout` and `stderr`. |
| `RequestTooLargeError` | none | A run's body is over the API's limit. It was not sent. |

The SDK tells the 409s apart by reading the pool's state, which costs one extra `GET`.

### Retries

Pool calls that are safe to repeat retry 429 and 503, honouring `Retry-After`, under the
client's `retry` (a `RetryPolicy`, default 4 attempts; `null` turns it off). `delete`
rides out 429, 503 and 504 for up to `maxWaitSeconds`, whatever the policy. A run retries
only if you pass `retry: {}`, because a repeated run isn't idempotent. It then repeats a
429 or 503, and never repeats a run past its first frame. A 429 means nothing ran. A 503
usually means the same, but a runner lost just after the program started also answers
503, so a retry can run the program twice. Use `retry` only for a program that is safe to
repeat. A `Retry-After` longer than `maxDelayMs` throws instead of waiting. Sandbox routes
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