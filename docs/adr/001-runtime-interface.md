# ADR 001 — Runtime interface

- Status: accepted
- Date: 2026-09-16

## Context

Vora must support several isolation technologies over time: Bubblewrap for
local development, gVisor for hosted untrusted workloads, and eventually
Firecracker for VM-level isolation. If the control plane embedded any
backend-specific detail, each new backend would leak into the runner, API and
CLI.

## Decision

Define a single `runtime.Backend` interface that all isolation technologies
implement. The control plane depends only on this interface and on the
`SandboxSpec`, `Stats`, and `ExecRequest` types.

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

Backends report `Capabilities` so the runner can adapt without special-casing:
`NativeExec` (gVisor true, bwrap false), `OwnsCgroups` (containerd-backed true),
and the supported `NetworkModes`. Backends that do not own cgroups receive a
`cgroup.Manager` and use it to launch their workload.

`SandboxSpec` is the policy input and carries tenant and agent IDs from day one,
so identity can be added later without a breaking change.

## Consequences

- Adding a backend is additive: implement the interface and register it.
- Capability negotiation is explicit rather than inferred from backend names.
- The interface is deliberately small. Features that only one backend can serve
  (snapshots, fork) are deferred until a second implementation justifies them.
- `runtime` stays in `internal/` for now. It can be promoted to `pkg/` if an
  external consumer appears.
