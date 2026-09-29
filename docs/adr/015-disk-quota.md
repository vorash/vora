# ADR 015 — Disk quota

- Status: accepted (v0.5)
- Date: 2026-09-21

## Context

`ResourceLimits.DiskBytes` existed since v0.1 but nothing enforced it: the README
stated plainly that a sandbox could fill its workspace up to host free space.
cgroup v2 has no byte-quota controller, so the enforcement has to come from the
filesystem or from the runner itself.

A sandbox's mutable state is two host directories bound into the sandbox:
`WorkspaceDir` at `/workspace` and `HomeDir` at `/root`. Every backend (bwrap,
gVisor) binds the same pair, so a host-side limit covers both.

## Decision

**1. Prefer an ext4/xfs project quota (hard limit).**
When the sandboxes filesystem is mounted with `prjquota` and the runner holds
`CAP_SYS_ADMIN` (root, or the `vora` user of the system unit, which has it
ambient; gating on uid 0 silently disabled this in production, `#102`) with
`chattr` and `setquota` available, each sandbox's writable directories are
assigned a project ID with a byte limit. Writes past the limit fail with
`EDQUOT` at the kernel boundary, so a sandbox that ignores the error still cannot
overrun the host. The project ID is derived deterministically from the sandbox
id (`fnv32a`, 31 bits), so a runner restart reapplies the same limit without
persisting any mapping. Setting the project id alone is not enough: the
directory must carry the project-hierarchy flag (`chattr +P`) or new files do
not inherit the id and escape the quota (observed on XFS).

**1a. Verify enforcement, don't trust the mount option.**
At startup the runner probes the filesystem: it creates a probe file, assigns a
throwaway project id to the directory, sets a 1 KiB limit, and requires a 64 KiB
write to fail. The file is created **before** the limit is applied, because XFS
charges the inode and directory entry to the project, so a create under a 1 KiB
limit fails with `ENOSPC` for the wrong reason and would look like
non-enforcement. Some kernels mount `prjquota` and account the quota but do not
enforce it (observed on ext4, kernel 7.0.0); a mount option alone cannot tell
the two apart. If the probe does not bite, the runner falls back to accounting
rather than claiming a hard guarantee it does not have. XFS is the verified
hard-limit filesystem.

**2. Fall back to a periodic usage scan (best-effort).**
When project quotas are unavailable — a runner without `CAP_SYS_ADMIN`, a filesystem
without `prjquota`, or a missing tool — the runner's `EnforceDiskQuota` loop
sums the writable bytes and stops a sandbox over its limit. This path runs on
every host and also supplies the semantic reason when hard quotas are active.

**3. Terminate with a semantic reason.**
An over-quota sandbox is moved to `FAILED`/`DESTROYED` with
`ExitReason.DiskQuotaExceeded` (proto `DISK_QUOTA_EXCEEDED`), exposed on the
sandbox record and `vora logs`, so callers never have to interpret a raw
`EDQUOT` or a generic exit code.

**4. Fail loud when a limit cannot be enforced.**
`NewServer` records `vora_disk_quota_enforced` (0/1). A sandbox whose
`DiskBytes` could not be applied increments `vora_disk_quota_unenforced_total`
and logs at error level. Enforcement is never silently skipped.

## Consequences

- The "host unaffected" guarantee holds only in project-quota mode. The soft
  scan bounds the sandbox but not a burst that lands between scans; this is
  documented as a limitation rather than hidden.
- Host convergence must mount the sandboxes filesystem with `prjquota` and
  install `chattr`/`setquota` for the hard path. **XFS** is the verified
  filesystem; ext4 may account without enforcing (see 1a) and will then be
  reported as accounting mode. This is a deployment change owned by the Ansible
  repository; the runner degrades safely without it.
- Project IDs collide with probability ~2⁻³¹ per pair on one filesystem; two
  colliding sandboxes would share a quota. Acceptable for v0.x and noted in the
  package.

## Refs

- Issue vorash/vora#21; master plan §21 ("Disk exhaustion").
- `internal/quota`, `internal/runner` (`applyDiskQuota`, `EnforceDiskQuota`).