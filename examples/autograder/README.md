# autograder — grade untrusted submissions with hard limits and an audit trail

**The problem.** You are grading 200 student submissions (or CTF writeup
scripts, or a coding challenge). Some are wrong, some are sloppy, and some are
hostile: fork bombs, `while True: open('/dev/zero').read()`, attempts
to read the grader's fixtures or attack the grading host. You still owe each
one a fair, identical environment and a defensible record of what happened.

**The move.** Grade each submission in its own sandbox with no network, capped
memory and PIDs, **bounded disk** (so "write 100GB" is a clean failure), and an
exec and audit record per submission. Every submission gets the same image and
the same budgets.

`grade.sh` grades every submission in a directory and prints a score line per
submission. A hostile submission scores zero instead of taking the grader down.
Its exec record holds the *semantic* outcome: `OOMKilled`, `TimeoutExceeded`,
`DiskQuotaExceeded`, or `RUNTIME_FAILURE` when the workload starves its own
exec agent at `pids.max`.

## Layout

```
submissions/          # one script per submission (here: three samples)
  alice.sh            # passes
  mallory.sh          # phones home (blocked) and reads the image's own /etc/shadow (useless)
  trudy.sh            # fork bomb — starves its exec agent at pids.max
grade.sh
```

## Run it

```bash
./grade.sh submissions/
```

## What to notice

- **The fixture is writeable but the grader is not.** The expected output is
  piped in over stdin. The submission only ever sees `/workspace` and `/root`.
- **`--disk 64m`** makes disk exhaustion a graded outcome (`DiskQuotaExceeded`)
  rather than a stuck host. Try a submission that writes forever.
- **The sandbox's `/etc/shadow` is the image's**, not the host's: mallory can
  read it and learns nothing. The grading host has no path into the sandbox.
- **Fairness is reproducibility.** Every submission gets the same image digest,
  budgets and fixture. `vora logs --execs` gives you the per-submission record
  (command, exit, CPU, memory peak) to attach to a grade dispute.
