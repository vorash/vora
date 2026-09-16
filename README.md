# Vora

**Secure execution infrastructure for autonomous agents.**

Vora runs untrusted AI agents and generated code inside isolated Linux
environments with resource controls, default-deny networking, and a complete
execution audit trail.

Built with Go, Bubblewrap, Linux namespaces, cgroups v2 (via systemd), gRPC,
and Prometheus.

> **Status: working v0.1 proof of concept.** The local Bubblewrap runtime is
> functional and covered by isolation tests. It is **not** production-ready and
> has not been independently audited. Hosted gVisor, network policy, and
> credential brokering are v0.2/v0.3 (see [Roadmap](#roadmap)).

## What works today

- A Go runner daemon (`vora-runner`) that manages the full sandbox lifecycle
  over a gRPC Unix socket.
- A **Bubblewrap backend**: every sandbox runs in its own user, PID, network,
  IPC, UTS and mount namespaces, with a read-only shared rootfs.
- A long-lived **in-sandbox agent** (`vora-init`, PID 1) that makes repeated
  `exec` into a running sandbox possible.
- **cgroup v2 limits** (`memory.max`, `pids.max`, `cpu.max`) enforced through
  transient systemd units, with semantic termination reasons (`OOMKilled`,
  `PidsLimitExceeded`, …).
- **Default-deny networking**: sandboxes have no interfaces or routes.
- **State persistence**, a **TTL reaper**, and startup **reconciliation** of
  orphaned sandboxes.
- An execution **audit trail** (lifecycle events + per-execution records) and
  **Prometheus metrics**.
- An isolation **security suite** and a **benchmark harness**.

## Architecture

```text
                         vora (CLI)
                             │  gRPC over unix socket
                             ▼
                    ┌─────────────────────┐
                    │    vora-runner      │  runtime/vora/runner.sock
                    │                     │
                    │ sandbox state mch.  │  state/images, state/sandboxes
                    │ TTL reaper          │
                    │ reconciler          │
                    │ runtime.Registry    │
                    └─────────┬───────────┘
                              │ runtime.Backend
                     ┌────────┴─────────┐
                     ▼                  ▼
              bwrap backend      cgroup.Manager (systemd service)
                     │                  │
                     │  systemd-run --user --unit=vora-<id> -p MemoryMax… bwrap …
                     ▼
              ┌──────────────────────────────────────────────┐
              │ bubblewrap sandbox                            │
              │  --unshare-all (user,pid,net,ipc,uts,cgroup)  │
              │  ro rootfs (shared image cache)               │
              │  rw /workspace, /root; tmpfs /tmp             │
              │  vora-init (PID 1, gRPC agent)                │
              └──────────────────────────────────────────────┘
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
| Processes / signals | PID namespace | `TestPIDNamespaceIsolation` |
| IPC | IPC namespace | `--unshare-all` |
| Privilege | unprivileged user namespace | `--unshare-all` |
| Memory | `memory.max` (+ `MemorySwapMax=0`) | `TestMemoryBombKilled` |
| Processes | `pids.max` | `TestForkBombContained` |
| CPU | `cpu.max` | `TestSystemdUnitLimitsAndTeardown` |
| Network | `--unshare-net` (no interfaces/routes) | `TestNetworkNone` |
| Secrets | runner credentials never injected into the sandbox | `TestNoSecretsLeak` |

`tests/security/` is a deliverable, not an afterthought: each test encodes *why*
the boundary matters and was verified to fail when the corresponding flag is
removed.

## Demo

```console
$ vora sandbox create --image python:3.13 --memory 512m --cpu 1 --pids 128 --network none
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
Docker (to build images), Bubblewrap, and a systemd user session.

```bash
make images      # build the read-only rootfs cache (~/.local/state/vora/images)
make binaries    # build bin/{vora,vora-runner,vora-init} (static)
make test        # unit tests
make test-integration  # runner + sandbox lifecycle over a real socket
make test-security     # isolation suite
make bench-report      # benchmarks + report in docs/benchmarks/
make help        # all targets
```

Run a local runner (rootless, user systemd) and use the CLI:

```bash
./bin/vora-runner &                     # serves ~/.local/state/.../runner.sock
vora sandbox create --image python:3.13
```

The runner exposes Prometheus metrics at `127.0.0.1:9101/metrics` and a health
check at `/healthz` (override with `VORA_METRICS_ADDR`).

Configuration is via environment variables: `VORA_STATE_DIR`, `VORA_SOCKET`,
`VORA_SYSTEMD_MODE` (`user`/`system`), `VORA_METRICS_ADDR`, `VORA_INIT_BINARY`.

## Limitations (honest)

- **Shared kernel.** Bubblewrap isolates namespaces but shares the Linux kernel;
  a kernel vulnerability could escape. gVisor (v0.2) reduces this surface.
- **No syscall filtering yet.** seccomp is not applied; Bubblewrap `--seccomp`
  is a planned hardening step.
- **Disk is not quota-enforced.** A sandbox can fill its workspace up to host
  free space.
- **Images are trusted.** No signature verification yet.
- **The image must provide mount points.** Rootfs images created by
  `make images` include `/workspace` and `/vora`; custom images must create them.
- **Sandboxes survive runner death briefly**, then exit via the agent's
  watchdog; orphaned units are cleaned on the next runner start.
- **Not production-ready.** No auth on the runner socket, single runner, no
  multi-tenant scheduling.

## Roadmap

| Version | Scope |
| ------- | ----- |
| **v0.1 (this)** | Go runner, Bubblewrap backend, cgroups v2, in-sandbox exec agent, CLI, security suite, benchmarks |
| v0.2 | gVisor/containerd backend, HTTP control plane, remote runner, systemd-delegated cgroups |
| v0.3 | Network policy, egress gateway (SNI allowlist), credential brokering, OTel, TS SDK, MCP |

## License

MIT — see [LICENSE](LICENSE).
