# Vora

Vora runs code you don't trust — AI agents, generated scripts, user uploads — in
isolated Linux sandboxes, and keeps a record of everything it did.

You ask for a sandbox from an OCI image with memory, CPU, PID, disk and TTL
limits; run commands in it; move files in and out; and read back every
lifecycle event, execution and network decision. Networking is off unless you
turn it on. Each sandbox is isolated by Bubblewrap (namespaces + cgroup v2) or
gVisor. Vora is written in Go and does not need Kubernetes.

> **Status: v0.10, a proof of concept.** Everything below is implemented and
> tested, but it has not been independently audited. Don't trust it with
> anything that matters yet — see [Limitations](#limitations).

## Contents

- [Quick start](#quick-start)
- [Using the HTTP API](#using-the-http-api)
- [SDKs and agents](#sdks-and-agents)
- [More examples](#more-examples)
- [How it works](#how-it-works)
- [Security model](#security-model)
- [Limitations](#limitations)
- [Install and release](#install-and-release)
- [Development](#development)

## Quick start

On a single machine, the `vora` CLI talks straight to the runner over a Unix
socket. With a [development checkout](#development):

```bash
make binaries images pull-images   # build bin/*, the rootfs images, import them into containerd
make install-rootless              # start vora-runner as a user systemd service
```

```console
$ vora sandbox create --image vora/python --memory 512m --cpu 1 --pids 128
Sandbox created: sbx_01M2NVHE1KRD8JT3D6QG6R0EW3

$ vora exec sbx_01M2NVHE1KRD8JT3D6QG6R0EW3 -- python3 -c 'print("hello from vora")'
hello from vora

$ vora exec sbx_01M2NVHE1KRD8JT3D6QG6R0EW3 -- python3 -c 'import socket; s=socket.socket(); s.settimeout(2); print("metadata:", "REACHABLE" if s.connect_ex(("169.254.169.254", 80)) == 0 else "BLOCKED")'
metadata: BLOCKED

$ vora stats sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
memory:    9777152 bytes
peak:      19267584 bytes
cpu:       182 millis
pids:      11

$ vora logs sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
2026-09-18T13:20:02+02:00  REQUESTED -> SCHEDULING
2026-09-18T13:20:02+02:00  SCHEDULING -> CREATING
2026-09-18T13:20:02+02:00  CREATING -> STARTING
2026-09-18T13:20:03+02:00  STARTING -> RUNNING

$ vora sandbox destroy sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
Destroyed sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
```

Use `vora/python`, not `python`: a bare name resolves to Docker Hub. The images
`vora/base`, `vora/python` and `vora/node` (Debian 13, Python 3.13, Node 22)
are published to GHCR; [Images](#images) covers pulling them on a server.

## Using the HTTP API

Applications, SDKs and agents use `vora-api`, the control plane. It
authenticates API keys, keeps tenants apart, and routes each sandbox to a
runner. Runners dial out to it and register themselves.

### 1. Start the API and a runner

For local development, both on loopback. The runner started by
`make install-rootless` lives in its own rootlesskit network namespace and
can't reach the host's loopback, so stop it and run the binary directly (bwrap
only; this runner has no gVisor):

```bash
systemctl --user stop vora-runner                         # frees its socket and :9101

export VORA_API_BOOTSTRAP_KEY="$(openssl rand -hex 32)"   # initial admin key, never stored
bin/vora-api &                                            # HTTP :9300, runner registry :9301

VORA_API_GRPC=127.0.0.1:9301 \
VORA_RUNNER_ADDR=127.0.0.1:9200 \
VORA_RUNNER_INSECURE=1 \
  bin/vora-runner &                                       # registers, then heartbeats
```

`VORA_RUNNER_INSECURE=1` is accepted on a loopback address only. Anything
reachable from another host needs TLS and a join token — see
[docs/configuration.md](docs/configuration.md#tls). On a server, the system
install runs both as services.

### 2. Create a tenant key

The bootstrap key is a global admin. Use it to mint a sandbox-scoped key for a
tenant, and use that key from then on:

```bash
API=http://127.0.0.1:9300

curl -s -X POST "$API/v1/keys" \
  -H "Authorization: Bearer $VORA_API_BOOTSTRAP_KEY" \
  -d '{"name": "demo", "tenant_id": "acme", "scopes": ["sandbox"]}'
# {"api_key":{"id":"key_…","tenant_id":"acme",…},"key":"vora_…"}   ← shown once

export VORA_API_KEY="vora_…"
```

A tenant key sees only its tenant's sandboxes and snapshots. `admin` keys also
manage keys and secrets, and a global admin (no tenant) can list runners.
From the CLI: `vora --api "$API" --api-key "$VORA_API_BOOTSTRAP_KEY" keys create
--name demo --tenant acme`.

### 3. Create a sandbox, run code, clean up

```bash
AUTH="Authorization: Bearer $VORA_API_KEY"

# Create and start. Returns the sandbox record with its id.
SBX=$(curl -s -X POST "$API/v1/sandboxes" -H "$AUTH" -d '{
  "image": "vora/python",
  "resources": {"cpuMillis": 1000, "memoryBytes": 536870912, "pids": 128},
  "network": {"mode": "NETWORK_NONE"},
  "ttlSeconds": 600
}' | jq -r .id)

# Upload a file (raw body, 16 MiB max).
curl -s -X PUT "$API/v1/sandboxes/$SBX/files?path=/workspace/main.py" -H "$AUTH" \
  --data-binary 'print(sum(range(10)))'
# {"bytes_written":21,"path":"/workspace/main.py"}

# Run it. The response is NDJSON, one chunk per line; output is base64.
curl -sN -X POST "$API/v1/sandboxes/$SBX/exec" -H "$AUTH" \
  -d '{"command": "python3", "args": ["main.py"], "cwd": "/workspace", "timeoutSeconds": 30}'
# {"stdout":"NDUK"}
# {"exit":{"reason":"EXITED","usage":{"cpuMillis":"18","memoryPeakBytes":"9437184","memoryPeakScope":"PEAK_SCOPE_WINDOW"},"executionId":"exe_01K…"}}

# Decode stdout on the fly.
curl -sN -X POST "$API/v1/sandboxes/$SBX/exec" -H "$AUTH" \
  -d '{"command": "/bin/sh", "args": ["-c", "ls /workspace | wc -l"]}' \
  | jq -r '.stdout // empty | @base64d'

# Read a file back, check usage and the audit trail.
curl -s "$API/v1/sandboxes/$SBX/files?path=/workspace/main.py" -H "$AUTH"
curl -s "$API/v1/sandboxes/$SBX/stats"  -H "$AUTH"
curl -s "$API/v1/sandboxes/$SBX/execs"  -H "$AUTH"   # every execution, with exit reason
curl -s "$API/v1/sandboxes/$SBX/events" -H "$AUTH"   # lifecycle transitions

# Destroy (204). The audit trail stays readable afterwards.
curl -s -X DELETE "$API/v1/sandboxes/$SBX" -H "$AUTH"
```

### Wire format

The sandbox, exec, snapshot and audit routes speak
[protojson](https://protobuf.dev/programming-guides/json/):

- **Field names** are camelCase in responses; requests accept camelCase or
  snake_case.
- **Enums use their full names**: `NETWORK_NONE`, `RUNNING`, `OOM_KILLED`. The
  SDKs accept `"none"`; the raw API does not.
- **`bytes` fields are base64**: exec `stdout`/`stderr`, and `stdin` on the
  exec request.
- **int64 values come back as strings** (`"createdAtUnixMs": "1790000000000"`),
  and fields at their zero value are left out. A successful exit has no
  `exitCode`, and a usage counter that stayed at zero (`oomKills`) is absent.
- **`command` runs as is**, without a shell. Use `"command": "/bin/sh", "args":
  ["-c", "…"]` for pipes and redirects.
- **An exec stream that ends without an `exit` chunk was killed.** Don't treat
  it as success.
- **Errors** are `{"error": "…"}` with an HTTP status.

The admin routes (`/v1/keys`, `/v1/secrets`, `/v1/runners`) and the file-write
response are plain JSON with snake_case names only: send `tenant_id`, not
`tenantId`.

### Endpoints

All routes take `Authorization: Bearer <key>`.

| Method and path | Purpose | Scope |
| --------------- | ------- | ----- |
| `POST /v1/sandboxes` | Create and start a sandbox | sandbox |
| `GET /v1/sandboxes[?include_destroyed=true]` | List your sandboxes | sandbox |
| `GET /v1/sandboxes/{id}` | Get one sandbox | sandbox |
| `DELETE /v1/sandboxes/{id}` | Destroy it | sandbox |
| `POST /v1/sandboxes/{id}/exec` | Run a command (streams NDJSON) | sandbox |
| `GET`/`PUT /v1/sandboxes/{id}/files?path=` | Read or write a file (`&create_parents=true`) | sandbox |
| `GET /v1/sandboxes/{id}/stats` | Memory, CPU, PIDs, OOM kills | sandbox |
| `GET /v1/sandboxes/{id}/events` | Lifecycle audit | sandbox |
| `GET /v1/sandboxes/{id}/execs` | Execution audit | sandbox |
| `GET /v1/sandboxes/{id}/egress` | Network decisions (restricted mode) | sandbox |
| `POST /v1/sandboxes/{id}/snapshot` | Capture writable state while it runs | sandbox |
| `GET /v1/snapshots`, `GET`/`DELETE /v1/snapshots/{id}` | Manage snapshots | sandbox |
| `POST /v1/snapshots/{id}/fork` | Start a new sandbox from a snapshot | sandbox |
| `PUT`/`GET`/`DELETE /v1/pools/{name}`, `GET /v1/pools` | Declare, read, delete and list warm pools | sandbox |
| `POST /v1/pools/{name}/run` | Run a command on a pool member (streams NDJSON) | sandbox |
| `GET /v1/pools/{name}/sandboxes/{id}/execs`, `.../events` | A member's audit trail | sandbox |
| `POST`/`GET /v1/keys`, `DELETE /v1/keys/{id}` | API keys | admin |
| `POST`/`GET /v1/secrets`, `DELETE /v1/secrets/{alias}` | Brokered credentials | admin |
| `GET /v1/runners` | Runner inventory (global admin only) | admin |

The CLI drives the same API with `--api`:

```bash
vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" sandbox create --image vora/base
```

### The sandbox spec

| Field | Meaning |
| ----- | ------- |
| `image` | OCI reference, e.g. `vora/python` or `ghcr.io/vorash/vora-python@sha256:…` |
| `backend` | `bwrap` or `gvisor`; empty picks one automatically |
| `resources` | `cpuMillis`, `memoryBytes`, `pids`, `diskBytes` — capped by the runner's ceilings |
| `network.mode` | `NETWORK_NONE` (default), `NETWORK_RESTRICTED`, `NETWORK_FULL`, `NETWORK_PRIVATE` |
| `network.allowedDomains` | Hosts a restricted sandbox may reach over TLS |
| `network.brokerAliases` | Credentials the sandbox may use through the broker |
| `env` | `["KEY=value", …]` |
| `ttlSeconds` | Destroyed automatically after this long |

| Network mode | What the sandbox gets |
| ------------ | --------------------- |
| `NETWORK_NONE` | No interfaces, no routes |
| `NETWORK_RESTRICTED` | TLS to allowlisted hosts only (checked by SNI), allowlist-only DNS, every decision audited ([ADR 012](docs/adr/012-egress-gateway.md)) |
| `NETWORK_FULL` | Public internet. Private ranges, cloud metadata and other sandboxes stay blocked (bwrap only) |
| `NETWORK_PRIVATE` | A `/24` shared with the tenant's other private sandboxes, no public egress (bwrap only; needs a tenant) |

The last three need the system install (root or a delegated cgroup subtree); a
rootless development runner refuses them.

## SDKs and agents

The SDKs wrap the HTTP API and handle the wire format for you: sizes like
`"512Mi"`, durations like `"10m"`, shell command strings, and base64. They read
`VORA_API_URL` and `VORA_API_KEY`. Neither is on a package registry yet;
install from this repository.

**TypeScript** ([`sdk/typescript`](sdk/typescript)), Node 18+ or any runtime
with `fetch`:

```ts
import { Vora } from "@vorash/sdk";

const vora = new Vora(); // VORA_API_URL, VORA_API_KEY
const sandbox = await vora.sandboxes.create({ image: "vora/node", memory: "256Mi", ttl: "5m" });
try {
  await sandbox.writeFile("/workspace/index.js", "console.log(process.version)");
  const { stdout, exitCode } = await sandbox.exec("node /workspace/index.js");
  console.log(exitCode, stdout); // 0 v22.x.x
} finally {
  await sandbox.destroy();
}
```

**Python** ([`sdk/python`](sdk/python), `pip install -e sdk/python`), asyncio
plus a sync facade:

```python
from vora.sync import Vora

with Vora() as client:
    sandbox = client.sandboxes.create(image="vora/python", memory="512Mi", ttl="10m")
    try:
        sandbox.write_file("/workspace/main.py", "print(6 * 7)\n")
        result = sandbox.exec("python main.py", cwd="/workspace")
        print(result.exit_code, result.stdout)  # 0 42
    finally:
        sandbox.destroy()
```

**Coding agents.** [`sdk/mcp`](sdk/mcp) is an MCP server that gives an agent
six sandbox tools: create, exec, read and write files, stats, and destroy. It
has its own policy, off-by-default networking and resource caps included, and
should use an agent-bound key:

```bash
claude mcp add vora --env VORA_API_URL=http://127.0.0.1:9300 --env VORA_API_KEY=vora_… \
  -- node /path/to/vora/sdk/mcp/dist/index.js
```

There is also an OpenAI Agents adapter
([`integrations/openai-agents`](integrations/openai-agents)) and a TypeSafe JEV
decision tool ([`integrations/typesafe-jev`](integrations/typesafe-jev)).

## More examples

### Stream a long-running command

```ts
for await (const chunk of sandbox.streamExec("for i in 1 2 3; do echo step $i; sleep 1; done")) {
  if (chunk.type === "stdout") process.stdout.write(chunk.data);
  if (chunk.type === "stderr") process.stderr.write(chunk.data);
  if (chunk.type === "exit") console.log(`exit ${chunk.exitCode} (${chunk.reason})`);
}
```

`reason` explains how the run ended: `EXITED`, `OOM_KILLED`,
`PIDS_LIMIT_EXCEEDED`, `TIMEOUT_EXCEEDED`, `DISK_QUOTA_EXCEEDED` or
`RUNTIME_FAILURE`, which means the sandbox failed rather than the program: it died
under the command, or couldn't start a process for it. A command that doesn't exist
is the command's own failure: `EXITED`, with exit code 127 on `bwrap` and 128 on
`gvisor`.

### What an exec used

The exit chunk also carries `executionId`, the id of the exec's record in
`GET /v1/sandboxes/{id}/execs`, and `usage`, what the sandbox used while that
exec ran:

| Field | Meaning |
| ----- | ------- |
| `cpuMillis` | CPU time consumed during the exec |
| `memoryPeakBytes` | memory high-water mark during the exec, including the sandbox's resident baseline (its init; on gVisor also the sentry and gofer) |
| `memoryPeakScope` | `PEAK_SCOPE_WINDOW`: the peak covers only this exec. `PEAK_SCOPE_LIFETIME`: the kernel (before Linux 6.12) could not reset the peak, so it is the sandbox's lifetime peak. Absent: not measured |
| `oomKills` | OOM kills during the exec |
| `pidsLimitHits` | forks refused by the sandbox's pids limit during the exec. On gVisor these are the Sentry's own threads, not the guest's forks |

The counters are per-exec deltas, not lifetime totals. They are measured on
the sandbox's cgroup, so execs that run at the same time in one sandbox have
overlapping windows and count each other's usage.

The reason is derived from that usage: an exec whose window counted an OOM
kill reports `OOM_KILLED`, even though the process itself only saw a kill
signal, and one that failed after hitting the pids limit reports
`PIDS_LIMIT_EXCEEDED`. `TIMEOUT_EXCEEDED` is never overridden. Exec records
carry the same reason and fields (`memoryPeakScope`, `oomKills`,
`pidsLimitHits`).

In an exec record a usage field is absent when it was not measured, and a
measured zero is `"0"`. The exec may have run unmetered, its sandbox may have
died before the window closed, or the record may be older than the field.

On Bubblewrap an OOM kill ends the exec, not the sandbox. The processes an
exec starts are always the OOM killer's first choice, ahead of the sandbox's
init, so the kernel kills one of them. The exec still ends with an exit chunk
that reports `OOM_KILLED` and its usage, the sandbox accepts the next exec,
and `stats` keeps reporting memory and CPU.

`memoryPeakBytes` in an exec record used to be the sandbox's lifetime peak at
the moment the exec finished. It now means the peak during that exec's window,
as `memoryPeakScope` states. Records written before this change have no scope.

### Reuse sandboxes with a warm pool

A pool keeps started sandboxes for one tenant and lends one per run. Between two
runs Vora kills what the run left and restores every writable mount, and it
destroys a sandbox whose reset it can't prove
([ADR 028](docs/adr/028-warm-pools.md)).

```bash
# Declare it. Repeating the same declare is a no-op (200). A different spec is 409.
curl -s -X PUT "$API/v1/pools/py" -H "$AUTH" -d '{
  "sandbox": {
    "image": "vora/python", "backend": "bwrap",
    "resources": {"cpuMillis": 1000, "memoryBytes": 268435456, "pids": 64},
    "network": {"mode": "NETWORK_NONE"}
  },
  "size": 2, "maxSize": 4
}'

# Run one command. timeoutSeconds is required.
curl -sN -X POST "$API/v1/pools/py/run" -H "$AUTH" \
  -d '{"command": "python3", "args": ["-c", "print(6*7)"], "timeoutSeconds": 30}'
# {"run":{"poolId":"pool_…","sandboxId":"sbx_…","executionId":"exe_…","warm":true,"sandboxRun":1,"reuse":"POOL_REUSE_RESET"}}
# {"stdout":"NDIK"}
# {"exit":{"reason":"EXITED","usage":{…},"executionId":"exe_…"}}
# {"runDone":{"outcome":"POOL_RESET_OK","resetMs":"2"}}

# Read its state, then delete it. A 202 means runs are still finishing: repeat it.
curl -s "$API/v1/pools/py" -H "$AUTH"
curl -s -X DELETE "$API/v1/pools/py" -H "$AUTH"
```

The `exit` frame comes as soon as the program ends, so a client that needs only
the result can stop there. `runDone` follows after the reset and says what
happened to the sandbox.

The spec's fields:

| Field | Meaning | Default |
| ----- | ------- | ------- |
| `sandbox` | The template for every member. It must name a `backend` and set `resources`, must leave its id, tenant, agent and TTL empty, and `network.mode` must be `NETWORK_NONE` | |
| `size` | Members to keep warm. `0` starts every run cold | 8 |
| `maxSize` | Most members at once, reserved against the runner's capacity even when idle | `max(size, 1)` |
| `maxRuns` | Runs per member before it is replaced | 100 |
| `maxAgeSeconds` | Age limit of a member | 1800 |
| `maxRunSeconds` | Largest `timeoutSeconds` a run may ask for | 300 |
| `borrowTimeoutMs` | How long a run waits for a member, at most 300000 | 2000 |
| `idleExpirySeconds` | With no run for this long, the warm target drops to 0 until the next run | 3600 |
| `reuse` | See below | `POOL_REUSE_RESET` |

`resources.pids` has a floor: 12 on `bwrap` and 64 on `gvisor`.

`reuse` has two modes:

- `POOL_REUSE_RESET` resets a member between runs, and destroys it unless the host
  can prove two things: no process remains but init's, and every writable mount is
  back to its listing from when the member was prepared. A later run can still see
  the traces listed at the end of this section.
- `POOL_REUSE_UNSAFE_NO_RESET` skips the reset, so a run sees what the last one
  left. It exists as a baseline for measuring what a reset costs. It needs an
  admin key and a runner started with `VORA_POOL_UNSAFE_REUSE=allow`.

`GET /v1/pools/{name}` returns `spec`, `state` and `stats`. Counters reset when
the runner restarts.

| `stats` field | Meaning |
| ------------- | ------- |
| `starting`, `idle`, `busy`, `resetting`, `retiring` | Members in each state now |
| `runsTotal`, `warmRunsTotal` | Runs, and how many found a warm member |
| `resetsTotal`, `resetFailuresTotal` | Resets, and how many could not prove the member clean |
| `destroyedTotal` | Members destroyed for any reason |
| `exhaustedTotal` | Runs refused because no member came free |
| `startFailedTotal`, `lastStartError` | Member starts that failed, and the kind of the last failure |
| `dormant` | `idleExpirySeconds` passed with no run, so no member is kept warm |

What a client sees when something goes wrong:

| Event | Client sees | Member |
| ----- | ----------- | ------ |
| No member came free in `borrowTimeoutMs` | 429 with `Retry-After`. The program did not run | |
| The first member's exec could not start | A normal run on another member, or 503 if the retry fails too | Destroyed |
| The program runs past `timeoutSeconds` | `exit` with `TIMEOUT_EXCEEDED`, within about 5 s more if a process holds its output | Reset |
| The command doesn't exist | `exit` with `EXITED`, and exit code 127 on bwrap or 128 on gVisor | Reset |
| The program runs out of memory | bwrap: `exit` with `OOM_KILLED`. gVisor: the sandbox goes down with it, so any `exit` other than `EXITED`, or the error line below | bwrap: reset. gVisor: destroyed |
| The sandbox dies under the program | `exit` with `RUNTIME_FAILURE`, or a last line `{"error":{"code":"run_failed",…}}` in place of `exit` | Destroyed |
| The reset can't prove the member clean | `runDone` with `POOL_RESET_FAILED` and a `retireCause` | Destroyed |
| The client disconnects | Nothing | Reset |
| The pool is being deleted | 409 | |
| The runner refused to load the pool at boot | 409, and `state` is `POOL_NOT_LOADED` with a `notLoadedReason` | None |
| The runner lost its record of the pool | 409, and `state` is `POOL_NOT_ON_RUNNER`. A PUT creates it again and a DELETE releases its reservation | None |

`notLoadedReason` is one of `backend_missing`, `backend_not_reusable`,
`unsafe_reuse_not_granted`, `spec_refused`, `capacity` or `duplicate_name`.

`GET /v1/pools/{name}/sandboxes/{id}/execs` and `.../events` read a member's audit
trail. `run.sandboxRun` marks run boundaries in it. Every plain `/v1/sandboxes/{id}`
route refuses a member with 409, reads included, so those two are how to read one.
`GET /v1/sandboxes` still lists it, with its pool. Behind the API, the runner's own
RPCs refuse what would change a member: start, exec, signal, destroy, snapshot and
files. Its RPCs for a sandbox's record, stats, events and execs stay open.

A later run on a member can still tell that an earlier one happened, through PID
numbers, file access times and directory inode flags. A pool belongs to one tenant,
so these stay open ([ADR 028](docs/adr/028-warm-pools.md)).

The [Python](sdk/python/README.md#pools) and
[TypeScript](sdk/typescript/README.md#pools) SDKs wrap these routes.

### Allow one API, deny everything else

A restricted sandbox reaches only the hosts you list, over TLS, and every
decision is recorded:

```bash
SBX=$(curl -s -X POST "$API/v1/sandboxes" -H "$AUTH" -d '{
  "image": "vora/python",
  "network": {"mode": "NETWORK_RESTRICTED", "allowedDomains": ["pypi.org", "files.pythonhosted.org"]}
}' | jq -r .id)

curl -sN -X POST "$API/v1/sandboxes/$SBX/exec" -H "$AUTH" \
  -d '{"command": "python3", "args": ["-m", "pip", "download", "--no-deps", "-d", "/tmp", "six"]}' >/dev/null

curl -s "$API/v1/sandboxes/$SBX/egress" -H "$AUTH" \
  | jq -c '.records[] | {host, allowed: (.allowed // false), reason}'
# {"host":"pypi.org","allowed":true,…}
# {"host":"files.pythonhosted.org","allowed":true,…}
```

A connection to anything else, including another sandbox, the host or
`169.254.169.254`, is dropped. It is recorded with a `reason`, and without
`allowed`, since protojson omits `false`.

### Try N fixes from one starting point

Set a sandbox up once, snapshot it while it runs, and fork it for each
candidate. Each fork starts from the same files and can't see the others'
changes:

```python
import asyncio
import vora

TEST = "from solution import slugify\nassert slugify('Hello, World') == 'hello-world'\n"

async def main(candidates: list[str]) -> None:
    async with vora.Vora() as client:
        base = await client.sandboxes.create(image="vora/python", memory="512Mi", ttl="30m")
        await base.write_file("/workspace/test_solution.py", TEST)
        snapshot = await client.snapshots.create(base.id)
        await base.destroy()  # the snapshot outlives it

        async def attempt(code: str) -> bool:
            box = await snapshot.fork()
            try:
                await box.write_file("/workspace/solution.py", code)
                result = await box.exec("python test_solution.py", cwd="/workspace")
                return result.exit_code == 0
            finally:
                await box.destroy()

        print(await asyncio.gather(*(attempt(c) for c in candidates)))  # [True, False, …]
        await snapshot.remove()

asyncio.run(main([
    "def slugify(s): return '-'.join(s.lower().replace(',', '').split())",
    "def slugify(s): return s.lower()",
]))
```

Over HTTP the same steps are `POST /v1/sandboxes/{id}/snapshot`, then
`POST /v1/snapshots/{snap_id}/fork` once per candidate
([ADR 023](docs/adr/023-snapshot-fork.md)).

### Give a sandbox a token it can't read

With `VORA_MASTER_KEY` set on `vora-api`, an admin stores a credential bound to
one host. The sandbox calls it through the broker, and the gateway adds the
token on the way out:

```bash
vora --api "$API" --api-key "$VORA_API_BOOTSTRAP_KEY" \
  secrets set --tenant acme --alias github --host api.github.com --value-stdin <<<"$GITHUB_TOKEN"

vora --api "$API" --api-key "$VORA_API_KEY" sandbox create --image vora/python \
  --network restricted --allow-domain api.github.com --broker github

vora --api "$API" --api-key "$VORA_API_KEY" exec sbx_… -- \
  python3 -c 'import os,urllib.request; print(urllib.request.urlopen(os.environ["VORA_BROKER_URL"]+"/creds/github/user").status)'
# 200
```

The token never enters the sandbox's environment, filesystem or `/proc`, the
sandbox record, or the audit trail, which records only the alias
([ADR 013](docs/adr/013-credential-brokering.md)). The broker needs TLS between
the API and the runner, unless `VORA_ALLOW_INSECURE_BROKER=1` is set.

### Full applications

[`examples/`](examples) has runnable recipes: a CI gate for untrusted PRs, an
autograder, a plugin runner, an agent `run_code` tool, patch fan-out, and more.

## How it works

```text
  SDKs · MCP · CLI --api          vora CLI (local)
          │ HTTP/JSON                  │ gRPC, unix socket
          ▼                            │
      vora-api  ◀── runners register ──┤
   keys, tenants,                      │
   placement                           │
          │ gRPC (mTLS)                ▼
          └──────────────────▶  vora-runner ── state, TTL reaper, reconciler, audit
                                       │
                      ┌────────────────┴────────────────┐
                      ▼                                 ▼
              bwrap backend                      gVisor backend
        namespaces + cgroup v2               runsc, user-space kernel
                      │                                 │
                      └───────── sandbox ───────────────┘
              read-only image rootfs, writable /workspace /root /tmp,
              vora-init as PID 1 (exec agent)
```

- **Runner** (`vora-runner`): manages the sandbox lifecycle, persists state,
  reaps expired sandboxes, and cleans up orphans at startup. Images come from
  containerd snapshots.
- **Backends**, chosen per sandbox behind one interface
  ([ADR 001](docs/adr/001-runtime-interface.md)):
  - **Bubblewrap**: unprivileged namespaces, cgroup v2 limits through systemd,
    and a seccomp deny-list.
  - **gVisor** (`runsc`): a user-space kernel.
- **`vora-init`**: Vora's own static PID 1 inside the sandbox, so shell-less
  (distroless) images work.
- **Audit**: lifecycle events, executions and egress decisions go to local
  JSONL or PostgreSQL/Supabase. Destroying a sandbox doesn't delete its trail;
  only `VORA_AUDIT_RETENTION` does ([ADR 026](docs/adr/026-audit-retention.md)).
- **Observability**: Prometheus metrics at `127.0.0.1:9101/metrics`, and
  OpenTelemetry traces from API to runtime.
- **Control plane** (`vora-api`): scoped, hashed API keys; strict tenant
  isolation; keys bound to agent identities; capacity-aware placement
  ([ADR 021](docs/adr/021-capacity-placement.md)). Runners behind NAT connect
  through a reverse tunnel ([ADR 022](docs/adr/022-reverse-tunnel.md)).
- **CLI plugins**: `vora <name>` runs `vora-<name>` from `$PATH`, the way `git`
  and `kubectl` do. Built-in commands always win. `VORA_DISABLE_PLUGINS=1`
  turns plugins off.

The design decisions are in [docs/adr](docs/adr), and the architecture in
[docs/architecture](docs/architecture).

## Security model

Vora assumes all sandboxed code is malicious. The
[threat model](docs/architecture/threat-model.md) lists the exact boundaries.
Each row below is enforced by a test, and each test was checked to fail when
its flag is removed:

| Boundary | Mechanism | Test |
| -------- | --------- | ---- |
| Host filesystem | read-only image rootfs; host paths never mounted | `TestFilesystemIsolation` |
| Rootfs writes | only `/workspace`, `/root`, `/tmp` writable | `TestFilesystemIsolation` |
| Cross-sandbox files | distinct mount namespaces | `TestCrossSandboxFilesystemIsolation` |
| Processes / signals | PID namespace | `TestPIDNamespaceIsolation`, `TestCannotSignalHost` |
| IPC / UTS | IPC namespace, private hostname | `TestIPCNamespaceIsolation`, `TestUTSNamespaceIsolation` |
| Privilege | unprivileged user namespace (bwrap) | `TestUnprivilegedUserNamespace` |
| Memory / PIDs / CPU | `memory.max` (no swap), `pids.max` (bwrap only for a fork bomb, see [Limitations](#limitations)), `cpu.max` | `TestMemoryBombKilled`, `TestForkBombContained`, `TestSystemdUnitLimitsAndTeardown` |
| Control socket | per-sandbox bind | `TestControlSocketIsolation` |
| Network | no interfaces by default | `TestNetworkNone` |
| Secrets | runner credentials never injected | `TestNoSecretsLeak` |

`make test-security-all` runs the matrix against both bwrap and gVisor, except
`TestForkBombContained`, which is bwrap-only.

## Limitations

- **Not production-ready.** It has had no security audit, and the runner's Unix
  socket has no authentication.
- **`NETWORK_FULL` and `NETWORK_PRIVATE` are bwrap-only.** `NETWORK_RESTRICTED`
  works on both backends ([ADR 017](docs/adr/017-gvisor-restricted-egress.md)).
- **Restricted egress is TLS-only, and DNS is visible.** Plaintext HTTP and
  ports other than 443 are denied. The runner's resolver answers only
  allowlisted names, but it sees which ones were asked for.
- **The broker is a route, not a transparent proxy.** Workloads call
  `$VORA_BROKER_URL/creds/<alias>/…`; a plain `curl https://api.github.com`
  gets no credential. The hop inside the sandbox's network namespace is
  plaintext, so a workload that logs broker responses sees what the API
  returned.
- **The bwrap seccomp filter is a deny-list** — hardening, not the isolation
  boundary ([ADR 018](docs/adr/018-bwrap-seccomp.md)).
- **Disk limits are hard only with an enforcing project quota.** That means XFS
  with `prjquota`, and a runner with `CAP_SYS_ADMIN` whose startup probe passes.
  Otherwise a periodic scan catches overuse, but a burst between scans can land
  first ([ADR 015](docs/adr/015-disk-quota.md)).
- **Images are trusted unless `VORA_IMAGE_POLICY=enforce`**, which requires
  digest-pinned, `cosign`-verified images
  ([ADR 020](docs/adr/020-image-signing.md)). Custom images must contain
  `/workspace` and `/vora`, and `/etc/resolv.conf` for restricted networking.
- **The production system unit offers no gVisor.** It runs as a non-root `vora`
  user, and `runsc` needs root or a rootlesskit wrapper
  ([ADR 011](docs/adr/011-rootless-containerd.md)).
- **On gVisor, `pids` doesn't limit guest processes.** It counts the Sentry's
  host threads: 800 guest processes held `pids.current` at 38 on vora-mini. That
  leaves `memory.max` to stop a fork bomb, which 6 logged runs saw at a 256 MiB limit.
  Reaching `pids` ends the whole sandbox. A limit of 26 or less can't start,
  and 28 to 36 died under load ([ADR 028](docs/adr/028-warm-pools.md)).
- **Warm pools are new.** Through a runner, the leak suite has passed on `bwrap` and
  gVisor on vora-mini, and on `bwrap` in CI. CI can't run the gVisor rows. No logged
  run has measured a pool under load.
- **Sandboxes outlive a dead runner briefly**, then exit; orphans are cleaned
  up at the next start.
- **The API store is in memory by default.** Set `VORA_API_DSN` for anything
  durable.

## Install and release

Every `v*` tag publishes static `linux/amd64` binaries (`vora`, `vora-runner`,
`vora-init`, `vora-api`), `SHA256SUMS`, and a tarball with the systemd units.
It also publishes the rootfs images to GHCR as
`ghcr.io/vorash/vora-{base,python,node}`.

```bash
gh release download v0.10.0 --pattern 'vora_*_linux_amd64.tar.gz' --pattern SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf vora_v0.10.0_linux_amd64.tar.gz
# From a source checkout, with the downloaded binaries in $PWD:
sudo ./scripts/install-system.sh --from "$PWD" --api
```

The system runner runs as a dedicated `vora` user with a lingering user
manager and its own rootless containerd. [docs/release.md](docs/release.md)
covers the host prerequisites and migrating an existing root install.
[vora-ansible](https://github.com/vorash/vora-ansible) sets real hosts up end
to end.

### Images

On a server, pull the rootfs images into the runner's containerd. Pin them by
digest, because a tag can move and a digest can't. The packages are private,
so you need a PAT with `read:packages`. As the `vora` user:

```bash
ctr --address /run/user/<vora-uid>/containerd/containerd.sock \
  --namespace vora images pull --user <github-user>:<PAT> \
  ghcr.io/vorash/vora-python@sha256:<digest from the release>
```

Don't pass `--snapshotter`; the runner unpacks images itself. If a create fails
with `image not found in cache` for a registry reference, the *pull* failed
(credentials or digest), not a local build. [docs/release.md](docs/release.md)
has the details.

## Development

You need:

- Go 1.27+
- `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc`
- Docker, to build images
- Bubblewrap, and `runsc` for gVisor
- `rootlesskit`, `slirp4netns` and `fuse-overlayfs`
- a systemd user session, systemd ≥ 254

```bash
make binaries          # bin/{vora,vora-runner,vora-init,vora-api} (static)
make test              # unit tests
make test-integration  # runner + sandbox lifecycle over a real socket
make test-security-all # isolation matrix, bwrap and gVisor
make test-sdk          # TypeScript SDK
make help              # everything else
```

CI runs the unit, bwrap integration and security tests on every PR. On `main`,
the `System unit` workflow also boots the production unit on Ubuntu 26.04.
Every environment variable is listed in
[docs/configuration.md](docs/configuration.md).

```text
api/proto/v1/   protobuf contracts          cmd/        vora, vora-runner, vora-init, vora-api
internal/       runtime, api, runner, cli   sdk/        typescript, python, mcp
integrations/   OpenAI Agents, TypeSafe JEV examples/   runnable recipes
images/         rootfs images               deploy/     systemd units
tests/          integration, security, bench docs/      ADRs, architecture, benchmarks
```

## License

MIT — see [LICENSE](LICENSE).
