# patch-fanout — evaluate N candidate patches from one repo state, in parallel

**The problem.** An agent proposes five candidate patches for a failing test.
Which one actually fixes it? Checking each candidate means resetting to the
same repo state, applying one patch, running the suite — serially, or on five
checkouts that drift apart. And the patches are agent-written, i.e. untrusted.

**The move.** Snapshot the prepared repo state **once** — live, while its
sandbox runs — then fork it once per candidate. Each fork is an independent
sandbox that starts from exactly the same state (crash-consistent copy of
`/workspace` + `/root`, same image; [ADR 023](../docs/adr/023-snapshot-fork.md)).
Apply one patch per fork, run the tests **concurrently**, collect a verdict
table, tear everything down.

This is the shape the snapshot/fork feature exists for: fan out from one state,
evaluate, throw the copies away.

## Run it

```bash
cd examples && npm install
node patch-fanout/fanout.mjs
```

## What to notice

- `vora.snapshots.create(sandbox.id)` takes the snapshot **while the source
  keeps running** — the copy is live and crash-consistent, no quiescing step.
- `snapshot.fork()` is an ordinary sandbox under the hood: same spec, fresh id,
  fresh TTL. Budgets and no-network apply to every fork identically.
- Forks diverge from their first write — edit `solution.py` in fork A and fork
  B never sees it. Sibling isolation is what makes parallel evaluation sound.
- Snapshots **outlive their source**: the example destroys the source sandbox
  before forking, which is exactly what an overnight fan-out does.
- Cost accounting: `stats()` per fork tells you which candidate was not just
  wrong but expensive.
