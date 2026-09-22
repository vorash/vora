# data-workbench — run each other's analysis scripts on shared data, safely

**The problem.** A research group (or a data team, or a class) shares datasets
and shares analysis scripts. "Just run Alice's notebook" means running Alice's
code — with your credentials, on your machine, on the shared drive. Usually
fine. Occasionally `shutil.rmtree`, a crypto miner, or an exfiltration of the
dataset you are not supposed to redistribute.

**The move.** The workbench runs every script in a sandbox: the dataset goes
in, the analysis runs with **no network**, bounded CPU/memory and a **disk
quota** (a script that writes a 100GB intermediate hits `DiskQuotaExceeded`,
not a full volume), and the outputs come back as data — text, files, and a
resource receipt.

## Run it

```bash
cd examples && npm install
node data-workbench/workbench.mjs analysis.py scores.csv
```

`analysis.py` computes column statistics of the CSV on stdin. Swap in any
script — including a hostile one — and the sandbox boundary is the same.

## What to notice

- The dataset is written into the sandbox and outputs are read back through
  the API's file routes — which only ever reach `/workspace` and `/root`, so an
  "output" cannot be `/etc/passwd`.
- `disk: "256Mi"` makes runaway intermediate files a semantic failure
  (`DiskQuotaExceeded`) with the sandbox stopped for you.
- The run's `execs()` record (command, exit, CPU, memory peak) doubles as the
  reproducibility appendix for whatever the script concluded.
