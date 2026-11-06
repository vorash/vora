# ci-pr-check — run untrusted PR tests without risking your CI runner

**The problem.** A pull request from the internet arrives with a patch that
edits `package.json` and the test suite. Your self-hosted CI runner is a
machine you care about: it has cloud credentials, SSH keys, and reachability
into your network. Running `npm test` from that PR executes whatever the
author wrote, on that machine.

**The move.** Check the PR out into a Vora sandbox instead. The sandbox has no
network and a read-only image rootfs. CPU, memory, and PIDs are capped, and Vora
records the whole execution.
The runner machine only ever copies bytes in and out.

`run.sh` takes a directory (the PR checkout), copies it into a fresh sandbox,
runs the test command under limits, and reports exit code, output and resource
usage. Failures and runaway workloads (`OOMKilled`, `PidsLimitExceeded`) come
back as ordinary results.

## Run it

```bash
./run.sh <path-to-pr-checkout> [test-command...]

# e.g. a Node PR:
./run.sh ~/src/pr-1234 npm test

# or a Python one, on the Python image:
VORA_IMAGE=vora/python ./run.sh ~/src/pr-5678 python -m pytest
```

Output is the streamed test log followed by a summary. The exit code is the
test command's, so CI fails exactly when the PR's tests fail.

## Wiring into GitHub Actions

[`github-action.yml`](github-action.yml) is a complete workflow. It checks out
the PR's head onto a self-hosted machine, runs `run.sh`, and streams logs to the
job. The trusted parts of the workflow (checkout, reporting) run on the host. The
PR's *code* only ever runs inside the sandbox.

## What to notice

- With `--network none`, a PR cannot exfiltrate your secrets or phone home.
- With `--memory/--cpu/--pids/--disk/--ttl`, a fork bomb or memory bomb becomes a
  `PidsLimitExceeded` or `OOMKilled` result, not a dead runner.
- `vora logs` and `vora stats` at the end give the audit record of what ran and
  what it consumed. In a dispute ("CI killed my PR unfairly"), this record is the
  evidence.
- The test command runs through `vora exec`, which streams stdout/stderr live,
  so the CI log fills in real time.
