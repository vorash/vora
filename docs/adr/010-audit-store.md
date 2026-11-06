# ADR 010 — Audit storage behind a `Store` interface (Supabase)

- Status: accepted
- Date: 2026-03-24

## Context

v0.1 wrote lifecycle events to `events.jsonl` and executions to
`exec/<id>.json` per sandbox. That is fine locally but not queryable, not
durable, and not multi-runner. The audit trail is a product pillar, so it needs
a real store, but one that does not force a database on every local run.

The team already has a Supabase project (hosted PostgreSQL) for Vora.

## Decision

Introduce `internal/audit.Store`:

```go
type Store interface {
    AppendEvent(ctx, sandboxID string, event sandbox.Event) error
    AppendExec(ctx, sandboxID string, exec sandbox.ExecRecord) error
    Events(ctx, sandboxID string) ([]sandbox.Event, error)
    Execs(ctx, sandboxID string) ([]sandbox.ExecRecord, error)
    Close() error
}
```

- **`audit/jsonl`** is the default and preserves v0.1 behaviour.
- **`audit/postgres`** is a `pgx/v5` pool against Supabase (hosted Postgres). Its
  schema is embedded and applied by a minimal migrator (`schema_migrations` table).
- The runner selects Postgres when `VORA_AUDIT_DSN` is set, and JSONL otherwise.
- Supabase specifics: SSL is required (`sslmode=require`). Use the session pooler
  DSN for the runner and the direct connection for migrations. The DSN is read from
  1Password (`op read`) or injected as an env var and never committed.
- The tables are `sandbox_events` and `sandbox_execs`, indexed on `sandbox_id`.
- Tests use `testcontainers-go` (`postgres:16-alpine`) for hermetic coverage,
  plus an env-gated test against a real Supabase project that skips by default.

Schema changes are additive `ADD COLUMN IF NOT EXISTS` statements in
`schema.sql`, applied at every runner start, with `schemaVersion` bumped. Runners
sharing a database migrate one at a time under an advisory lock. A column for a
measurement is nullable with no default, because NULL means not measured and zero
is a measurement. The suite in `internal/audit/postgres/postgrestest` upgrades a
database from the last released schema, kept in its `testdata`, with rows in it.

## Consequences

- Local development and CI need no database.
- Hosted runs get durable, queryable audit data on managed Postgres (no DB
  operations to run ourselves).
- Supabase is external, so the runner needs network access and a DSN. Audit writes
  must not block execution if the DB is unavailable (logged, not fatal).
- Connection pooling matters for many concurrent runners. The session pooler is
  the default, and the pooling strategy is configurable through the DSN.