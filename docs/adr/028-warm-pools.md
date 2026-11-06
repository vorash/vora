# ADR 028 — Warm pools with a proven reset between runs

- Status: accepted
- Date: 2026-11-06 (`vorash/vora#153`)

## Context

A tenant that runs thousands of short programs pays for a sandbox start on each
one. On vora-mini on 2026-11-05 a cold bwrap create took about 185 ms, and a
gVisor start took 70 ms with 58 ms more to prepare it for reuse. Reusing a
sandbox removes that cost, and adds a risk: whatever one run leaves behind can
change what the next run does. A `usercustomize.py` in the home directory, for
example, runs inside every later Python process.

So reuse is only worth having if a run can trust that the run before it left
nothing it can act on, and if "can't prove it" costs a sandbox rather than a
leak. This ADR records what Vora does between two runs, what it closes, and
what it leaves open on purpose.

## Decision

**1. A pool lives in the runner, and a run is one streaming call.** `PUT
/v1/pools/{name}` declares it, and `POST /v1/pools/{name}/run` borrows a member,
runs one command and streams `run`, output, `exit`, then `run_done`. A client
never holds a member, so a client that crashes can't strand one. Members are
ordinary sandbox records with a `pool_id`. Which ones are idle is kept in memory
only, because a runner restart destroys them anyway.

**2. A reset trusts nothing inside the sandbox until the host has cleared it.**
Three steps, in order:

1. The host kills every process but init's, and proves that only those remain.
   bwrap freezes the cgroup, kills through pidfds, thaws and compares
   `cgroup.procs` with a baseline. gVisor uses `runsc ps` and `runsc kill`.
2. A helper restores every writable mount to the listing captured at `Prepare`,
   and verifies it. bwrap's helper is the agent. gVisor's is `runsc exec` of
   `/vora/vora-init reset`, from a read-only bind.
3. The host proves the process set again.

Any step that fails, is refused, can't be parsed or times out returns
`runtime.ErrUnproven`, and the pool destroys the member. A member becomes idle
only from a reset that returned nil. `Prepare` runs one full reset on the fresh
sandbox, so a backend that can't reset fails at warm-up and not at first reuse.

**3. Every exec on a member is finished, or has its cancel requested, before its
reset, and the runner starts none until the reset returns.** An exec issued with a live context during a
reset can leak past the proof: 198 of 300 tries returned clean with a run's
process and file present, and cancelling first gave 300 of 300 clean (bwrap,
backend level, step 10 of the build, 2026-11-05; the log names no host for this
one). `RunInPool` runs its exec on the client's context, and `rejectPooled` refuses
every plain RPC that could change a member.

The order this relies on is that `runExec` closes the exec's stream before `Pool.Run`
checks the member in, and check-in is what starts the reset. A closed stream means the
cancel was requested, not that it arrived, so the request may still be on its way to
the agent or to `runsc`. What covers that gap differs by backend:

- On bwrap the agent holds `execMu` while an exec runs, its restore waits for `execMu`,
  and an exec whose cancel has arrived by the time it gets `execMu` doesn't start. The
  host proves the process set after the restore as well as before it.
- On gVisor a reset waits for each `runsc exec` client to answer, because a killed client's
  request can still reach the Sentry later.

**4. What a reset closes, and how:**

| A run leaves | Closed by | Test row |
| ------------ | --------- | -------- |
| Files in `/tmp`, `/dev`, `/dev/shm`, `/workspace`, `/root`, including `usercustomize.py`, `.pth` files and shell profiles | restore to the listing; a listed entry that changed can't be repaired and costs the member | L1, L2 |
| Files in `/vora`, or a replaced `vora-init` | restore; a read-only bind over the binary (bwrap), all of `/vora` read-only (gVisor) | L3 |
| Environment | each exec starts from init's unchanged environment | L4 |
| Background processes, in any session | the host kill and both proofs | L5 |
| A process holding the exec's stdout | the run's hard deadline, then the host kill | L6 |
| SysV IPC and POSIX queues | denied by seccomp on every member | `TestPoolMembersDenyIPC` |
| A changed nice, affinity, rlimit or sysctl on a long-lived process | detected at reset, and costs the member | `reset_kernel_test.go` |

`tests/integration/leak_test.go` holds the rows.
`tests/integration/pool_leak_test.go` runs them through a real API and runner. It has
rows for both backends, and CI can only run the bwrap ones, as `Consequences` says.

**5. A pool is single-tenant by construction, and these channels stay open.** A
pool belongs to one tenant, its routes are tenant-scoped, and a member never
moves. Mixed-tenant reuse is out of scope and must stay so, because each of
these lets a later run learn something about an earlier one:

- **The PID counter.** PIDs keep counting up across runs.
  `TestPoolPIDCounterIsAnAcceptedResidual` holds this in place.
- **A file's atime.** Reading a listed file moves it, and the reset doesn't
  compare it. A directory's atime is written back.
