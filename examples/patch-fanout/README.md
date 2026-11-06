# patch-fanout — evaluate N candidate patches from one repo state, in parallel

**The problem.** An agent proposes five candidate patches for a failing test.
Which one actually fixes it? Checking each candidate means resetting to the
same repo state, applying one patch, and running the suite, either serially or on
five checkouts that drift apart. The patches are also agent-written, so they are
untrusted.

**The move.** Snapshot the prepared repo state **once**, live, while its sandbox
runs. Then fork it once per candidate. Each fork is an independent sandbox that
starts from exactly the same state: a crash-consistent copy of `/workspace` and
`/root`, on the same image ([ADR 023](../docs/adr/023-snapshot-fork.md)).
Apply one patch per fork, run the tests **concurrently**, collect a verdict
table, and tear everything down.

The snapshot and fork feature exists for this pattern: fan out from one state,
evaluate, and throw the copies away.

## Run it

```bash
cd examples && npm install
node patch-fanout/fanout.mjs
```

## What to notice

- `vora.snapshots.create(sandbox.id)` takes the snapshot **while the source
  keeps running**. The copy is live and crash-consistent, with no quiescing step.
- `snapshot.fork()` creates an ordinary sandbox with the same spec, a fresh id,
  and a fresh TTL. Budgets and no-network apply to every fork identically.
- Forks diverge from their first write. If you edit `solution.py` in fork A, fork
  B never sees it. Parallel evaluation is sound because sibling forks are isolated.
- Snapshots **outlive their source**: the example destroys the source sandbox
  before forking, which is exactly what an overnight fan-out does.
- For cost accounting, `stats()` per fork tells you which candidate was both
  wrong and expensive.
