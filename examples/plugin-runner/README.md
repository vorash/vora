# plugin-runner — user-uploaded plugins as a product feature

**The problem.** Your product is extensible: users upload small data-transform
plugins (map a CSV column, normalize a payload, generate a report). The plugin
runs as part of *your* pipeline, on *your* workers, next to *your* data. Every
uploaded plugin is arbitrary code from a stranger.

**The move.** Run every plugin in its own sandbox with a strict profile:
no network, few PIDs, small memory, bounded disk and a short TTL. The plugin
receives input on stdin and returns output on stdout — that is its entire
world. `run.mjs` is the executor: plugin in, result out, plus the resource
receipt (`stats`) and the audit record for compliance.

## Run it

```bash
cd examples && npm install
node plugin-runner/run.mjs plugin-runner/sample-plugin.mjs data.csv
```

`sample-plugin.mjs` is a tiny CSV normalizer (reads stdin, writes stdout). Try
swapping it for a hostile one — read `/etc/shadow`, open a socket, fork forever
— and watch it fail inside the box while `run.mjs` returns a clean error.

## Profiles

Three presets in `run.mjs`, chosen by the `--profile` env (`strict` default):
`strict` (no network, 32 pids, 64Mi), `standard` (no network, 64 pids,
256Mi), `network` (restricted egress + allowlist, for plugins that fetch).
Ship `strict` for first-party marketplace plugins and require an audit-clean
run history before upgrading a publisher's profile.

## What to notice

- The plugin's **only** contract is stdin/stdout. No shared filesystem, no
  plugin API import from the host — the product's own code never loads the
  plugin.
- `stats()` after each run gives memory peak / CPU — bill by consumption, or
  cap publishers at a quota.
- The audit trail (`events`, `execs`) is per-plugin-run: a compliance answer to
  "what did this third-party code do inside our system on March 3rd?".