- **Inode flags on a directory.** A few bits, not compared.
- **Network namespace state.** TIME_WAIT sockets, the neighbour cache and port
  usage survive, so a later run can see an earlier run's peer addresses in
  `/proc/net/tcp` under `NETWORK_FULL` and `NETWORK_PRIVATE`. It doesn't apply to
  a pool today: the pool spec refuses every mode but `NETWORK_NONE`, and this
  needs revisiting before that changes.

A run can also tell it isn't the first through uptime and `sandbox_run`. Neither
is a secret between runs of one tenant.

**6. Skipping the reset needs three separate things.**
`POOL_REUSE_UNSAFE_NO_RESET` exists as an experiment's baseline. The API refuses
it without an admin key (403), the runner refuses it unless started with
`VORA_POOL_UNSAFE_REUSE=allow`, and every run on such a pool reports it in its
`run` frame. A runner that loses the grant refuses to load the pool, rather than
running it with a reset. The pool stays single-tenant. What it gives up is
cleanliness between runs of that tenant.

**7. A member's `pids` floor depends on the backend, because the limit counts
different things.** Under bwrap it counts Vora's tasks and the workload's. An
idle sandbox holds 10 or 11, so 12 is the first limit that always leaves the
workload one, and an exec loop passed 150 of 150 there (bwrap, vora-mini,
2026-11-05). A workload that leaves a process behind needs 13.

Under gVisor it counts the Sentry's host threads, not guest tasks: 800 guest
processes held `pids.current` at 38. So it doesn't bound guest tasks, which leaves
`memory.max` to stop a fork bomb. Through a pool with a 256 MiB limit, a fork bomb ended `OOM_KILLED` at a 256 MiB peak and cost the member, in each of 6 logged runs (gVisor, vora-mini, 2026-11-06). `TestPoolWorkloadThatEndsItsMember` asserts it, and CI can't run that row.
Reaching `pids` ends the whole sandbox. Limits from 12 to 26
can't start, 28 to 36 died under load, and 48, 64 and 128 each passed 60 cycles
(gVisor, vora-mini, 2026-11-05). Thread growth has no measured plateau, so the
floor is 64, with a margin above the lowest that passed.

**8. An exec that didn't start costs the member, not the client.** `Exec` fails
when nothing ran: a dead agent on bwrap, a stopped sandbox on gVisor. The pool
retires that member and retries the run once on another, before the client sees
a frame. A sandbox that dies under a running command is different, because the program
may have run. The client sees a `RUNTIME_FAILURE` exit or an error line, and the
member's reset fails.

A command that doesn't exist is neither of these, and costs nothing but its own run.
The agent or `runsc` answers, so the client sees an ordinary `EXITED` exit, with code
127 on bwrap and 128 on gVisor, and the member goes through its reset and stays.
`vora_exec_failed_total` doesn't count it.

## Alternatives

- **A lease API (acquire, exec, release).** It costs three calls per run, shows
  member identity to every caller, and a crashed client strands its lease until
  a second TTL system expires it.
- **Fork from a snapshot per run.** A fork is a full start plus a tar extract,
  and restoring one doesn't delete files or clear processes.
- **A public reset RPC, with the pool in each SDK.** The pool logic would be
  written once per SDK, and its metrics would leave Vora.
- **The pool in `vora-api`.** The API has no timers, handles or backend access,
  and a reset needs all three.

## Consequences

- Reset cost, on vora-mini on 2026-11-05: bwrap p50 2.1 ms, p99 7.6 ms; gVisor
  p50 29 ms, p99 about 32 ms, limited by starting `runsc` three times. A template
  of 5133 entries takes gVisor's reset to 1.38 s. These are backend-level
  numbers, not a pool's throughput, which no logged run has measured.
- Through a runner, `pool_leak_test.go` passed on both backends on vora-mini on
  2026-11-06. Its 300-round disconnect test kept the member in every round, over 4
  runs on gVisor and 11 on bwrap. CI runs only the bwrap rows, on 2 vCPUs, so a
  gVisor regression needs a host to show. This tests safety, not load: nothing here
  says how a pool behaves under real load.
- On gVisor, guest code runs between kill passes, so forking chains can outlast
  them and cost the member. That fails closed. In `TestGvisorDeadSandbox` an OOM
  also takes the sandbox down, which costs the member.
- `max_size` is reserved against the runner's capacity whether members are busy
  or not.
- Every `Exec` on gVisor now waits up to 2 s for `runsc exec` to answer before it
  returns. A client that exits without having started the command also costs a
  `runsc state`, bounded at 5 s, so by those bounds the worst case is about 7 s. An
  exec into a stopped sandbox is an error from `Exec` rather than an exit chunk.

## Refs

- ADR 001 (runtime interface), ADR 008 (gVisor), ADR 018 (bwrap seccomp), ADR 021
  (capacity placement), ADR 027 (network allocation).
- `internal/pool`, `internal/runner/pools.go`, `internal/sandboxreset`,
  `internal/runtime/bwrap/reuse.go`, `internal/runtime/gvisor/reuse.go`.
