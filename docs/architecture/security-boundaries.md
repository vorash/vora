# Security Boundaries

Status: draft · Applies to: v0.2

This document answers a single question for every resource type:

> What exactly prevents Sandbox A from affecting Sandbox B (or the host)?

Each boundary lists the mechanism, how it is enforced, and the Go test that
proves it. If the mechanism is removed, the test must fail.

## Sandbox A vs Sandbox B

| Resource | A cannot reach B because… | Mechanism | Test |
| -------- | ------------------------- | --------- | ---- |
| Files | A's mount namespace has no path to B's workspace or rootfs; root rootfs is shared read-only and holds no tenant data | mount namespace, `--ro-bind` / OCI readonly root, per-sandbox workspace bind | `TestFilesystemIsolation` |
| Processes | A's PID namespace contains only its own processes; B's PIDs are not namespaced into A | PID namespace | `TestPIDNamespaceIsolation` |
| Signals | A cannot `kill` a PID it cannot see | PID namespace | `TestCannotSignalHost` |
| IPC | A has a private SysV/POSIX IPC namespace; B's shared memory is invisible | IPC namespace | `TestIPCNamespaceIsolation` |
| Hostname / domain | A's UTS namespace is private (`hostname=vora`) | UTS namespace | `TestUTSNamespaceIsolation` |
| Users / capabilities | A runs in an unprivileged user namespace with no host capabilities (bwrap) | user namespace | `TestUnprivilegedUserNamespace` |
| Network | A's network namespace has no interfaces and no routes; B is unreachable | net namespace / `runsc --network none` | `TestNetworkNone` |
| Memory | A's allocations are capped; exceeding the cap kills A, not B | `memory.max`, `memory.swap.max=0` | `TestMemoryBombKilled` |
| PIDs | A's fork attempts hit its own `pids.max`; B's process budget is untouched | `pids.max` | `TestForkBombContained` |
| Control socket | A's control socket lives in its own `ipc/` directory bound only into A | per-sandbox bind mount | `TestControlSocketIsolation` |
| Metadata / secrets | A has no network and no runner credentials in env, files or `/proc/self/environ` | no network; secrets stay outside sandbox | `TestNoSecretsLeak` |

CPU quota is set (`cpu.max` / systemd `CPUQuota`) and asserted by
`TestSystemdUnitLimitsAndTeardown` / the gVisor subtree writer. There is no
live-throttle security test (too flaky). Disk is directory-bounded, not
quota-enforced; there is no disk-quota test until a later milestone.

## Host protection

| Host resource | Protection |
| ------------- | ---------- |
| Filesystem | rootfs mounted read-only; host paths never bind-mounted; no host `/` exposure |
| Kernel | bwrap: unprivileged user namespace. gVisor: user-space kernel (KVM). seccomp on bwrap is a follow-up |
| PID 1 / init | sandbox has its own PID namespace |
| Devices | minimal `/dev` from Bubblewrap / gVisor OCI spec |
| cgroup hierarchy | bwrap: transient systemd units. gVisor: delegated subtree under `vora-runner.service` (`Delegate=yes`) |
| Availability | memory/PID caps prevent host exhaustion; CPU quota bounds utilisation |

## Residual risks (accepted in v0.2)

- **Shared kernel on bwrap.** Bubblewrap isolates namespaces but shares the
  kernel; a kernel vulnerability could escape. gVisor reduces this surface.
- **No syscall filtering on bwrap.** seccomp is not yet applied.
- **Read-only rootfs assumptions.** Some toolchains expect writable paths
  outside `/workspace`; those calls fail closed rather than being permitted.
- **Disk quota is directory-bounded, not enforced by a filesystem quota**; the
  sandbox can fill its workspace up to host free space.
- **Images are trusted.** No signature verification.

## Verification policy

`tests/security/` is a deliverable, not an afterthought. Every test:

1. creates a real sandbox through the normal runner path;
2. attempts the attack described above;
3. asserts the boundary holds;
4. documents *why* the boundary matters so that weakening it breaks the test.

## Backends

The matrix runs against **every available backend** as subtests:

| Backend | How it runs | Covered by the matrix |
| ------- | ----------- | --------------------- |
| `bwrap` | unprivileged, cgroups v2 via systemd | full table including fork/memory and user-ns |
| `gVisor` | `runsc` Method 2 inside a `rootlesskit` user namespace; limits via delegated cgroup subtree | full table except `TestUnprivilegedUserNamespace` |

`make test-security` runs the bwrap leg (and skips the gVisor subtests when the
runner is unprivileged). `make test-security-all` additionally runs the gVisor
leg inside a `rootlesskit` userns.
