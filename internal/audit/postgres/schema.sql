-- Vora audit schema (PostgreSQL / Supabase).
-- Applied idempotently by the embedded migrator.

-- Runners sharing a database migrate at the same boot. The script runs as one
-- transaction whose CREATE INDEX and ALTER TABLE statements take table locks in
-- an order that deadlocks against itself, so one runner goes at a time.
SELECT pg_advisory_xact_lock(hashtext('vora_audit_schema'));

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

-- What an execution used. NULL is "not measured" (a row from before these
-- columns, or a window that could not be read) and zero is a measurement, so
-- none of these may gain a default.
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS cpu_millis BIGINT;
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS memory_peak_bytes BIGINT;
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS memory_peak_scope TEXT;
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS oom_kills BIGINT;
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS pids_limit_hits BIGINT;
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS egress_allowed BIGINT;
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS egress_denied BIGINT;

-- A pool run names its pool and its run inside a reused sandbox; empty and zero
-- mean it was not a pool run.
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS pool_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_execs ADD COLUMN IF NOT EXISTS sandbox_run INTEGER NOT NULL DEFAULT 0;

-- A pool event (reset, retire, alive, destroy, pool_create, pool_drain,
-- pool_delete). detail can hold raw backend text, as reason does, so both stay
-- behind the grants and row-level security below.
ALTER TABLE sandbox_events ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_events ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';

-- Vora connects as the table owner over the direct DSN, so it bypasses RLS
-- (no FORCE here on purpose). This closes the Supabase PostgREST/anon-key
-- exposure of the audit trail, which otherwise defaults to world-readable in
-- the public schema.
ALTER TABLE schema_migrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandbox_events    ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandbox_execs     ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandbox_egress    ENABLE ROW LEVEL SECURITY;

-- `anon`/`authenticated` only exist on Supabase; keep this portable for
-- self-hosted Postgres and testcontainers.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    EXECUTE 'REVOKE ALL ON schema_migrations, sandbox_events, sandbox_execs, sandbox_egress FROM anon, authenticated';
  END IF;
END $$;
