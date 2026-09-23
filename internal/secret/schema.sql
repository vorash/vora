-- Brokered third-party credentials. Values are sealed with AES-256-GCM under a
-- key derived from VORA_MASTER_KEY; only vora-api holds that key. Nonce and
-- ciphertext are stored separately so the sealed blob is self-describing.
CREATE TABLE IF NOT EXISTS vora_secrets (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    alias      text NOT NULL,
    host       text NOT NULL,
    scheme     text NOT NULL,
    header     text NOT NULL DEFAULT '',
    nonce      bytea NOT NULL,
    ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, alias)
);

CREATE INDEX IF NOT EXISTS vora_secrets_tenant_idx ON vora_secrets (tenant_id, alias);

-- Brokered credential blobs must never be reachable through Supabase's
-- PostgREST API. The owner role (Vora's DSN) bypasses RLS, so this only
-- affects the anon/authenticated roles.
ALTER TABLE vora_secrets ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    EXECUTE 'REVOKE ALL ON vora_secrets FROM anon, authenticated';
  END IF;
END $$;
