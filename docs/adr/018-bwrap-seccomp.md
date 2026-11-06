# ADR 018 — Bubblewrap seccomp hardening

- Status: accepted (v0.6)
- Date: 2026-09-21

## Context

Bubblewrap sandboxes ran with namespaces and cgroup limits but no syscall
filter. The README listed "no syscall filtering for bwrap" as a known gap and
called bwrap's `--seccomp` the planned step. gVisor already provides its own
syscall surface, so this is a Bubblewrap-only concern.

## Decision

**1. A deny-list filter, not an allow-list.**
Syscalls are allowed by default, and a fixed set that a sandboxed workload has
no legitimate need for returns `EPERM`. An allow-list has to enumerate every
syscall every image's runtime uses, which breaks workloads in ways that are
hard to diagnose (and the base/node/python images differ). The deny-list
removes the obvious kernel attack surface: `ptrace` and the process-vm/kcmp
family, the mount API (`mount`, `umount2`, `pivot_root`, `open_tree`,
`move_mount`, `fsopen`, `fsconfig`, `fsmount`, `mount_setattr`, `setns`,
`unshare`), module loading and `kexec`, `reboot`/`swapon`/`swapoff`,
hostname/domain/time changes, `bpf` and `perf_event_open`, the keyring calls,
`iopl`/`ioperm`, and the handle and quota calls.

**2. Installed by the in-sandbox init, before it serves.**
`vora-init` (PID 1) installs the filter with `prctl(PR_SET_NO_NEW_PRIVS)` then
`seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC)`, so it applies to
every thread and to every process the agent later starts.

The issue suggested bwrap's `--seccomp`, which takes a file descriptor. That
works only for sandboxes the runner spawns directly. The `NetworkNone` path
launches bwrap through `systemd-run` for its cgroup, and a file descriptor
cannot be passed across that boundary. Installing in the init works identically
for both launch paths and covers the whole sandbox, so it is the choice here.

**3. Fail loud.**
If the filter cannot be installed (an unsupported architecture, or a kernel
without seccomp), `vora-init` logs the error and exits non-zero, so the sandbox
never starts unfiltered. A regression test installs the filter in a child
process and asserts a denied syscall returns `EPERM` while an allowed one
succeeds, so a silent fallback to an unfiltered sandbox fails the test.

## Consequences

- The deny-list is a hardening layer, not a containment boundary: the real
  isolation is namespaces and cgroup limits (and gVisor for stronger needs). It
  narrows the kernel surface reachable from a bwrap sandbox.
- The architecture check kills a process whose `seccomp_data.arch` differs from
  the build architecture, because the syscall numbers would then be
  meaningless.
- Adding a syscall to the list is cheap and testable. Removing one needs a
  reason. A workload that legitimately needs a denied syscall will see `EPERM`
  and can be granted an exception explicitly.
- gVisor is unaffected (its own syscall surface). `vora-init pause` under
  gVisor does not install the filter.

## Refs

- Issue vorash/vora#23; master plan's bwrap-hardening note.
- `internal/seccomp`, `cmd/vora-init`.