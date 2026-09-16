# ADR 002 — Bubblewrap first, gVisor second

- Status: accepted
- Date: 2026-09-16

## Context

Bubblewrap and gVisor offer different isolation and operational properties.
Bubblewrap is a thin wrapper over Linux namespaces; gVisor interposes a
user-space kernel between the workload and the host. gVisor needs containerd
and `runsc`, and on Hetzner Cloud it must run on the `systrap` platform because
nested virtualisation is unavailable.

The project needs both a fast local iteration loop and a credible untrusted
security boundary.

## Decision

Implement **Bubblewrap first** (v0.1) and **gVisor second** (v0.2), behind the
same `runtime.Backend` interface (ADR 001).

- Bubblewrap is used for local development, understanding the namespace
  primitives, and benchmarking.
- gVisor is used for the hosted runner and untrusted workloads, via containerd
  and `runsc` on the `systrap` platform.
- Bubblewrap-inside-gVisor is explicitly **not** a dependency; the two are
  independent backends.

## Consequences

- v0.1 can start immediately: Bubblewrap is already installed and user
  namespaces are enabled on the development host.
- The security test suite is written against the interface and is expected to
  pass against both backends, giving a meaningful basis for comparison.
- gVisor increases syscall overhead; the benchmark suite quantifies the
  difference rather than assuming it is acceptable.
- Two backends must expose the same semantic termination reasons
  (`OOMKilled`, `PidsLimitExceeded`, …), which keeps the abstraction honest.
- Firecracker remains a hypothetical third backend and is not implemented.
