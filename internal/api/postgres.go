package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresSchema = `
create table if not exists api_keys (
  id           text primary key,
  name         text not null default '',
  tenant_id    text not null default '',
  agent_id     text not null default '',
  key_hash     text not null unique,
  scopes       text[] not null default '{}',
  created_at   timestamptz not null default now(),
  last_used    timestamptz,
  revoked      boolean not null default false
);
create index if not exists api_keys_tenant_idx on api_keys (tenant_id);

create table if not exists runners (
  id             text primary key,
  address        text not null,
  backends       text[] not null default '{}',
  cpu_millis     bigint not null default 0,
  memory_bytes   bigint not null default 0,
  max_sandboxes  integer not null default 0,
  running        integer not null default 0,
  version        text not null default '',
  registered_at  timestamptz not null default now(),
  last_heartbeat timestamptz not null default now()
);
alter table runners add column if not exists network_modes text[] not null default '{}';
alter table runners add column if not exists tunneled boolean not null default false;
alter table api_keys add column if not exists agent_id text not null default '';

create table if not exists sandbox_routes (
  sandbox_id text primary key,
  runner_id  text not null,
  tenant_id  text not null default '',
  agent_id   text not null default '',
  created_at timestamptz not null default now()
);
alter table sandbox_routes add column if not exists tenant_id text not null default '';
alter table sandbox_routes add column if not exists agent_id text not null default '';
alter table sandbox_routes add column if not exists cpu_millis bigint not null default 0;
alter table sandbox_routes add column if not exists memory_bytes bigint not null default 0;
create index if not exists sandbox_routes_tenant_idx on sandbox_routes (tenant_id);

create table if not exists snapshot_routes (
  snapshot_id text primary key,
  runner_id   text not null,
  tenant_id   text not null default '',
  agent_id    text not null default '',
  sandbox_id  text not null default '',
  created_at  timestamptz not null default now()
);
alter table snapshot_routes add column if not exists agent_id text not null default '';
create index if not exists snapshot_routes_tenant_idx on snapshot_routes (tenant_id);

-- The API connects as the table owner, which bypasses RLS; enabling it keeps
-- api_keys (key hashes), runners and routing state out of Supabase's PostgREST
-- API and away from the anon key.
alter table api_keys        enable row level security;
alter table runners         enable row level security;
alter table sandbox_routes  enable row level security;
alter table snapshot_routes enable row level security;

do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    execute 'revoke all on api_keys, runners, sandbox_routes, snapshot_routes from anon, authenticated';
  end if;
end $$;
`

// PostgresStore is a persistent Store backed by PostgreSQL/Supabase.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects and applies the schema.
func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("api: connect postgres: %w", err)
	}
	s := &PostgresStore{pool: pool}
	if _, err := pool.Exec(ctx, postgresSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("api: apply schema: %w", err)
	}
	return s, nil
}

// Close releases the pool.
func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

func (s *PostgresStore) CreateKey(ctx context.Context, key APIKey) error {
	_, err := s.pool.Exec(ctx,
		`insert into api_keys (id, name, tenant_id, agent_id, key_hash, scopes, created_at, revoked)
		 values ($1,$2,$3,$4,$5,$6,$7,$8)`,
		key.ID, key.Name, key.TenantID, key.AgentID, key.KeyHash, key.Scopes, key.CreatedAt, key.Revoked)
	return err
}

