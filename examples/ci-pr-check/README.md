# ci-pr-check — run untrusted PR tests without risking your CI runner

**The problem.** A pull request from the internet arrives with a patch that
edits `package.json` and the test suite. Your self-hosted CI runner is a
machine you care about: it has cloud credentials, SSH keys, and reachability
into your network. Running `npm test` from that PR executes whatever the
author wrote, on that machine.

**The move.** Check the PR out into a Vora sandbox instead: no network, capped
CPU/memory/PIDs, read-only image rootfs, and the whole execution recorded.
The runner machine only ever copies bytes in and out.

`run.sh` takes a directory (the PR checkout), copies it into a fresh sandbox,
runs the test command under limits, and reports exit code, output and resource
usage. Failures and explosions (`OOMKilled`, `PidsLimitExceeded`) come back as
first-class results.

## Run it

```bash
./run.sh <path-to-pr-checkout> [test-command...]

# e.g. a Node PR:
./run.sh ~/src/pr-1234 npm test

# or a Python one, on the Python image:
VORA_IMAGE=vora/python ./run.sh ~/src/pr-5678 python -m pytest
```

Output is the streamed test log followed by a summary; the exit code is the
test command's, so CI fails exactly when the PR's tests fail.

## Wiring into GitHub Actions

[`github-action.yml`](github-action.yml) is a complete workflow: check out the
PR's head onto a self-hosted machine, run `run.sh`, stream logs to the job.
The trusted parts of the workflow (checkout, reporting) run on the host; the
PR's *code* only ever runs inside the sandbox.

## What to notice

- `--network none` — a PR cannot exfiltrate your secrets or phone home.
- `--memory/--cpu/--pids/--disk/--ttl` — a fork bomb or memory bomb becomes a
  `PidsLimitExceeded` / `OOMKilled` result, not a dead runner.
- `vora logs` + `vora stats` at the end — the audit record of what ran and what
  it consumed. In a dispute ("CI killed my PR unfairly") this is the evidence.
- The test command runs through `vora exec`, which streams stdout/stderr live,
  so the CI log fills in real time.
