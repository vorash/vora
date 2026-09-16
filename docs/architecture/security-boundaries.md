# Security Boundaries

Status: draft · Applies to: v0.1

This document answers a single question for every resource type:

> What exactly prevents Sandbox A from affecting Sandbox B (or the host)?

Each boundary lists the mechanism, how it is enforced, and the test that proves
it. If the mechanism is removed, the test must fail.

## Sandbox A vs Sandbox B

| Resource | A cannot reach B because… | Mechanism | Test |
| -------- | ------------------------- | --------- | ---- |
| Files | A's mount namespace has no path to B's workspace or rootfs; root rootfs is shared read-only and holds no tenant data | mount namespace, `--ro-bind` rootfs, per-sandbox `--bind` workspace | `security/fs_*` |
| Processes | A's PID namespace contains only its own processes; B's PIDs are not namespaced into A | `--unshare-pid` | `security/pid_*` |
| Signals | A cannot `kill` a PID it cannot see | PID namespace | `security/pid_kill_*` |
| IPC | A has a private SysV/POSIX IPC namespace; B's shared memory and semaphores are invisible | `--unshare-ipc` | `security/ipc_*` |
| Hostname / domain | A's UTS namespace is private | `--unshare-uts` | `security/uts_*` |
| Users / capabilities | A runs in an unprivileged user namespace with no host capabilities | `--unshare-user` | `security/userns_*` |
| Network | A's network namespace has no interfaces and no routes; B is unreachable | `--unshare-net` | `security/net_*` |
| CPU | A is throttled by its own cgroup quota; B is unaffected by A's load | `cpu.max` | `security/cpu_*` |
| Memory | A's allocations are capped; exceeding the cap kills A, not B | `memory.max`, `memory.swap.max=0` | `security/memory_bomb` |
| PIDs | A's fork attempts hit its own `pids.max`; B's process budget is untouched | `pids.max` | `security/fork_bomb` |
| Disk | A writes only into its own workspace; the host filesystem is read-only to A | bind mounts, read-only rootfs | `security/disk_*` |
| Control socket | A's control socket lives in its own `ipc/` directory bound only into A; B cannot see or connect to it | per-sandbox bind mount + filesystem permissions | `security/control_socket_*` |
| Metadata / secrets | A has no network and no runner credentials in env, files or `/proc/self/environ` | no network; secrets stay outside sandbox | `security/secrets_*` |

## Host protection

| Host resource | Protection |
| ------------- | ---------- |
| Filesystem | rootfs mounted read-only; host paths never bind-mounted; no host `/` exposure |
| Kernel | unprivileged user namespace; no `CAP_SYS_ADMIN` from the host; seccomp hardening is a follow-up |
| PID 1 / init | sandbox has its own PID namespace and its own PID 1 (`vora-init`) |
| Devices | minimal `/dev` provided by Bubblewrap |
| cgroup hierarchy | sandbox services are transient systemd units; removing the unit removes the cgroup |
| Availability | memory/PID caps prevent host exhaustion; CPU quota bounds utilisation |

## Residual risks (accepted in v0.1)

- **Shared kernel.** Bubblewrap isolates namespaces but shares the kernel; a
  kernel vulnerability could escape. gVisor (v0.2) reduces this surface.
- **No syscall filtering.** seccomp is not yet applied; Bubblewrap `--seccomp`
  is a planned hardening step.
- **Read-only rootfs assumptions.** Some toolchains expect writable paths
  outside `/workspace`; those calls fail closed rather than being permitted.
- **Disk quota is directory-bounded, not enforced by a filesystem quota** in
  v0.1; the sandbox can fill its workspace up to host free space. A project
  quota or `io.max` is planned.
- **Images are trusted.** No signature verification in v0.1.

## Verification policy

`tests/security/` is a deliverable, not an afterthought. Every test:

1. creates a real sandbox through the normal runner path;
2. attempts the attack described above;
3. asserts the boundary holds;
4. documents *why* the boundary matters so that weakening it breaks the test.
