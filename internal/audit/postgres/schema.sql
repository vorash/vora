-- Vora audit schema (PostgreSQL / Supabase).
-- Applied idempotently by the embedded migrator.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sandbox_events (
    id         BIGSERIAL PRIMARY KEY,
    sandbox_id TEXT        NOT NULL,
    from_state TEXT        NOT NULL,
    to_state   TEXT        NOT NULL,
    reason     TEXT,
    runner_id  TEXT,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS sandbox_events_sandbox_id_idx
    ON sandbox_events (sandbox_id, occurred_at);

CREATE TABLE IF NOT EXISTS sandbox_execs (
    execution_id TEXT PRIMARY KEY,
    sandbox_id   TEXT        NOT NULL,
    command      TEXT        NOT NULL,
    args         TEXT[],
    cwd          TEXT,
    started_at   TIMESTAMPTZ NOT NULL,
    finished_at  TIMESTAMPTZ NOT NULL,
    exit_code    INTEGER     NOT NULL,
    reason       TEXT,
    stdout_bytes BIGINT      NOT NULL DEFAULT 0,
    stderr_bytes BIGINT      NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS sandbox_execs_sandbox_id_idx
    ON sandbox_execs (sandbox_id, started_at);
