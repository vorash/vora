# vorash

Python client for the [Vora](https://github.com/vorash/vora) sandbox API.

```sh
pip install vorash
```

```python
import vora

async with vora.Vora() as client:
    sandbox = await client.sandboxes.create(image="vora/python", memory="512Mi", network="none")
    try:
        result = await sandbox.exec("python -c 'print(45)'")
        print(result.stdout)  # 45
    finally:
        await sandbox.destroy()
```

This mirrors [`@vorash/sdk`](../typescript) (the TypeScript SDK) endpoint for
endpoint and semantic for semantic — the wire encoding, the exec exit-chunk
contract and the error shapes are pinned against it by the test suite.

## What you get

| Call | What it does |
| --- | --- |
| `client.sandboxes.create(image=..., memory=..., cpu=..., pids=..., disk=..., network=..., env=..., ttl=...)` | create + start, returns a `Sandbox` |
| `client.sandboxes.get/list/destroy` | sandbox lifecycle |
| `sandbox.exec(command, args=None, cwd=, env=, stdin=, timeout_seconds=)` | run and collect `ExecResult(stdout, stderr, exit_code, reason)` |
| `sandbox.stream_exec(...)` | stream `ExecChunk`s (`stdout`/`stderr` data, final `exit` with `execution_id` and `usage: ExecUsage`) |
| `sandbox.write_file/read_file/read_text` | files in the sandbox's writable roots |
| `sandbox.stats/events/execs/egress` | resource usage and the audit readers |
| `client.snapshots.create/get/list/remove/fork`, `Snapshot.fork/remove/refresh` | snapshot capture and forks (ADR 023) |
| `client.pools.declare/get/list`, `pool.run/stream_run/stats/refresh/execs/events/delete` | warm pools, see [Pools](#pools) |

### Exec semantics

`exec("ls | wc -l")` is a shell line run via `/bin/sh -c`; pass `args` (even
empty) to run `command` literally: `exec("echo", ["hi"])`. Output that arrives
on a stream **without an exit record is a killed run**, not a success:
`exec` raises `VoraError` carrying whatever output there was.

### Pools

A pool keeps warm sandboxes for one tenant. Each run gets a sandbox that has never
run anything, or one that a reset proved clean. A sandbox whose reset fails is
destroyed.

```python
pool = await client.pools.declare(
    "py", image="vora/python", backend="gvisor", cpu="1000m", memory="256Mi", pids=64, size=8
)
result = await pool.run("python3", ["-c", "print(6*7)"], timeout_seconds=10)
print(result.stdout, result.exit.exit_code, result.exit.reason)
print(result.run.sandbox_id, result.run.warm, result.run.startup_ms, result.run.sandbox_run)
print(result.exit.usage)
print(result.done.outcome if result.done else "reset outcome unknown")
await pool.delete()
```

`declare` is idempotent for an equal spec. `size=0` starts every run cold. A global
admin passes `tenant_id` to `declare`, `get` and `list`, and the handle keeps it.
`stream_run` takes the same arguments as `run` and yields `RunChunk`s: `run`, then
`stdout` and `stderr`, then `exit`, then `run_done`. Leaving the loop before `exit`
cancels the run.

What a result tells you:

- `result.run.reset_ms` is the reset that prepared this sandbox. `result.done.reset_ms`
  is this run's own reset.
- `result.done.outcome == "POOL_RESET_FAILED"` means the reset could not prove the
  sandbox clean, even though the command succeeded. The API destroys that sandbox.
- `result.run.reuse == "POOL_REUSE_UNSAFE_NO_RESET"` and `result.done.outcome ==
  "POOL_RESET_SKIPPED"` mean nothing was cleaned between runs.
- `exit` makes a result valid. If the stream breaks after it, `result.done` is None and
  `result.stream_error` says why.
- protojson leaves out zeros, so a duration or usage value that is None was not
  measured or was zero. The SDK does not guess which.

Read `pool.info.state` before `pool.info.stats`. Only `POOL_ACTIVE` and `POOL_DRAINING`
have stats. Otherwise `stats` is None, because nothing was measured:

| `state` | Meaning | What to do |
| --- | --- | --- |
| `POOL_NOT_ON_RUNNER` | The route holds the reservation and the name, and the runner has no such pool. A run raises `PoolNotOnRunnerError`. | `declare` creates it again under a new id. `delete` releases the reservation. |
| `POOL_NOT_LOADED` | The runner refused it at boot. `not_loaded_reason` says why. | Fix the cause, or `delete` it. |
| None, in a listing | The runner did not answer. | Read `spec.reuse` if it matters. It comes from the route. |

Both `POOL_NOT_ON_RUNNER` and an unanswered entry have only the route's part of `spec`:
the name, tenant, agent and reuse. Lost pools stay listed until deleted, as they hold a
reservation. Keep the pool's name, not its `id`, across a `declare`.

Limits on a run:

- The encoded request must fit the API's 1 MiB body limit, which leaves about 786 KiB
  of stdin. A larger one raises `RequestTooLargeError` without being sent.
- A client that reads slower than about 1.5 KiB/s while output is flowing is cut off
  with `client_stalled`.

`declare` needs `cpu` and `memory`, as the API has no default for them.
`borrow_timeout_ms` is at most 300000. Another agent's pool name answers 404.

`delete` returns once the pool is gone. Each request can block for up to about 25 s,
and `delete` repeats it for up to `max_wait_seconds` (default 600). The pool calls set
their own HTTP timeouts above the API's own bounds, so a custom `httpx.AsyncClient`'s
timeouts do not cut them short. A run can wait up to 7 minutes for its first frame,
which then raises `GatewayTimeoutError`. Plain `exec` and `stream_exec` have no read
timeout, as a silent command has nothing to read until it exits.

### Sync convenience

Scripts and notebooks can skip the event loop:

```python
from vora.sync import Vora

with Vora() as client:
    sandbox = client.sandboxes.create(image="vora/python")
    print(sandbox.exec("echo hi").stdout)
    sandbox.destroy()
```

The facade drives a private event loop per call — single-threaded convenience
only; use the asyncio client for concurrent work.

## Typed

Full annotations and a `py.typed` marker ship with the package; `mypy --strict`
is clean and runs in CI.

## Configuration

| Option | Default | Meaning |
| --- | --- | --- |
| `base_url` | `VORA_API_URL`, then `http://127.0.0.1:9300` | Base URL of `vora-api` |
| `api_key` | `VORA_API_KEY` | Bearer API key |
| `client` | a fresh `httpx.AsyncClient` | Reuse a client (custom transports, tests) |

Resource strings follow container-runtime quoting: `"512Mi"` is powers of 1024,
`"500M"` is powers of 1000 (`parse_size`, `parse_cpu`, `parse_duration` are
exported for reuse).

### Errors

Every failed API call raises `vora.VoraError` with `status` (HTTP status when
the failure came from the API) and `body` (the parsed `{"error": "..."}`), and
the message carries the API's error string. These subclasses say what to do next:

| Error | Status | Meaning |
| --- | --- | --- |
| `RateLimitedError` | 429 | The pool is full or capacity is reached. Nothing ran. `retry_after` has the API's wait. |
| `UnavailableError` | 503 | No sandbox could start, or a runner is down or still starting. Nothing ran. `retry_after` has the API's wait. |
| `GatewayTimeoutError` | 504 | The runner did not answer in time. It is not known whether anything ran. |
| `ConflictError` | 409 | The resource's state refuses the call. The next three are its subclasses. |
| `PoolDrainingError` | 409 | The pool is being deleted. Retry later, or declare it again once it is gone. |
| `PoolNotLoadedError` | 409 | The runner refused to load the pool. Do not retry. `reason` is its `not_loaded_reason`. |
| `PoolNotOnRunnerError` | 409 | The runner has no such pool, and the route still holds its reservation. Don't retry: `declare` it again, or `delete` it. |
| `PoolSpecConflictError` | 409 | A live pool of this name has a different spec. |
| `PoolRunError` | none | A run failed after its first frame and before `exit`, so the command may have run. It has `code`, `run`, `stdout` and `stderr`. |
| `RequestTooLargeError` | none | A run's body is over the API's limit. It was not sent. |

The SDK tells the 409s apart by reading the pool's state, which costs one extra `GET`.

Pool calls that are safe to repeat retry 429 and 503, honouring `Retry-After`, under
the client's `retry` (a `RetryPolicy`, default 4 attempts; `None` turns it off). A run
retries only if you pass `retry=RetryPolicy()`, because a repeated run isn't
idempotent. It then repeats a 429 or 503, which mean nothing ran, and never repeats a
run past its first frame. A `Retry-After` longer than `max_delay` raises instead of
waiting. Sandbox routes do not retry.

## Development

```sh
cd sdk/python
python -m venv .venv && . .venv/bin/activate
pip install -e .[dev]
mypy src
pytest
python examples/basic.py   # needs VORA_API_URL / VORA_API_KEY
```

The tests drive everything through an `httpx.MockTransport` stand-in for
`vora-api` — no server, no network. The pool tests use a local socket that answers
with `../testdata/pool_wire.json`, which `TestSDKWireGolden` in `internal/api` writes
from the real handlers. `tests/test_pools_e2e.py` runs against a real API when
`VORA_API_URL` and `VORA_API_KEY` are set, and is skipped otherwise. `python -m build` produces the sdist and
wheel; `twine upload dist/*` publishes to PyPI once a token is configured (the
release workflow attaches the built artifacts to the GitHub release).