func (s *PostgresStore) ListKeys(ctx context.Context, tenant string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`select id, name, tenant_id, agent_id, scopes, created_at, coalesce(last_used, 'epoch'::timestamptz), revoked
		   from api_keys
		  where ($1 = '' or tenant_id = $1)
		  order by created_at`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.TenantID, &k.AgentID, &k.Scopes, &k.CreatedAt, &k.LastUsed, &k.Revoked); err != nil {
			return nil, err
		}
		k.KeyHash = ""
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Key(ctx context.Context, id string) (APIKey, error) {
	var k APIKey
	err := s.pool.QueryRow(ctx,
		`select id, name, tenant_id, agent_id, scopes, created_at, coalesce(last_used, 'epoch'::timestamptz), revoked
		   from api_keys where id = $1`, id).
		Scan(&k.ID, &k.Name, &k.TenantID, &k.AgentID, &k.Scopes, &k.CreatedAt, &k.LastUsed, &k.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	return k, err
}

func (s *PostgresStore) KeyByHash(ctx context.Context, hash string) (APIKey, error) {
	var k APIKey
	err := s.pool.QueryRow(ctx,
		`select id, name, tenant_id, agent_id, key_hash, scopes, created_at, coalesce(last_used, 'epoch'::timestamptz), revoked
		   from api_keys where key_hash = $1`, hash).
		Scan(&k.ID, &k.Name, &k.TenantID, &k.AgentID, &k.KeyHash, &k.Scopes, &k.CreatedAt, &k.LastUsed, &k.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	return k, err
}

func (s *PostgresStore) RevokeKey(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `update api_keys set revoked = true where id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) TouchKey(ctx context.Context, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `update api_keys set last_used = $2 where id = $1`, id, at)
	return err
}

func (s *PostgresStore) UpsertRunner(ctx context.Context, r Runner) error {
	// A nil []string is sent as SQL NULL, which violates the not-null columns;
	// empty arrays are the right value for a runner with no backends/modes.
	backends := r.Backends
	if backends == nil {
		backends = []string{}
	}
	networkModes := r.NetworkModes
	if networkModes == nil {
		networkModes = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`insert into runners (id, address, tunneled, backends, network_modes, cpu_millis, memory_bytes, max_sandboxes, running, version, registered_at, last_heartbeat)
		 values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 on conflict (id) do update set
		   address = excluded.address,
		   tunneled = excluded.tunneled,
		   backends = excluded.backends,
		   network_modes = excluded.network_modes,
		   cpu_millis = excluded.cpu_millis,
		   memory_bytes = excluded.memory_bytes,
		   max_sandboxes = excluded.max_sandboxes,
		   running = excluded.running,
		   version = excluded.version,
		   last_heartbeat = excluded.last_heartbeat`,
		r.ID, r.Address, r.Tunneled, backends, networkModes, r.CPUMillis, r.MemoryBytes, r.MaxSandboxes, r.Running, r.Version, r.RegisteredAt, r.LastHeartbeat)
	return err
}

func (s *PostgresStore) ListRunners(ctx context.Context) ([]Runner, error) {
	rows, err := s.pool.Query(ctx,
		`select id, address, tunneled, backends, network_modes, cpu_millis, memory_bytes, max_sandboxes, running, version, registered_at, last_heartbeat
		   from runners order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		var r Runner
		if err := rows.Scan(&r.ID, &r.Address, &r.Tunneled, &r.Backends, &r.NetworkModes, &r.CPUMillis, &r.MemoryBytes, &r.MaxSandboxes,
			&r.Running, &r.Version, &r.RegisteredAt, &r.LastHeartbeat); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Runner(ctx context.Context, id string) (Runner, error) {
	var r Runner
	err := s.pool.QueryRow(ctx,
		`select id, address, tunneled, backends, network_modes, cpu_millis, memory_bytes, max_sandboxes, running, version, registered_at, last_heartbeat
		   from runners where id = $1`, id).
		Scan(&r.ID, &r.Address, &r.Tunneled, &r.Backends, &r.NetworkModes, &r.CPUMillis, &r.MemoryBytes, &r.MaxSandboxes,
			&r.Running, &r.Version, &r.RegisteredAt, &r.LastHeartbeat)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) SetRoute(ctx context.Context, route SandboxRoute) error {
	_, err := s.pool.Exec(ctx,
		`insert into sandbox_routes (sandbox_id, runner_id, tenant_id, agent_id, cpu_millis, memory_bytes) values ($1,$2,$3,$4,$5,$6)
		 on conflict (sandbox_id) do update set runner_id = excluded.runner_id, tenant_id = excluded.tenant_id,
		   agent_id = excluded.agent_id, cpu_millis = excluded.cpu_millis, memory_bytes = excluded.memory_bytes`,
		route.SandboxID, route.RunnerID, route.TenantID, route.AgentID, route.CPUMillis, route.MemoryBytes)
	return err
}

func (s *PostgresStore) Route(ctx context.Context, sandboxID string) (SandboxRoute, error) {
	route := SandboxRoute{SandboxID: sandboxID}
	err := s.pool.QueryRow(ctx, `select runner_id, tenant_id, agent_id, cpu_millis, memory_bytes from sandbox_routes where sandbox_id = $1`, sandboxID).
		Scan(&route.RunnerID, &route.TenantID, &route.AgentID, &route.CPUMillis, &route.MemoryBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return SandboxRoute{}, ErrNotFound
	}
	return route, err
}

func (s *PostgresStore) ListRoutes(ctx context.Context) ([]SandboxRoute, error) {
	rows, err := s.pool.Query(ctx, `select sandbox_id, runner_id, tenant_id, agent_id, cpu_millis, memory_bytes from sandbox_routes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SandboxRoute
	for rows.Next() {
		var r SandboxRoute
		if err := rows.Scan(&r.SandboxID, &r.RunnerID, &r.TenantID, &r.AgentID, &r.CPUMillis, &r.MemoryBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteRoute(ctx context.Context, sandboxID string) error {
	_, err := s.pool.Exec(ctx, `delete from sandbox_routes where sandbox_id = $1`, sandboxID)
	return err
}

func (s *PostgresStore) SetSnapshotRoute(ctx context.Context, route SnapshotRoute) error {
	_, err := s.pool.Exec(ctx,
		`insert into snapshot_routes (snapshot_id, runner_id, tenant_id, agent_id, sandbox_id) values ($1,$2,$3,$4,$5)
		 on conflict (snapshot_id) do update set runner_id = excluded.runner_id, tenant_id = excluded.tenant_id,
		   agent_id = excluded.agent_id, sandbox_id = excluded.sandbox_id`,
		route.SnapshotID, route.RunnerID, route.TenantID, route.AgentID, route.SandboxID)
	return err
}

func (s *PostgresStore) SnapshotRoute(ctx context.Context, snapshotID string) (SnapshotRoute, error) {
	route := SnapshotRoute{SnapshotID: snapshotID}
	err := s.pool.QueryRow(ctx, `select runner_id, tenant_id, agent_id, sandbox_id from snapshot_routes where snapshot_id = $1`, snapshotID).
		Scan(&route.RunnerID, &route.TenantID, &route.AgentID, &route.SandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SnapshotRoute{}, ErrNotFound
	}
	return route, err
}

func (s *PostgresStore) DeleteSnapshotRoute(ctx context.Context, snapshotID string) error {
	_, err := s.pool.Exec(ctx, `delete from snapshot_routes where snapshot_id = $1`, snapshotID)
	return err
}
