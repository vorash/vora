# Architecture Overview

Status: draft · Applies to: v0.2

## Purpose

Vora executes untrusted agent-generated code behind a policy-controlled
execution boundary. This document describes the v0.2 component model and the
seams that later versions extend.

## Scope

**v0.2 (this document).** A single Go runner with two isolation backends
(Bubblewrap and gVisor) behind `runtime.Backend`, cgroup v2 limits on both,
repeated exec into a live sandbox, persist-before-observe state, fail-closed
startup reconcile, and a CLI over a gRPC Unix socket.

**Not in v0.2.** HTTP control plane, egress gateway, credential brokering,
snapshots, SDKs, multi-runner scheduling, Kubernetes.

## Components

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
                              │ runtime.Backend
              ┌───────────────┴───────────────┐
              ▼                               ▼
       gVisor (runsc Method 2)         bwrap + systemd-run
       delegated cgroup subtree        -p MemoryMax/TasksMax/CPUQuota
```

| Component | Responsibility |
| --------- | -------------- |
| `vora` | CLI: create/inspect/exec/copy/logs/stats/destroy |
| `vora-runner` | daemon: lifecycle, state, TTL, reconciliation, exposes `RunnerService` |
| `vora-init` | PID 1 inside the sandbox; serves `AgentService` for exec/signal/shutdown |
| `runtime.Backend` | isolation-technology interface (`bwrap`, `gvisor`) |
| `cgroup.Manager` / `cgroup.Subtree` | bwrap: transient systemd units. gVisor: delegated cgroup v2 subtree |
| `images` | read-only rootfs cache shared by all sandboxes |
| `workspace` | per-sandbox mutable directories and mount points |
| `sandbox.Store` | persisted metadata and state machine |
| `snapshot` | captured writable state and forks (ADR 023) |
| `telemetry` | structured logs and Prometheus metrics |

## Design principles

1. **Isolation is behind an interface.** Control-plane code depends only on
   `runtime.Backend`; backend specifics stay in `internal/runtime/<backend>`.
2. **Read-only shared rootfs.** Mutable state lives only in `/workspace`,
   `/root` and `/tmp`, so rootfs can be shared and need not be copied.
3. **Long-lived sandboxes.** A sandbox keeps its process state between execs, so
   the RUNNING lifecycle is meaningful.
4. **Persist before observing.** A state change is written to disk before it is
   reported to a caller.
5. **Fail loud.** Termination reasons are semantic (`OOMKilled`,
   `PidsLimitExceeded`, …) rather than raw exit codes.

## Runtime abstraction

```go
type Backend interface {
    Name() string
    Capabilities() Capabilities
    Create(ctx context.Context, spec SandboxSpec) (Handle, error)
    Start(ctx context.Context, h Handle) error
    Exec(ctx context.Context, h Handle, req ExecRequest) (ExecStream, error)
    Signal(ctx context.Context, h Handle, sig syscall.Signal) error
    Stats(ctx context.Context, h Handle) (*Stats, error)
    Stop(ctx context.Context, h Handle) error
    Destroy(ctx context.Context, h Handle) error
}
```

`Capabilities` declares native exec, cgroup ownership, whether resource limits
actually bite (`EnforcesResourceLimits`), and which network modes it supports.
Auto-select prefers gVisor only when UID is 0 and limits are enforced.

## Deployment modes

| Mode | cgroup target | Root required | Use |
| ---- | ------------- | ------------- | --- |
| local (default) | `systemd-run --user` | no | development, CI, benchmarks |
| system | `systemd-run --system` | yes | hosted runner (Hetzner) |

Both use the same `cgroup.Manager`; the mode is a single config switch.

## Related documents

- [threat-model.md](threat-model.md)
- [security-boundaries.md](security-boundaries.md)
- [sandbox-lifecycle.md](sandbox-lifecycle.md)
- ADRs under `docs/adr/`
