# Sandbox Lifecycle

Status: draft · Applies to: v0.1

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

The runner holds an attach stream to each agent. If it drops, `vora-init` starts
a grace timer and then exits, which tears down the sandbox. This preserves
`--die-with-parent` semantics even though, under systemd services, the sandbox's
parent is systemd rather than the runner.

## Resource limits and termination reasons

Limits are applied by `cgroup.Manager` on a transient systemd service. Stats and
termination reasons are read from the sandbox cgroup:

| Reason | Source |
| ------ | ------ |
| `Exited` | normal process exit |
| `OOMKilled` | `memory.events` `oom_kill > 0` |
| `PidsLimitExceeded` | `pids.events` `max > 0` |
| `TimeoutExceeded` | runner deadline / TTL |
| `PolicyViolation` | reserved for v0.3 |
| `RuntimeFailure` | backend/agent error |

## TTL and reconciliation

- A reaper goroutine expires RUNNING sandboxes whose TTL elapsed → `EXPIRED`,
  then destroys them.
- On runner start the reconciler compares persisted state with reality:
  - persisted RUNNING but unit gone → mark `FAILED`;
  - unit present but no config → stop the orphan unit;
  - state `CREATING`/`STARTING` → clean up partial resources.
- Orphan units are found via
  `systemctl --user list-units 'vora-sbx_*.service'`.

## Cleanup guarantees

Destroying a sandbox must: stop the unit, kill the process tree, remove the
cgroup, and delete the sandbox directory. `make test-security` asserts no
`vora-sbx_*.service` units and no `vora-*` processes remain after the suite runs.
