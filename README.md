# Vora

Vora runs untrusted code — AI agents, generated scripts, code you would not run
on your own machine — in isolated Linux sandboxes, and keeps a record of what it
did.

Start a sandbox from an OCI image, `exec` into it, cap its memory/CPU/PIDs, cut
its network off by default, and read back every lifecycle event, execution and
network decision. Isolation is Bubblewrap (namespaces plus cgroup v2 through
systemd) or gVisor (`runsc`), chosen per sandbox; the rootfs comes from containerd
snapshots. Written in Go, with no Kubernetes in the path.

> **Status: v0.7, a proof of concept.** What is described here is implemented and
> covered by tests, but it has not been independently audited and is not ready to
> be trusted with anything that matters. [Limitations](#limitations-honest) lists
> what is missing on purpose and what is a hard v0.x boundary;
> [Roadmap](#roadmap) is what comes next.

## What works today

- A Go runner daemon (`vora-runner`) that manages the full sandbox lifecycle
  over a gRPC Unix socket, with per-sandbox **backend selection**.
- **Two isolation backends** behind one `runtime.Backend` interface:
  - **Bubblewrap** — user/PID/net/IPC/UTS/mount namespaces + cgroup v2 limits
    via systemd, read-only shared rootfs. Unprivileged, default local backend.
  - **gVisor (`runsc`)** — a user-space kernel (KVM platform), driven directly
    via gVisor **Method 2** inside a persistent `rootlesskit` user namespace.
- A long-lived **in-sandbox init** (`vora-init`, PID 1) on both backends: the
  exec agent for bwrap, a keep-alive process for gVisor (which execs natively).
  It is Vora's own static binary, not the image's, so **shell-less (distroless)
  images** boot on either backend.
- **Default-deny networking**. `NetworkNone` (default) gives a sandbox no
  interfaces or routes. **`NetworkRestricted`** (bwrap) DNATs non-loopback TCP
  through an SNI-inspecting passthrough proxy and **drops every other packet**
  (including UDP and direct gateway services). Allowed and denied decisions are
  persisted, metered, and fail closed if an allow cannot be audited. See
  [ADR 012](docs/adr/012-egress-gateway.md).
- **Allowlist-only DNS** for restricted sandboxes. The gateway resolver answers
  only allowlisted names (everything else is `NXDOMAIN` before the host resolver
  is consulted), refuses answers that map to loopback/private/metadata
  addresses, refuses non-address query types, and audits every query. A workload
  with a hardcoded resolver is redirected to it rather than silently dropped.
- **`NetworkFull` and `NetworkPrivate`** (bwrap, [ADR 016](docs/adr/016-network-modes.md)).
  `NetworkFull` routes public egress through the runner while keeping private
  ranges (RFC1918, link-local/metadata, loopback) and every other sandbox
  blocked. `NetworkPrivate` gives a tenant a shared `/24` so its sandboxes reach
  each other, with public egress and all other private ranges denied. Both keep
  per-sandbox network namespaces; the two modes are bwrap-only (gVisor remains
  `NetworkNone`).
- **Brokered credentials** ([ADR 013](docs/adr/013-credential-brokering.md)).
  `vora secrets set` stores a third-party token encrypted under `VORA_MASTER_KEY`
  in the control plane; a sandbox created with `--broker <alias>` calls
  `http://$VORA_BROKER_URL/creds/<alias>/<path>` and the gateway attaches the
  credential upstream over TLS. The value never enters the sandbox environment,
  filesystem, `/proc`, the persisted sandbox record, or the audit trail (which
  records the alias). The broker only ever talks to the alias's bound,
  allowlisted host and never follows a redirect.
- **Disk quota** (`DiskBytes`). A sandbox's writable directories are limited by
  a filesystem **project quota** when the host supports one; writes past the
  limit fail at the kernel boundary and the sandbox is stopped with a
  `DiskQuotaExceeded` reason. The runner probes enforcement at startup (XFS is
  the verified filesystem) and otherwise falls back to a periodic usage scan —
  see [Limitations](#limitations-honest).
- State persistence, a **TTL reaper**, and startup **reconciliation** (incl.
  reaping orphaned `runsc` containers and systemd units).
- **Snapshots and forks** ([ADR 023](docs/adr/023-snapshot-fork.md)).
  `vora snapshot create` captures a sandbox's writable state live and
  crash-consistent while it runs; `vora snapshot fork` starts independent
  sandboxes from the copy — the fan-out primitive for parallel agents.
  Snapshots outlive their source sandbox, and a fork restores them as what
  they are (workload-authored archives: never followed, never able to write
  outside its own writable roots).
- **Audit trail** behind a `Store` interface: local JSONL, or **PostgreSQL /
  Supabase** (`VORA_AUDIT_DSN`), with lifecycle events, per-execution records,
  and restricted-network decisions (`vora logs --egress`).
- **Prometheus metrics** at `127.0.0.1:9101/metrics`, and **OpenTelemetry
tracing** across API → runner → runtime (off unless
`OTEL_EXPORTER_OTLP_ENDPOINT` is set), with trace ids in logs and an
`X-Trace-Id` response header ([ADR 019](docs/adr/019-opentelemetry.md)).
- A dual-backend **isolation security suite** and a **benchmark harness**.
- A **control plane** (`vora-api`, [ADR 014](docs/adr/014-control-plane.md)):
  HTTP/JSON API with hashed, scoped **API keys**, **strict tenant isolation**,
  and a gRPC runner registry that runners **dial out** to (`Register`/`Heartbeat`,
  join token). Tenant keys can only access their own sandboxes and keys; runner
  inventory requires a global administrator. The CLI drives either the local
  socket or, with `--api`, the control plane.
- **External CLI plugins.** An unknown `vora <name> …` runs `vora-<name>` from
  `$PATH` (the `git`/`kubectl` pattern), so tools such as `vora-probe` read as
  part of Vora without Vora depending on them. Built-in commands always win, and
  `VORA_DISABLE_PLUGINS=1` turns dispatch off.
- **Agent-facing tooling.** A TypeScript SDK for the HTTP API
  (`sdk/typescript`), a Python SDK (`sdk/python`), an MCP server that exposes
  sandboxes to coding agents
  (`sdk/mcp`), an OpenAI Agents adapter that runs agent code inside
  sandboxes (`integrations/openai-agents`), and a TypeSafe JEV decision-model
  tool for agents (`integrations/typesafe-jev`, ADR 024); `vora exec` forwards
  stdin to the command it runs.

## Architecture

```text
                         vora (CLI)
                             │  gRPC over unix socket
                             ▼
                    ┌─────────────────────┐
                    │    vora-runner      │  runtime/vora/runner.sock
                    │  (rootlesskit userns)│
                    │ sandbox state mch.  │  state/<sandbox>/
                    │ TTL reaper          │
                    │ reconciler          │
                    │ runtime.Registry    │
                    └─────────┬───────────┘
                              │ runtime.Backend (auto-selected)
              ┌───────────────┴───────────────┐
              ▼                               ▼
      gvisor backend                   bwrap backend
      (runsc Method 2, KVM)            (namespaces + cgroup v2)
              │                               │
              │  runsc create/start/exec      │  systemd-run --user -p MemoryMax… bwrap
              ▼                               ▼
      ┌────────────────────┐        ┌────────────────────────────┐
      │ gVisor sandbox     │        │ bubblewrap sandbox          │
      │  user-space kernel │        │  --unshare-all              │
      │  ro rootfs         │        │  ro rootfs, rw /workspace    │
      │  rw /workspace     │        │  vora-init (PID 1, agent)    │
      └────────────────────┘        └────────────────────────────┘
```

The control plane never depends on the isolation technology: every backend
implements a small `runtime.Backend` interface (see
[ADR 001](docs/adr/001-runtime-interface.md)).

## Security model

Vora assumes all sandboxed code is malicious. The threat model and exact
boundaries are in [docs/architecture](docs/architecture/). The boundary matrix:

| Boundary | Mechanism | Test |
| -------- | --------- | ---- |
| Host filesystem | read-only image rootfs; host paths never mounted | `TestFilesystemIsolation` |
| Rootfs writes | `--ro-bind` root; only `/workspace`, `/root`, `/tmp` writable | `TestFilesystemIsolation` |
| Cross-sandbox files | distinct mount namespaces; no shared writable paths | `TestCrossSandboxFilesystemIsolation` |
| Processes / signals | PID namespace | `TestPIDNamespaceIsolation`, `TestCannotSignalHost` |
| IPC | IPC namespace | `TestIPCNamespaceIsolation` |
| UTS | private hostname | `TestUTSNamespaceIsolation` |
| Privilege | unprivileged user namespace (bwrap) | `TestUnprivilegedUserNamespace` |
| Memory | `memory.max` (+ `MemorySwapMax=0`) | `TestMemoryBombKilled` |
| Processes | `pids.max` | `TestForkBombContained` |
| CPU | `cpu.max` | `TestSystemdUnitLimitsAndTeardown` |
| Control socket | per-sandbox ipc bind | `TestControlSocketIsolation` |
| Network | `--unshare-net` / `runsc --network none` | `TestNetworkNone` |
| Secrets | runner credentials never injected into the sandbox | `TestNoSecretsLeak` |

`tests/security/` is a deliverable, not an afterthought: each test encodes *why*
the boundary matters and was verified to fail when the corresponding flag is
removed. `make test-security-all` runs the whole matrix against **both** bwrap
and gVisor (the gVisor leg inside a `rootlesskit` user namespace).

## Demo

```console
$ vora sandbox create --image vora/python --memory 512m --cpu 1 --pids 128 --network none
Sandbox created: sbx_01M2NVHE1KRD8JT3D6QG6R0EW3

$ vora exec sbx_01M2NVHE1KRD8JT3D6QG6R0EW3 -- python3 -c 'print("hello from vora")'
hello from vora

$ vora exec sbx_01M2NVHE1KRD8JT3D6QG6R0EW3 -- python3 -c 'import socket; s=socket.socket(); s.settimeout(2); print("169.254.169.254 =>", "REACHABLE" if s.connect_ex(("169.254.169.254",80))==0 else "BLOCKED")'
169.254.169.254 => BLOCKED

$ vora stats sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
memory:    9777152 bytes
peak:      19267584 bytes
cpu:       182 millis
pids:      11
oom kills: 0
pid limit: 0

$ vora sandbox destroy sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
Destroyed sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
```

### Snapshot and fork

```console
$ vora snapshot create sbx_01M2NVHE1KRD8JT3D6QG6R0EW3
Snapshot created: snap_01M2PE4G7QW3X8Y5Z6A7B8C9D0 (18432 bytes)

$ vora snapshot fork snap_01M2PE4G7QW3X8Y5Z6A7B8C9D0
Sandbox forked: sbx_01M2PF2K4M6N8P0Q2R4S6T8V0W

$ vora exec sbx_01M2PF2K4M6N8P0Q2R4S6T8V0W -- ls /workspace
main.py
```

The fork inherits the source's spec verbatim and sees the writable state as of
the snapshot; its writes are invisible to its siblings from the first one.

### Brokered credentials

```console
$ export VORA_MASTER_KEY="$(openssl rand -base64 48)"   # vora-api only
$ vora-api & vora-runner &

$ vora keys create --name operator --scope admin          # prints the key once
$ export VORA_ADMIN_KEY="vora_..."

# A global-admin key names the tenant it acts on; a tenant-scoped key does not.
$ vora --api http://127.0.0.1:9300 --api-key "$VORA_ADMIN_KEY" \
      keys create --name tenant-a --tenant t1            # sandbox scope by default
key:    vora_...
$ export VORA_TENANT_KEY="vora_..."

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_ADMIN_KEY" \
      secrets set --tenant t1 --alias github --host api.github.com --value-stdin <<<"$GITHUB_TOKEN"
secret github bound to api.github.com (bearer)

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_TENANT_KEY" sandbox create \
      --image vora/python --network restricted --allow-domain api.github.com --broker github
Sandbox created: sbx_01M2NVHE1KRD8JT3D6QG6R0EW3

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_TENANT_KEY" exec sbx_... -- \
      python3 -c 'import os,urllib.request; print(urllib.request.urlopen(os.environ["VORA_BROKER_URL"]+"/creds/github/user").status)'
200

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_TENANT_KEY" logs sbx_... --egress
2026-09-17T21:00:00Z  allowed alias=github api.github.com
```

Inside that sandbox, `env`, `/proc/self/environ`, and the filesystem contain no token
and no alias secret — only the non-secret `VORA_BROKER_URL`.

**Keys and scopes.** `admin` covers keys, secrets and runner inventory, and also
permits sandbox routes; `sandbox` covers sandboxes, their logs and their stats
only. Create the operator key with `--scope admin` and it can do both. A key is
pinned to the tenant it was created for, and a global-admin key must name that
tenant (`--tenant t1`) for secrets and for new keys — `vora keys list` prints
every key's `tenant_id`, which is how a global operator discovers the tenants in
use. `VORA_MASTER_KEY` is required for `vora secrets`; without it the endpoints
answer `503` and brokering stays off.

### External plugins

An otherwise-unknown subcommand runs as `vora-<name>` on `$PATH`:

```console
$ vora probe replay test/fixtures/oom --out .probe   # runs vora-probe
```

Arguments, stdin/stdout/stderr and the exit code pass through unchanged, and
`vora probe --help` reaches the plugin so the plugin owns its own help. Built-in
commands always win over a plugin of the same name, `vora --help` lists
discovered plugins under **External plugins**, and `VORA_DISABLE_PLUGINS=1`
disables dispatch entirely.

Dispatch uses `exec.LookPath` on a strictly validated name
(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, at most 64 bytes): no shell, no
config-supplied command line, and no name that can escape the `vora-` prefix. It
has the same trust model as `git` and `kubectl` — anything that can plant a
`vora-<name>` earlier in your `$PATH` could already run code as you, so dispatch
adds no new path. See [the threat model](docs/architecture/threat-model.md).

Vora's own companion binaries are excluded from dispatch: `vora api`,
`vora runner` and `vora init` report an unknown command rather than launching
`vora-api`, `vora-runner` or `vora-init` — run those binaries directly.

## Testing the sandbox images

`vora-base`, `vora-python` and `vora-node` are ordinary OCI images. What makes
them *vora* images is `/workspace` and `/vora` (the runner needs both) and a
`/etc/resolv.conf` in the layers — the restricted path binds a resolver config
over that path, and refuses to start an image that has none. Debian 13, Python
3.13 and Node 22 at the time of writing.

The steps below go from an empty containerd to a sandbox you have checked
yourself. They use `vora/python`; `vora/base` and `vora/node` work the same way,
and they assume a runner is up (`make install-rootless` for the dev service, see
[Development](#development)).

**1. Put the image where the runner looks.** From a checkout:

```bash
make images         # docker build of images/{base,python,node}
make pull-images    # import them into containerd, namespace vora
```

On a host with the released binaries, pull the published image instead, pinned by
digest — `:v0.4.2` can be moved, a digest cannot:

```bash
sudo ctr --namespace vora images pull \
  --user <github-user>:<PAT with read:packages> \
  ghcr.io/vorash/vora-python@sha256:56c7b3e0f252a34b6f977f94fd03ce8eefffa6af8a2f355f489ecd25d2538c94
```

Do not add `--snapshotter`: on ctr 2.x it fails with `unable to initialize
unpacker: no unpack platforms defined`, and it is not needed — the runner unpacks
into its own snapshotter from the local content store. Private packages and what a
failed pull looks like are in [docs/release.md](docs/release.md).

**2. Create a sandbox and look inside.**

```console
$ vora sandbox create --image vora/python --memory 256m --cpu 1 --pids 128 --network none
Sandbox created: sbx_01M2T40M8YQQDNNP4M17AFCPHX

$ vora exec sbx_... -- python3 -c 'import sys, platform; print("python", sys.version.split()[0], "on", platform.platform())'
python 3.13.15 on Linux-4.19.0-gvisor-x86_64-with-glibc2.41

$ vora exec sbx_... -- sh -c 'id; grep VERSION_ID /etc/os-release; ls /vora /workspace'
uid=0(root) gid=0(root) groups=0(root)
VERSION_ID="13"
/vora:

/workspace:
```

That `uid=0` is root inside the sandbox's user namespace, not on the host, and
`4.19.0-gvisor` is gVisor's synthetic kernel — this sandbox took the gVisor
backend (`--backend bwrap` forces Bubblewrap, `--backend auto` is the default).
Pass `vora/python`, not `python`: a bare name resolves to Docker Hub's own image.

**3. Check that the box actually holds.**

```console
$ vora exec sbx_... -- python3 -c 'import socket; s=socket.socket(); s.settimeout(2); print("metadata:", "REACHABLE" if s.connect_ex(("169.254.169.254", 80)) == 0 else "BLOCKED")'
metadata: BLOCKED

$ vora exec sbx_... -- sh -c 'ip -o addr show | wc -l'
0

$ vora exec sbx_... -- touch /etc/foo
touch: cannot touch '/etc/foo': Read-only file system
```

`--network none` leaves the sandbox with no interfaces at all, and the rootfs is
read-only everywhere except `/workspace`, `/root` and `/tmp`.

**4. Watch it, then clean up.**

```console
$ vora stats sbx_...
memory:    51187712 bytes
peak:      62246912 bytes
cpu:       152 millis
pids:      30

$ vora logs sbx_...
2026-09-18T13:20:02+02:00  REQUESTED -> SCHEDULING
2026-09-18T13:20:02+02:00  SCHEDULING -> CREATING
2026-09-18T13:20:02+02:00  CREATING -> STARTING
2026-09-18T13:20:03+02:00  STARTING -> RUNNING

$ vora sandbox destroy sbx_...
Destroyed sbx_01M2T40M8YQQDNNP4M17AFCPHX
```

**When it does not work.** The first failure is usually this one:

```text
vora: sandbox sbx_... created but failed to start: rpc error: code = FailedPrecondition
desc = images: image not found in cache: ghcr.io/vorash/vora-python:v0.4.2 (run 'make images')
```

It is misleading. For a registry ref it means the *pull* failed — private
package, missing or wrong credentials, wrong digest — not that a local image is
missing; [docs/release.md](docs/release.md) has the credential setup. Restricted
networking, the broker and the gateway resolver need root or a delegated cgroup
subtree, so a rootless dev runner refuses them by design. Everything above works
without that.

## Repository layout

```text
api/proto/v1/   protobuf contracts (runner + in-sandbox agent)
cmd/            vora (CLI), vora-runner (daemon), vora-init (sandbox agent), vora-api
internal/       runtime, cgroup, images, workspace, sandbox, runner, cli, telemetry
docs/           architecture docs, ADRs and benchmark reports
tests/          integration, security and benchmark suites
examples/       real-world recipes: CI gates, autograders, plugin runners, fan-out, agent tools
images/         rootfs image definitions (base, python, node)
sdk/typescript/ TypeScript SDK for the HTTP API
sdk/python/    Python SDK for the HTTP API (PyPI: vorash, import: vora)
sdk/mcp/        MCP server exposing sandboxes to coding agents
integrations/   adapters for agent frameworks (OpenAI Agents, TypeSafe JEV)
scripts/        image build and benchmark helpers
deploy/         systemd units for the system install (vora-ansible is its own repo)
```

## Development

Requirements: Go 1.27+, `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc`,
Docker (to build images), Bubblewrap, `runsc` (gVisor), `rootlesskit`,
`slirp4netns`, `fuse-overlayfs`, and a systemd user session (systemd ≥ 254,
for `systemd-run --expand-environment=no`).

```bash
make images            # build the read-only rootfs images with Docker
make pull-images       # import those images into containerd (namespace vora)
make binaries          # build bin/{vora,vora-runner,vora-init} (static)
make test              # unit tests
make test-integration  # runner + sandbox lifecycle over a real socket (incl. Postgres via testcontainers)
make test-security     # isolation suite (bwrap; gVisor subtests skipped unprivileged)
make test-security-all # isolation matrix against bwrap AND gVisor
make test-gvisor       # gVisor integration suite incl. a shell-less image (rootlesskit)
make test-network      # NetworkRestricted netns/DNAT test (rootlesskit)
make test-broker       # credential-broker boundary + end-to-end test (rootlesskit)
make test-dns          # gateway resolver test (rootlesskit)
make bench-report      # benchmarks + report in docs/benchmarks/
make test-sdk          # TypeScript SDK: build, typecheck, vitest
make help              # all targets
```

GitHub Actions (`.github/workflows/ci.yml`) runs unit tests, then bwrap
integration and security tests on `ubuntu-latest` (plus the TypeScript SDK
suite). gVisor is skipped there (no KVM / rootlesskit userns on hosted runners).

Install and start the rootlesskit-wrapped runner (required for gVisor):

```bash
make install-rootless   # user systemd service: vora-runner.service
vora sandbox create --image vora/base --backend gvisor
```

The runner exposes Prometheus metrics at `127.0.0.1:9101/metrics` and a health
check at `/healthz` (override with `VORA_METRICS_ADDR`). Restricted egress is
bounded by `VORA_EGRESS_MAX_CONNECTIONS` (default 128) and
`VORA_EGRESS_IDLE_TIMEOUT` (default 5m); the broker listens on
`VORA_EGRESS_BROKER_PORT` (default 8080).

Configuration is via environment variables: `VORA_STATE_DIR`, `VORA_SOCKET`,
`VORA_SYSTEMD_MODE` (`user`/`system`), `VORA_METRICS_ADDR`, `VORA_INIT_BINARY`,
`VORA_GVISOR_PLATFORM` (`kvm`/`systrap`), `VORA_GVISOR_ROOT`, `VORA_RUNSC`,
`VORA_CONTAINERD_ADDRESS`, `VORA_CONTAINERD_NAMESPACE` (default `vora`),
`VORA_CONTAINERD_SNAPSHOTTER` (default `native`), `VORA_IMAGES_MODE`
(`auto`/`containerd`/`cache`), and `VORA_AUDIT_DSN` (PostgreSQL/Supabase;
omitted → local JSONL).

Network-restricted sandbox:

```bash
vora sandbox create --image vora/python --network restricted \
  --allow-domain example.com --backend bwrap
```

Brokered credentials (requires `--api`; see [ADR 013](docs/adr/013-credential-brokering.md)):

```bash
export VORA_MASTER_KEY="$(openssl rand -base64 48)"   # vora-api only
vora --api https://api.example:9300 --api-key "$VORA_API_KEY" \
  secrets set --alias github --host api.github.com --value-stdin < token.txt
vora --api https://api.example:9300 --api-key "$VORA_API_KEY" sandbox create \
  --image vora/python --network restricted --allow-domain api.github.com --broker github
```

Control plane (remote HTTP API):

```bash
vora-api &                       # HTTP :9300, registry gRPC :9301
vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" runners
vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" keys create --name ci
vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" sandbox create --image vora/base
```

Control-plane configuration: `VORA_API_HTTP_ADDR`, `VORA_API_GRPC_ADDR`,
`VORA_API_DSN` (PostgreSQL; omitted → in-memory), `VORA_API_BOOTSTRAP_KEY`
(initial admin key), `VORA_API_JOIN_TOKEN` (runner registration), and on the
runner `VORA_RUNNER_ADDR` (TCP gRPC), `VORA_RUNNER_ADVERTISE_ADDR`,
`VORA_API_GRPC`, `VORA_API_JOIN_TOKEN`. Runner resource ceilings use
`VORA_MAX_MEMORY` (bytes), `VORA_MAX_CPU` (millicores), `VORA_MAX_PIDS`,
`VORA_MAX_TTL` (duration), `VORA_DEFAULT_DISK` (bytes), and
`VORA_MAX_SNAPSHOT_BYTES` (bytes). Credential brokering is enabled by
`VORA_MASTER_KEY` (≥32 bytes, `vora-api` only); brokered creates also require
TLS on the API→runner channel unless `VORA_ALLOW_INSECURE_BROKER=1`. TLS/mTLS is
enabled per channel with
`VORA_API_HTTP_TLS_{CERT,KEY,CA}` (external HTTP/CLI),
`VORA_API_REGISTRY_TLS_{CERT,KEY,CA}` (runner registry), and
`VORA_RUNNER_TLS_{CERT,KEY,CA}` (API→runner). Runner TCP requires all three
(mTLS); the API client certificate must carry DNS SAN `vora-api` (or the
`VORA_RUNNER_CLIENT_IDENTITY` override). For local development only, plaintext
runner TCP requires `VORA_RUNNER_INSECURE=1` and a loopback IP bind. Other
channels retain their own TLS settings. `VORA_API_TLS_*` is no longer read. A tunnel bound to loopback may use
`VORA_TUNNEL_INSECURE=1` for local development, but then brokered credentials
are refused by default.

## Releases

A `v*` tag produces a GitHub release with static `linux/amd64` binaries
(`vora`, `vora-runner`, `vora-init`, `vora-api`), `SHA256SUMS`, and a tarball
that also contains `LICENSE` and the systemd units. The same workflow publishes
the sandbox rootfs images to GHCR as `ghcr.io/vorash/vora-{base,python,node}`,
which the runner pulls by reference. Those packages are private by default, so a
host pre-pulls them with a PAT — see [docs/release.md](docs/release.md) and
[Testing the sandbox images](#testing-the-sandbox-images). Re-run it for an existing tag with
`gh workflow run release.yml -f version=v0.4.0`.

```console
$ gh release download v0.4.0 --pattern 'vora_*_linux_amd64.tar.gz'
$ gh release download v0.4.0 --pattern 'SHA256SUMS'
$ sha256sum --check --ignore-missing SHA256SUMS
$ tar -xzf vora_v0.4.0_linux_amd64.tar.gz

# System installation requires the dedicated vora user, its lingering user
# manager, and its rootless containerd socket; see docs/release.md.
$ # From a source checkout with the downloaded binaries in this directory:
$ sudo ./scripts/install-system.sh --from "$PWD" --api
```

From a checkout the equivalents are `make release-artifacts` and
`sudo make install-system API=1` (which also writes the `/etc/vora/*.env`
templates; existing root-owned state and system containerd sockets must be
migrated before installation). Host convergence for real machines is
[`vora-ansible`](https://github.com/vorash/vora-ansible);
[docs/release.md](docs/release.md) covers what a release contains, the image
requirement (`/etc/resolv.conf` must exist in the image layers for
`NetworkRestricted`), and the package-visibility caveat for GHCR.

## Limitations (honest)

- **Images resolve from containerd snapshots (ADR 009).** OCI images are pulled
  and unpacked into the rootless containerd `native` snapshotter and the
  snapshot rootfs is bound read-only; the Docker-export cache remains only as a
  fallback when containerd is unavailable.
- **`NetworkFull`/`NetworkPrivate` are bwrap-only.** `NetworkRestricted` is
  enforced for **both** backends: bwrap via the sandbox-netns DNAT, gVisor via a
  runner-netns prerouting DNAT (ADR 017). gVisor's `NetworkFull`/`NetworkPrivate`
  and its `NetworkNone`-only history through v0.4 are covered in
  [ADR 016](docs/adr/016-network-modes.md) / [ADR 017](docs/adr/017-gvisor-restricted-egress.md).
- **DNS is allowlist-only, and the resolver sees the names.** Restricted
  sandboxes resolve through the gateway; only allowlisted names are answered,
  but the runner does learn which of them were asked for.
- **The brokered hop inside the netns is plaintext HTTP.** The credential is
  attached by the gateway, so it still never enters the sandbox — but a workload
  that logs its own broker responses sees whatever the API returned.
- **Brokering is a broker route, not a transparent MITM.** Workloads must call
  `$VORA_BROKER_URL/creds/<alias>/…`; unmodified `curl https://api.github.com`
  is not rewritten, non-HTTP protocols are not brokered, and brokered creates
  are refused unless the API-to-runner channel is encrypted (or the operator
  sets `VORA_ALLOW_INSECURE_BROKER=1`).
- **Non-TLS egress is denied.** The proxy only understands TLS SNI; plaintext
  HTTP and non-443 ports are blocked by default.
- **bwrap syscall filtering is a deny-list.** `vora-init` installs a seccomp
  filter (`EPERM` for ptrace, the mount API, module/kexec, bpf, keyrings, …)
  before the sandbox serves, so every workload inherits it. It is hardening, not
  a containment boundary — namespaces, cgroup limits, and gVisor remain the
  isolation ([ADR 018](docs/adr/018-bwrap-seccomp.md)). gVisor provides its own
  syscall surface.
- **Disk quota is only hard with an enforcing project quota.** `DiskBytes` is
  enforced by a filesystem project quota when the sandboxes directory is
  mounted with `prjquota`, the runner can run `chattr`/`setquota` (root), and a
  startup probe confirms the kernel actually refuses writes past the limit.
  XFS is the verified filesystem; ext4 on some kernels accounts the quota
  without enforcing it, and the runner then falls back to the periodic usage
  scan, which bounds the *sandbox* but not a burst that lands between scans.
  The "host unaffected" guarantee holds only when the probe passes.
- **Images are trusted unless a signature policy is set.** `VORA_IMAGE_POLICY=off`
  (the default) trusts every image and logs a warning at startup;
  `VORA_IMAGE_POLICY=enforce` requires a digest-pinned reference and containerd,
  verifies that digest with `cosign verify --key` before pull, and refuses
  unsigned images and unauthenticated cache fallback
  ([ADR 020](docs/adr/020-image-signing.md)).
- **The image must provide mount points.** Rootfs images include `/workspace`
  and `/vora`; custom images must create them.
- **Sandboxes survive runner death briefly**, then exit via the agent watchdog /
  runsc teardown; orphaned units and containers are cleaned on the next start.
- **Registry and HTTP channels can be plaintext by default.** Registration is
  gated by a join token; runner TCP requires mTLS or an explicitly opted-in
  loopback development bind. A public reverse tunnel requires mTLS using
  `VORA_API_TUNNEL_TLS_{CERT,KEY,CA}` on both ends; its runner certificate DNS
  SAN must match the runner ID. A runner behind NAT can still be driven by setting
  `VORA_API_TUNNEL_ADDR` on both sides, so the runner connects out and the API
  multiplexes over a reverse tunnel ([ADR 022](docs/adr/022-reverse-tunnel.md));
  capacity-aware placement is in [ADR 021](docs/adr/021-capacity-placement.md),
  and the default API store is in-memory
  ([ADR 014](docs/adr/014-control-plane.md)).
- **Not production-ready.** No auth on the runner Unix socket, and no security
  audit.

## Roadmap

| Version | Scope |
| ------- | ----- |
| v0.1 | Go runner, Bubblewrap backend, cgroups v2, in-sandbox exec agent, CLI, security suite, benchmarks |
| v0.2 | gVisor backend (direct `runsc`, Method 2), backend selection, rootlesskit runner service, PostgreSQL/Supabase audit, dual-backend isolation matrix |
| **v0.3** | containerd-snapshot rootfs (ADR 009), bwrap `NetworkRestricted` with filter+audit (ADR 012), `vora-api` with tenant isolation, split TLS, and failure-safe create (ADR 014) |
| **v0.4** | Allowlist-only gateway DNS (ADR 012) and credential brokering (ADR 013) for restricted bwrap sandboxes. gVisor egress recorded as a documented limitation (closed in v0.5) |
| v0.5 | External CLI plugin dispatch, API files routes, `DiskBytes` project quota (ADR 015), `NetworkFull`/`NetworkPrivate` (ADR 016), gVisor `NetworkRestricted` (ADR 017), enriched execution traces |
| v0.6 | bwrap seccomp deny-list (ADR 018), OpenTelemetry (ADR 019), image signing (ADR 020), capacity-aware placement (ADR 021), reverse tunnel for NAT'd runners (ADR 022) |
| **v0.7 (this)** | **TypeScript SDK (`sdk/typescript`), MCP server (`sdk/mcp`), OpenAI Agents adapter (`integrations/openai-agents`), stdin forwarding for `vora exec`** |
| v0.8 | Sandbox snapshots and forks ([ADR 023](docs/adr/023-snapshot-fork.md)) |
| v1.0 | Firecracker microVM tier |

Host deployment is [vora-ansible](https://github.com/vorash/vora-ansible).

## License

MIT — see [LICENSE](LICENSE).
