# ADR 026 — Audit trail outlives the sandbox

- Status: accepted
- Date: 2026-09 (V-26, `vorash/vora#88`)

## Context

The audit trail is what makes executed code attributable. The default JSONL
store kept it under `<sandbox>/logs`, and `workspace.Clean`, run by destroy,
TTL expiry, disk-quota enforcement and reconcile, removed that directory with
the workspace. A key with the `sandbox` scope, including an agent-bound
workload key, could therefore erase the record of what its sandbox ran and
which hosts it contacted by destroying the sandbox, which is also how every
sandbox normally ends.

Separately, when `VORA_AUDIT_DSN` was set but Postgres was unreachable at
runner start, the runner logged a warning and served on the ephemeral JSONL
store, so the operator silently got a weaker guarantee than they configured.

## Decision

**Destroy never deletes audit.** `Clean` reduces the sandbox directory to its
tombstone: `config.json` and `logs/` (events, exec records, egress decisions).
Everything else goes, including backend leftovers such as a gVisor `bundle/`
or a bound `resolv.conf`, which are not audit data. `logs/` is not mounted into
either backend, so the workload cannot write to it. The failed-create rollback
still removes the whole directory, because nothing has run by then.

**The API keeps the route.** A trail the API cannot reach is as hidden as a
deleted one, and every audit read (`events`, `execs`, `egress`) finds its runner
through the sandbox route. `vora-api` therefore marks the route `destroyed` on
destroy instead of deleting it. A destroyed route keeps its tenant and agent
scoping but no longer counts toward placement. When the runner answers
`NotFound` (its tombstone was pruned), the API drops the route.

**Retention is a reaper policy, off by default.** `VORA_AUDIT_RETENTION` (a Go
duration) makes the runner's periodic reaper remove the tombstone, audit trail
included, of sandboxes that reached a terminal state (`DESTROYED`, `FAILED`,
`EXPIRED`) longer ago than the limit. `STOPPED` is not terminal and is never
pruned. Unset means keep forever: deleting evidence is an operator decision, not
a default. There is no size-based limit. Pruning runs every 10 minutes, apart
from the 30-second reaper, and re-checks each record under the same
per-sandbox lock destroy holds, so it never deletes a tombstone a concurrent
destroy is rewriting. A sandbox that failed without being destroyed may still
hold a project quota. Pruning releases it before deleting the directory.

Retention applies only to the runner's own JSONL trail. That is recorded when
the store is configured (installing another store with `SetAudit` turns it
off), not inferred from the store's type. With a Postgres audit store the rows
live in the database, and the runner only serves them for a sandbox whose
tombstone it still has, so the reaper leaves the tombstone alone and the rows
are the operator's to retain. `VORA_AUDIT_RETENTION` is read by the runner
only. The CLI never parses it, so a bad value cannot break CLI commands.

**A configured Postgres audit store is required, not preferred.** With
`VORA_AUDIT_DSN` set, failing to connect or migrate is a startup error. The
runner units set `StartLimitIntervalSec=0` so systemd keeps retrying while the
database converges (as `vora-api` already does) instead of giving up after five
starts.

**Lost audit writes are metered.** A failed exec-record or lifecycle-event
write (including the `DESTROYED` transition) increments
`vora_audit_write_failed_total`, the counter egress write failures already use.
It still does not fail the caller's command. A fail-closed option for execs, as
egress has, is left for a later decision.

## Consequences

- Disk use per finished sandbox is no longer bounded to `config.json`. Operators
  who do not set `VORA_AUDIT_RETENTION` keep every trail.
- **Accepted residual risk: caller-driven disk growth.** The egress log is
  capped at 1 MiB per sandbox. An exec record carries the caller's command,
  arguments and cwd, bounded only by the API's 1 MiB request-body limit, and
  sandbox count limits apply to active sandboxes, not tombstones. A tenant key
  looping create, exec, destroy can therefore grow the runner's disk until the
  operator sets `VORA_AUDIT_RETENTION` or the disk is full. Keeping evidence is
  still the right default. Operators exposed to untrusted tenants should set a
  retention limit and alert on runner disk.
- A runner with `VORA_AUDIT_DSN` set does not start until the database is up.
- The API's route table keeps one row per destroyed sandbox until a later
  destroy finds the runner's tombstone pruned.
- A sandbox that failed without being destroyed (for example after a runner
  restart) still counts toward placement until someone destroys it. That
  predates this ADR. Pruning its tombstone does not strand the route, because
  the next destroy gets `NotFound` and drops it.
- Regression tests: `TestAuditSurvivesDestroy`, `…TTLExpiry`,
  `…DiskQuotaEnforcement`, `…Reconcile`, `TestPruneAudit*`,
  `TestLifecycleEventWriteFailureIsMetered` in `internal/runner`;
  `TestAuditReadableThroughAPIAfterDestroy`,
  `TestDestroyedRouteReleasesPlacementCapacity`,
  `TestDestroyDropsRouteWhenRunnerForgotSandbox` in `internal/api`;
  `TestCleanKeepsAuditLogs`, `TestCleanLeavesOnlyTombstone` in
  `internal/workspace`; `TestDefaultIgnoresAuditRetention` in `internal/config`;
  `TestOpenAuditFailsWhenPostgresUnreachable` in `cmd/vora-runner`.
