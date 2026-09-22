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
| `sandbox.stream_exec(...)` | stream `ExecChunk`s (`stdout`/`stderr` data, final `exit`) |
| `sandbox.write_file/read_file/read_text` | files in the sandbox's writable roots |
| `sandbox.stats/events/execs/egress` | resource usage and the audit readers |
| `client.snapshots.create/get/list/remove/fork`, `Snapshot.fork/remove/refresh` | snapshot capture and forks (ADR 023) |

### Exec semantics

`exec("ls | wc -l")` is a shell line run via `/bin/sh -c`; pass `args` (even
empty) to run `command` literally: `exec("echo", ["hi"])`. Output that arrives
on a stream **without an exit record is a killed run**, not a success:
`exec` raises `VoraError` carrying whatever output there was.

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
the message carries the API's error string.

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
`vora-api` — no server, no network. `python -m build` produces the sdist and
wheel; `twine upload dist/*` publishes to PyPI once a token is configured (the
release workflow attaches the built artifacts to the GitHub release).
