# plugin-runner — user-uploaded plugins as a product feature

**The problem.** Your product is extensible: users upload small data-transform
plugins (map a CSV column, normalize a payload, generate a report). The plugin
runs as part of *your* pipeline, on *your* workers, next to *your* data. Every
uploaded plugin is arbitrary code from a stranger.

**The move.** Run every plugin in its own sandbox with a strict profile:
no network, few PIDs, small memory, bounded disk and a short TTL. The plugin
receives input on stdin and returns output on stdout, and it has no other interface.
`run.mjs` is the executor. It takes a plugin and returns the result, plus the
resource receipt (`stats`) and the audit record for compliance.

## Run it

```bash
cd examples && npm install
node plugin-runner/run.mjs plugin-runner/sample-plugin.mjs data.csv
```

`sample-plugin.mjs` is a tiny CSV normalizer (reads stdin, writes stdout). Try
swapping it for a hostile one that reads `/etc/shadow`, opens a socket, or forks
forever. It fails inside the sandbox while `run.mjs` returns a clean error.

## Profiles

`run.mjs` has three presets, chosen by the `PLUGIN_PROFILE` environment variable (`strict` by default):
`strict` (no network, 32 pids, 64Mi), `standard` (no network, 64 pids,
256Mi), and `network` (restricted egress and an allowlist, for plugins that fetch).
Ship `strict` for first-party marketplace plugins and require an audit-clean
run history before upgrading a publisher's profile.

## What to notice

- The plugin's **only** contract is stdin and stdout. It has no shared filesystem
  and no plugin API import from the host, and the product's own code never loads
  the plugin.
- `stats()` after each run gives memory peak and CPU. Bill by consumption, or
  cap publishers at a quota.
- The audit trail (`events`, `execs`) is per plugin run. It is a compliance answer to
  "what did this third-party code do inside our system on March 3rd?".
