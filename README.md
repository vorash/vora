# Vora

**Secure execution infrastructure for autonomous agents.**

Vora runs untrusted AI agents and generated code inside isolated Linux
environments with resource controls, default-deny networking, and a complete
execution audit trail.

Built with Go, Bubblewrap, Linux namespaces, cgroups v2 (via systemd), gRPC,
and Prometheus.

> **Status: v0.4 proof of concept.** Two isolation backends (Bubblewrap and
> gVisor), restricted bwrap egress with an allowlist-only resolver and
> credential brokering, and a `vora-api` control plane are functional and
> covered by tests. It is **not** production-ready and has not been
> independently audited. See [Roadmap](#roadmap) and
> [Limitations](#limitations-honest).

## What works today

- A Go runner daemon (`vora-runner`) that manages the full sandbox lifecycle
  over a gRPC Unix socket, with per-sandbox **backend selection**.
- **Two isolation backends** behind one `runtime.Backend` interface:
  - **Bubblewrap** — user/PID/net/IPC/UTS/mount namespaces + cgroup v2 limits
    via systemd, read-only shared rootfs. Unprivileged, default local backend.
  - **gVisor (`runsc`)** — a user-space kernel (KVM platform), driven directly
    via gVisor **Method 2** inside a persistent `rootlesskit` user namespace.
- A long-lived **in-sandbox agent** (`vora-init`, PID 1) for the bwrap backend;
  gVisor uses native `runsc exec`.
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
- **Brokered credentials** ([ADR 013](docs/adr/013-credential-brokering.md)).
  `vora secrets set` stores a third-party token encrypted under `VORA_MASTER_KEY`
  in the control plane; a sandbox created with `--broker <alias>` calls
  `http://$VORA_BROKER_URL/creds/<alias>/<path>` and the gateway attaches the
  credential upstream over TLS. The value never enters the sandbox environment,
  filesystem, `/proc`, the persisted sandbox record, or the audit trail (which
  records the alias). The broker only ever talks to the alias's bound,
  allowlisted host and never follows a redirect.
- State persistence, a **TTL reaper**, and startup **reconciliation** (incl.
  reaping orphaned `runsc` containers and systemd units).
- **Audit trail** behind a `Store` interface: local JSONL, or **PostgreSQL /
  Supabase** (`VORA_AUDIT_DSN`), with lifecycle events, per-execution records,
  and restricted-network decisions (`vora logs --egress`).
- **Prometheus metrics** at `127.0.0.1:9101/metrics`.
- A dual-backend **isolation security suite** and a **benchmark harness**.
- A **control plane** (`vora-api`, [ADR 014](docs/adr/014-control-plane.md)):
  HTTP/JSON API with hashed, scoped **API keys**, **strict tenant isolation**,
  and a gRPC runner registry that runners **dial out** to (`Register`/`Heartbeat`,
  join token). Tenant keys can only access their own sandboxes and keys; runner
  inventory requires a global administrator. The CLI drives either the local
  socket or, with `--api`, the control plane.

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
$ vora sandbox create --image python --memory 512m --cpu 1 --pids 128 --network none
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

### Brokered credentials

```console
$ export VORA_MASTER_KEY="$(openssl rand -base64 48)"   # vora-api only
$ vora-api & vora-runner &

$ vora keys create --name operator --scope admin          # prints the key once
$ export VORA_API_KEY="vora_..."

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" \
      secrets set --alias github --host api.github.com --value-stdin <<<"$GITHUB_TOKEN"
secret github bound to api.github.com (bearer)

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" sandbox create \
      --image python --network restricted --allow-domain api.github.com --broker github
Sandbox created: sbx_01M2NVHE1KRD8JT3D6QG6R0EW3

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" exec sbx_... -- \
      python3 -c 'import os,urllib.request; print(urllib.request.urlopen(os.environ["VORA_BROKER_URL"]+"/creds/github/user").status)'
200

$ vora --api http://127.0.0.1:9300 --api-key "$VORA_API_KEY" logs sbx_... --egress
2026-09-17T21:00:00Z  allowed alias=github api.github.com
```

Inside that sandbox, `env`, `/proc/self/environ`, and the filesystem contain no
token and no alias secret — only the non-secret `VORA_BROKER_URL`.

## Repository layout

```text
api/proto/v1/   protobuf contracts (runner + in-sandbox agent)
cmd/            vora (CLI), vora-runner (daemon), vora-init (sandbox agent), vora-api
internal/       runtime, cgroup, images, workspace, sandbox, runner, cli, telemetry
docs/           architecture docs, ADRs and benchmark reports
tests/          integration, security and benchmark suites
images/         rootfs image definitions (base, python, node)
scripts/        image build and benchmark helpers
deploy/         systemd units and Ansible roles (v0.2)
```

## Development

Requirements: Go 1.27+, `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc`,
Docker (to build images), Bubblewrap, `runsc` (gVisor), `rootlesskit`,
`slirp4netns`, `fuse-overlayfs`, and a systemd user session.

```bash
make images            # build the read-only rootfs cache as <name>/latest (Docker export; containerd-backed WIP)
make binaries          # build bin/{vora,vora-runner,vora-init} (static)
make test              # unit tests
make test-integration  # runner + sandbox lifecycle over a real socket (incl. Postgres via testcontainers)
make test-security     # isolation suite (bwrap; gVisor subtests skipped unprivileged)
make test-security-all # isolation matrix against bwrap AND gVisor
make test-network      # NetworkRestricted netns/DNAT test (rootlesskit)
make test-broker       # credential-broker boundary + end-to-end test (rootlesskit)
make test-dns          # gateway resolver test (rootlesskit)
make bench-report      # benchmarks + report in docs/benchmarks/
make help              # all targets
```

GitHub Actions (`.github/workflows/ci.yml`) runs unit tests, then bwrap
integration and security tests on `ubuntu-latest`. gVisor is skipped there
(no KVM / rootlesskit userns on hosted runners).

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
  --image python --network restricted --allow-domain api.github.com --broker github
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
`VORA_API_GRPC`, `VORA_API_JOIN_TOKEN`. Credential brokering is enabled by
`VORA_MASTER_KEY` (≥32 bytes, `vora-api` only); brokered creates also require
TLS on the API→runner channel unless `VORA_ALLOW_INSECURE_BROKER=1`. TLS/mTLS is
enabled per channel with
`VORA_API_HTTP_TLS_{CERT,KEY,CA}` (external HTTP/CLI),
`VORA_API_REGISTRY_TLS_{CERT,KEY,CA}` (runner registry), and
`VORA_RUNNER_TLS_{CERT,KEY,CA}` (API→runner). A server-side CA requires and
verifies client certificates. Unset means plaintext. `VORA_API_TLS_*` is no
longer read.

## Limitations (honest)

- **Images resolve from containerd snapshots (ADR 009).** OCI images are pulled
  and unpacked into the rootless containerd `native` snapshotter and the
  snapshot rootfs is bound read-only; the Docker-export cache remains only as a
  fallback when containerd is unavailable.
- **Egress, DNS, and brokering are bwrap-only (documented limitation, not
  pending work).** `NetworkRestricted` is enforced for the bubblewrap backend.
  gVisor supports `NetworkNone` only: its host network mode escapes to the
  runner namespace, and joining a prepared netns via the OCI spec makes
  netstack egress time out (ADR 012). Closing this would need a
  userspace-netstack gateway, which is out of scope for v0.x.
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
- **No syscall filtering for bwrap.** seccomp is not applied; Bubblewrap
  `--seccomp` is a planned hardening step. gVisor provides its own syscall
  surface.
- **Disk is not quota-enforced.** A sandbox can fill its workspace up to host
  free space.
- **Images are trusted.** No signature verification yet.
- **The image must provide mount points.** Rootfs images include `/workspace`
  and `/vora`; custom images must create them.
- **Sandboxes survive runner death briefly**, then exit via the agent watchdog /
  runsc teardown; orphaned units and containers are cleaned on the next start.
- **Control-plane channels are plaintext by default.** Registration is gated by
  a join token, and TLS/mTLS is available per channel
  (`VORA_API_HTTP_TLS_*`, `VORA_API_REGISTRY_TLS_*`, `VORA_RUNNER_TLS_*`), but
  it is off until configured. The API→runner hop needs the runner reachable (no
  NAT reverse tunnel yet), placement is first-healthy (no capacity-aware
  scheduling), and the default API store is in-memory
  ([ADR 014](docs/adr/014-control-plane.md)).
- **Not production-ready.** No auth on the runner Unix socket, first-healthy
  placement only, no independent security audit.

## Roadmap

| Version | Scope |
| ------- | ----- |
| v0.1 | Go runner, Bubblewrap backend, cgroups v2, in-sandbox exec agent, CLI, security suite, benchmarks |
| v0.2 | gVisor backend (direct `runsc`, Method 2), backend selection, rootlesskit runner service, PostgreSQL/Supabase audit, dual-backend isolation matrix |
| **v0.3** | containerd-snapshot rootfs (ADR 009), bwrap `NetworkRestricted` with filter+audit (ADR 012), `vora-api` with tenant isolation, split TLS, and failure-safe create (ADR 014) |
| **v0.4 (this)** | **Allowlist-only gateway DNS (ADR 012) and credential brokering (ADR 013) for restricted bwrap sandboxes.** gVisor egress recorded as a documented limitation. **Deferred:** multi-runner scheduling, SDK/MCP, Ansible deployment, bwrap seccomp, disk quota, image signing |
| v1.0 | Firecracker microVM tier, snapshots/forks |

## License

MIT — see [LICENSE](LICENSE).
