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

CREATE TABLE IF NOT EXISTS sandbox_egress (
    id          BIGSERIAL PRIMARY KEY,
    sandbox_id  TEXT        NOT NULL,
    host        TEXT,
    remote      TEXT,
    alias       TEXT        NOT NULL DEFAULT '',
    allowed     BOOLEAN     NOT NULL,
    reason      TEXT,
    occurred_at TIMESTAMPTZ NOT NULL
);

-- Credential-broker decisions carry the alias they were made for (never the
-- secret value).
ALTER TABLE sandbox_egress ADD COLUMN IF NOT EXISTS alias TEXT NOT NULL DEFAULT '';

-- Records attribute actions to the caller: tenant and agent identity are
-- derived from the sandbox spec, key_id marks request-driven actions (ADR 025).
ALTER TABLE sandbox_events ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_events ADD COLUMN IF NOT EXISTS agent_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_events ADD COLUMN IF NOT EXISTS key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS agent_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_egress ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_egress ADD COLUMN IF NOT EXISTS agent_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_egress ADD COLUMN IF NOT EXISTS key_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sandbox_egress_sandbox_id_idx
    ON sandbox_egress (sandbox_id, occurred_at, id);
