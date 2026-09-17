# Vora

**Secure execution infrastructure for autonomous agents.**

Vora runs untrusted AI agents and generated code inside isolated Linux
environments with resource controls, default-deny networking, and a complete
execution audit trail.

Built with Go, Bubblewrap, Linux namespaces, cgroups v2 (via systemd), gRPC,
and Prometheus.

> **Status: v0.2 proof of concept.** Two isolation backends (Bubblewrap and
> gVisor) are functional and covered by a shared isolation test suite. It is
> **not** production-ready and has not been independently audited. See
> [Roadmap](#roadmap) and [Limitations](#limitations-honest).

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
- **Default-deny networking**: sandboxes have no interfaces or routes.
- State persistence, a **TTL reaper**, and startup **reconciliation** (incl.
  reaping orphaned `runsc` containers and systemd units).
- **Audit trail** behind a `Store` interface: local JSONL, or **PostgreSQL /
  Supabase** (`VORA_AUDIT_DSN`), with lifecycle events and per-execution records.
- **Prometheus metrics** at `127.0.0.1:9101/metrics`.
- A dual-backend **isolation security suite** and a **benchmark harness**.

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

## Repository layout

```text
api/proto/v1/   protobuf contracts (runner + in-sandbox agent)
cmd/            vora (CLI), vora-runner (daemon), vora-init (sandbox agent)
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
make bench-report      # benchmarks + report in docs/benchmarks/
make help              # all targets
```

Install and start the rootlesskit-wrapped runner (required for gVisor):

```bash
make install-rootless   # user systemd service: vora-runner.service
vora sandbox create --image vora/base --backend gvisor
```

The runner exposes Prometheus metrics at `127.0.0.1:9101/metrics` and a health
check at `/healthz` (override with `VORA_METRICS_ADDR`).

Configuration is via environment variables: `VORA_STATE_DIR`, `VORA_SOCKET`,
`VORA_SYSTEMD_MODE` (`user`/`system`), `VORA_METRICS_ADDR`, `VORA_INIT_BINARY`,
`VORA_GVISOR_PLATFORM` (`kvm`/`systrap`), `VORA_GVISOR_ROOT`, `VORA_RUNSC`, and
`VORA_AUDIT_DSN` (PostgreSQL/Supabase; omitted → local JSONL).

## Limitations (honest)

- **Images still come from the Docker-export cache** as `<name>/<tag>/rootfs`
  (implicit tag `latest`). containerd is used as the OCI store for container
  data, but the rootfs cache is not yet materialised from containerd snapshots
  (ADR 009) — that migration is pending.
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
- **Not production-ready.** No auth on the runner socket, single runner, no
  multi-tenant scheduling.

## Roadmap

| Version | Scope |
| ------- | ----- |
| v0.1 | Go runner, Bubblewrap backend, cgroups v2, in-sandbox exec agent, CLI, security suite, benchmarks |
| **v0.2 (this)** | **gVisor backend (direct `runsc`, Method 2), backend selection, rootlesskit runner service, PostgreSQL/Supabase audit, dual-backend isolation matrix** |
| v0.3 | HTTP control plane + remote runner, network policy + egress gateway (SNI allowlist), credential brokering, TS/Python SDK, MCP |
| v1.0 | Firecracker microVM tier, snapshots/forks |
| pending | containerd-snapshot rootfs (ADR 009) |

## License

MIT — see [LICENSE](LICENSE).
