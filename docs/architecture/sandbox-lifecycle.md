# Sandbox Lifecycle

Status: draft · Applies to: v0.2

## States

```text
REQUESTED → SCHEDULING → CREATING → STARTING → RUNNING
   RUNNING → STOPPING → STOPPED → DESTROYED
   RUNNING → FAILED → DESTROYED
   RUNNING → EXPIRED → DESTROYED
```

`SCHEDULING` is a no-op in v0.1 (single runner) but exists so multi-runner
scheduling does not change the state machine. `FAILED`, `EXPIRED` and
`DESTROYED` are terminal; `DESTROYED` is retained as a tombstone record.

## Transitions

| From | To | Trigger | Side effects |
| ---- | -- | ------- | ------------ |
| — | REQUESTED | `CreateSandbox` | allocate ID, persist config, create dirs |
| REQUESTED | SCHEDULING | runner accepts | select backend (single runner: itself) |
| SCHEDULING | CREATING | backend selected | materialise mounts, reserve unit |
| CREATING | STARTING | resources ready | start `vora-init` under a cgroup service |
| STARTING | RUNNING | agent ready (`Ping` ok) | record runner_id, start TTL timer |
| RUNNING | STOPPING | `DestroySandbox` / TTL | signal agent, stop unit |
| STOPPING | STOPPED | process tree exits | collect final stats |
| STOPPED | DESTROYED | cleanup | remove mounts/dirs, record tombstone |
| RUNNING | FAILED | runtime error / unexpected exit | capture reason, surface to caller |
| RUNNING | EXPIRED | TTL elapsed | stop then destroy |
| any | DESTROYED | reconciler on restart | repair drift |

## Events

Every transition appends a record to `logs/events.jsonl`:

```json
{
  "sandbox_id": "sbx_01J...",
  "from": "STARTING",
  "to": "RUNNING",
  "timestamp": "2026-09-16T18:00:00Z",
  "runner_id": "run_01J...",
  "reason": ""
}
```

## Persistence

```text
/var/lib/vora/sandboxes/<sbx_id>/
├── config.json          # spec, state, timestamps, limits, runner_id
├── workspace/           # rw, bind-mounted /workspace
├── home/                # rw, bind-mounted /root
├── ipc/                 # bind-mounted /run/vora (control.sock)
└── logs/
    ├── events.jsonl
    └── exec/<exe_id>.json
```

`config.json` is written atomically (write temp file, `rename`) before a state
change is observable. The store is the source of truth across runner restarts.

## Exec model

A sandbox is long-lived and holds a `vora-init` process as PID 1. Exec is
mediated by an in-sandbox agent rather than re-launching Bubblewrap:

1. Runner binds `<sandbox>/ipc` to `/run/vora` inside the sandbox.
2. `vora-init` creates `control.sock` and serves `AgentService`
   (`Ping`, `Exec`, `Signal`, `Shutdown`).
3. Runner dials the socket (AF_UNIX filesystem sockets cross network
   namespaces, so this works in `NetworkNone`).
4. `Exec` streams stdout/stderr and returns the exit code and reason.

This is required because Bubblewrap cannot join an existing sandbox. gVisor
(v0.2) exposes native exec, so the agent is bwrap-only.

### Die-with-runner

The runner pings each ready agent every 5 s, from the moment it answers until
the sandbox is stopped or the runner shuts down. `vora-init` exits when no RPC
or open stream has reached it for its grace (20 s), which tears down the
sandbox. An idle sandbox therefore stays up while its runner lives. This
preserves `--die-with-parent` semantics even though, under systemd services, the
sandbox's parent is systemd rather than the runner.

## Resource limits and termination reasons

Limits are applied by `cgroup.Manager` on a transient systemd service. Stats and
termination reasons are read from the sandbox cgroup:

| Reason | Source |
| ------ | ------ |
| `Exited` | normal process exit |
| `OOMKilled` | `memory.events` `oom_kill > 0` |
| `PidsLimitExceeded` | `pids.events` `max > 0`. On gVisor this counts the Sentry's host threads, and reaching the limit ends the sandbox |
| `TimeoutExceeded` | runner deadline / TTL |
| `PolicyViolation` | reserved for v0.3 |
| `RuntimeFailure` | backend/agent error |

An OOM kill ends the exec, not the sandbox. The unit runs with
`OOMPolicy=continue`, so systemd does not stop it when the kernel kills a
process in it, and vora-init starts every exec with `oom_score_adj` 1000, so
the kernel's single victim is a workload process, never the agent. The exec
reports `OOMKilled` with its usage on its exit chunk, the cgroup keeps its
counters for `stats`, and the sandbox accepts the next exec.

## TTL and reconciliation

- A reaper goroutine expires RUNNING sandboxes whose TTL elapsed → `EXPIRED`,
  then destroys them.
- On runner start the reconciler **fail-closes**. It does not reattach live
  sandboxes:
  1. every store record in `CREATING`/`STARTING`/`RUNNING`/`STOPPING` is
     reaped (`Sweeper.Reap`) and marked `FAILED`, workspace cleaned;
  2. leftover backend containers (e.g. `runsc list`) not in the store are reaped;
  3. leftover `vora-sbx_*` systemd units are stopped and reset.
- Handles are in-memory; exec after a restart requires a new sandbox.

## Snapshots and forks

A snapshot captures a RUNNING or stopped sandbox's writable state
(crash-consistent; ADR 023) and a fork is an ordinary create whose workspace
is restored from that copy instead of starting empty. The fork goes through
the same `REQUESTED → … → RUNNING` path and gets a fresh TTL; the snapshot
record outlives the source sandbox, so forking after the source is gone is
valid.

## Cleanup guarantees

Destroying a sandbox must: stop the unit, kill the process tree, remove the
cgroup, and delete the sandbox directory. `make test-security` asserts no
`vora-sbx_*.service` units and no `vora-*` processes remain after the suite runs.
