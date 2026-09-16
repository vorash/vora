# ADR 010 — Audit storage behind a `Store` interface (Supabase)

- Status: accepted
- Date: 2026-03-24

## Context

v0.1 wrote lifecycle events to `events.jsonl` and executions to
`exec/<id>.json` per sandbox. That is fine locally but not queryable, not
durable, and not multi-runner. The audit trail is a product pillar, so it needs
a real store — without forcing a database on every local run.

The team already has a **Supabase** project (hosted PostgreSQL) for Vora.

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

- **`audit/jsonl`** — the default; preserves v0.1 behaviour.
- **`audit/postgres`** — `pgx/v5` pool against **Supabase** (hosted Postgres),
  schema embedded and applied by a minimal migrator (`schema_migrations` table).
- The runner selects: `VORA_AUDIT_DSN` set → Postgres; otherwise JSONL.
- Supabase specifics: SSL required (`sslmode=require`); use the session pooler
  DSN for the runner, the direct connection for migrations. The DSN is read from
  1Password (`op read`) or injected as an env var and never committed.
- Tables `sandbox_events`, `sandbox_execs`, indexed on `sandbox_id`.
- Tests use `testcontainers-go` (`postgres:16-alpine`) for hermetic coverage,
  plus an env-gated test against a real Supabase project that skips by default.

## Consequences

- Local development and CI need no database.
- Hosted runs get durable, queryable audit data on managed Postgres (no DB
  operations to run ourselves).
- Supabase is external: the runner needs network access and a DSN; audit writes
  must not block execution if the DB is unavailable (logged, not fatal).
- Connection pooling matters for many concurrent runners; the session pooler is
  the default and pooling strategy is configurable via the DSN.