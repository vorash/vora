# ADR 006 — Resource limits via transient systemd services

- Status: accepted
- Date: 2026-09-16

## Context

The plan assumed `systemd-run --scope`. In practice a scope cannot be launched
detached: `systemd-run --scope` blocks until the sandbox exits, and
`--no-block` is rejected (`--no-block is not supported in --scope mode`). The
runner must start a sandbox and return without holding the sandbox's lifetime
in its own process tree.

A second problem: systemd removes a failed unit's cgroup as soon as it exits,
so `memory.events`/`pids.events` are gone by the time the runner looks for a
termination reason.

## Decision

- Launch each sandbox as a **transient systemd service**:
  `systemd-run --user --unit=vora-<id> -p MemoryMax=… -p MemorySwapMax=0
  -p TasksMax=… -p CPUQuota=… -p KillMode=control-group -- bwrap …`.
  The runner returns immediately; the process tree lives under its own cgroup.
- Do **not** use `--collect`: failed units stay loaded so their `Result`
  property (`oom-kill`, `timeout`, `resources`, …) is available after death.
  The runner calls `systemctl reset-failed` when destroying.
- Derive semantic termination reasons from a combination of `systemctl show
  -p Result` and (while running) cgroup `memory.events`/`pids.events`.
- `--user` in local development (no root), `--system` on hosted runners.

## Consequences

- The runner never blocks on a sandbox and does not need to be the sandbox's
  parent; `--die-with-parent` is replaced by the agent watchdog.
- `Result` gives reliable OOM/timeout detection without racing cgroup teardown.
- Failed units accumulate until reset; the reconciler and destroy path clear
  them, and `make test-security` asserts no `vora-*` units remain afterwards.
- One `systemd-run` fork per sandbox is a scale ceiling (~500–1000). v0.2
  replaces it with a `Delegate=yes` runner service managing a delegated cgroup
  subtree directly.
