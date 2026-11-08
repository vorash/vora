# Vora in the real world

These are runnable recipes, not API tours. Each directory is a small,
self-contained application that uses the `vora` CLI and the
[TypeScript SDK](../sdk/typescript) to solve a concrete problem: running code you
do **not** trust. Read them as starting points to adapt.

| Recipe | The problem it solves | Interface |
| ------ | --------------------- | --------- |
| [ci-pr-check](ci-pr-check/) | Run an untrusted PR's test suite without letting it touch your CI runner | CLI + GitHub Actions |
| [autograder](autograder/) | Grade untrusted submissions (homework, CTF) with hard limits and an audit trail | CLI |
| [release-bot](release-bot/) | Automation that calls GitHub with a real token the workload never sees | CLI + credential broker |
| [codegen-gate](codegen-gate/) | Accept AI-generated code only after it passes its tests in isolation | JS SDK (HTTP service) |
| [plugin-runner](plugin-runner/) | Execute user-uploaded data-transform plugins as part of your product | JS SDK |
| [data-workbench](data-workbench/) | Let people run each other's analysis scripts on shared data, safely | JS SDK |
| [patch-fanout](patch-fanout/) | Evaluate N candidate patches from one repo state in parallel | JS SDK (snapshots + forks) |
| [agent-tool-runner](agent-tool-runner/) | Give an LLM agent a `run_code` tool without executing anything on your host | JS SDK (agent loop) |

## Prerequisites

Every recipe assumes a working Vora installation:

```bash
vora-runner &                 # local runner (make install-rootless for the service)
vora sandbox create --image vora/base && vora sandbox destroy sbx_...   # sanity
```

The JS SDK recipes also need the control plane and an API key
(`vora-api`, `vora keys create`, `VORA_API_KEY`). The release-bot recipe needs
`vora-api` too, started with `VORA_MASTER_KEY` for the secret store. See the [README](../README.md) for
setting up the API and images, and [docs/configuration.md](../docs/configuration.md)
for every setting.

The JavaScript recipes share one package:

```bash
cd examples
npm install          # links the local @vorash/sdk (build it first: npm run build at the repo root)
node codegen-gate/server.mjs
```

## The rules these recipes follow

- **Cleanup is part of the example**: every recipe destroys what it creates
  (`trap` in shell, `try/finally` in JS) and does not leave sandboxes behind.
- **A pinned, portable backend**: recipes run on `bwrap`, which is available on
  any host and unprivileged. Set `VORA_BACKEND=gvisor` to run them on a hosted
  runner with gVisor.
- **Least privilege by default**: recipes use `--network none` unless the job
  needs egress. A job that needs egress uses `restricted` with an explicit
  allowlist, or brokered credentials.
- **Budgets are not optional**: memory, pids and TTL are always set. A runaway
  workload gets `OOMKilled`, `PidsLimitExceeded`, or `TimeoutExceeded`, not your
  host.
- **Read the audit trail**: `vora logs`, `vora stats`, and the exec and egress
  records are how you explain, after the fact, exactly what untrusted code did.
