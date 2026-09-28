# ADR 026 — Audit trail outlives the sandbox

- Status: accepted
- Date: 2026-09 (V-26, `vorash/vora#88`)

## Context

The audit trail is what makes executed code attributable. The default JSONL
store kept it under `<sandbox>/logs`, and `workspace.Clean` — run by destroy,
TTL expiry, disk-quota enforcement and reconcile — removed that directory with
the workspace. A key with the `sandbox` scope, including an agent-bound
workload key, could therefore erase the record of what its sandbox ran and
which hosts it contacted by destroying the sandbox, which is also how every
sandbox normally ends.

Separately, when `VORA_AUDIT_DSN` was set but Postgres was unreachable at
runner start, the runner logged a warning and served on the ephemeral JSONL
store, so the operator silently got a weaker guarantee than they configured.

## Decision

**Destroy never deletes audit.** `Clean` removes `workspace/`, `home/` and
`ipc/` only. `logs/` (events, exec records, egress decisions) stays next to
`config.json` as part of the sandbox's tombstone. `logs/` is not mounted into
either backend, so the workload cannot write to it. The failed-create rollback
still removes the whole directory; nothing has run by then.

**Retention is a reaper policy, off by default.** `VORA_AUDIT_RETENTION` (a Go
duration) makes the runner's periodic reaper remove the tombstone, audit trail
included, of sandboxes that reached a terminal state (`DESTROYED`, `FAILED`,
`EXPIRED`) longer ago than the limit. `STOPPED` is not terminal and is never
pruned. Unset means keep forever: deleting evidence is an operator decision, not
a default. There is no size-based limit. Retention applies only to the local
JSONL trail: with a Postgres audit store the tombstone is what lets the API
reach that store's rows, so the reaper leaves it alone and the rows are the
operator's to retain.

**A configured Postgres audit store is required, not preferred.** With
`VORA_AUDIT_DSN` set, failing to connect or migrate is a startup error. The
runner units set `StartLimitIntervalSec=0` so systemd keeps retrying while the
database converges (as `vora-api` already does) instead of giving up after five
starts.

**Lost exec records are metered.** A failed exec-record write increments
`vora_audit_write_failed_total`, the counter egress write failures already use.
It still does not fail the caller's command; a fail-closed option for execs, as
egress has, is left for a later decision.

## Consequences

- Disk use per finished sandbox is no longer bounded to `config.json`; operators
  who do not set `VORA_AUDIT_RETENTION` keep every trail.
- **Accepted residual risk: caller-driven disk growth.** The egress log is
  capped at 1 MiB per sandbox. An exec record carries the caller's command,
  arguments and cwd, bounded only by the API's 1 MiB request-body limit, and
  sandbox count limits apply to active sandboxes, not tombstones. A tenant key
  looping create, exec, destroy can therefore grow the runner's disk until the
  operator sets `VORA_AUDIT_RETENTION` or the disk is full. Keeping evidence is
  still the right default; operators exposed to untrusted tenants should set a
  retention limit and alert on runner disk.
- A runner with `VORA_AUDIT_DSN` set does not start until the database is up.
- Regression tests: `TestAuditSurvivesDestroy`, `…TTLExpiry`,
  `…DiskQuotaEnforcement`, `…Reconcile` and `TestPruneAudit*` in
  `internal/runner`, `TestCleanKeepsAuditLogs` in `internal/workspace`,
  `TestOpenAuditFailsWhenPostgresUnreachable` in `cmd/vora-runner`.
