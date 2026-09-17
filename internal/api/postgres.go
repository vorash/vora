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

create table if not exists sandbox_routes (
  sandbox_id text primary key,
  runner_id  text not null,
  created_at timestamptz not null default now()
);
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
		`insert into api_keys (id, name, tenant_id, key_hash, scopes, created_at, revoked)
		 values ($1,$2,$3,$4,$5,$6,$7)`,
		key.ID, key.Name, key.TenantID, key.KeyHash, key.Scopes, key.CreatedAt, key.Revoked)
	return err
}

func (s *PostgresStore) ListKeys(ctx context.Context, tenant string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`select id, name, tenant_id, scopes, created_at, coalesce(last_used, 'epoch'::timestamptz), revoked
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
		if err := rows.Scan(&k.ID, &k.Name, &k.TenantID, &k.Scopes, &k.CreatedAt, &k.LastUsed, &k.Revoked); err != nil {
			return nil, err
		}
		k.KeyHash = ""
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PostgresStore) KeyByHash(ctx context.Context, hash string) (APIKey, error) {
	var k APIKey
	err := s.pool.QueryRow(ctx,
		`select id, name, tenant_id, key_hash, scopes, created_at, coalesce(last_used, 'epoch'::timestamptz), revoked
		   from api_keys where key_hash = $1`, hash).
		Scan(&k.ID, &k.Name, &k.TenantID, &k.KeyHash, &k.Scopes, &k.CreatedAt, &k.LastUsed, &k.Revoked)
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
	_, err := s.pool.Exec(ctx,
		`insert into runners (id, address, backends, cpu_millis, memory_bytes, max_sandboxes, running, version, registered_at, last_heartbeat)
		 values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 on conflict (id) do update set
		   address = excluded.address,
		   backends = excluded.backends,
		   cpu_millis = excluded.cpu_millis,
		   memory_bytes = excluded.memory_bytes,
		   max_sandboxes = excluded.max_sandboxes,
		   running = excluded.running,
		   version = excluded.version,
		   last_heartbeat = excluded.last_heartbeat`,
		r.ID, r.Address, r.Backends, r.CPUMillis, r.MemoryBytes, r.MaxSandboxes, r.Running, r.Version, r.RegisteredAt, r.LastHeartbeat)
	return err
}

func (s *PostgresStore) ListRunners(ctx context.Context) ([]Runner, error) {
	rows, err := s.pool.Query(ctx,
		`select id, address, backends, cpu_millis, memory_bytes, max_sandboxes, running, version, registered_at, last_heartbeat
		   from runners order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		var r Runner
		if err := rows.Scan(&r.ID, &r.Address, &r.Backends, &r.CPUMillis, &r.MemoryBytes, &r.MaxSandboxes,
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
		`select id, address, backends, cpu_millis, memory_bytes, max_sandboxes, running, version, registered_at, last_heartbeat
		   from runners where id = $1`, id).
		Scan(&r.ID, &r.Address, &r.Backends, &r.CPUMillis, &r.MemoryBytes, &r.MaxSandboxes,
			&r.Running, &r.Version, &r.RegisteredAt, &r.LastHeartbeat)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) SetRoute(ctx context.Context, sandboxID, runnerID string) error {
	_, err := s.pool.Exec(ctx,
		`insert into sandbox_routes (sandbox_id, runner_id) values ($1,$2)
		 on conflict (sandbox_id) do update set runner_id = excluded.runner_id`,
		sandboxID, runnerID)
	return err
}

func (s *PostgresStore) Route(ctx context.Context, sandboxID string) (string, error) {
	var runnerID string
	err := s.pool.QueryRow(ctx, `select runner_id from sandbox_routes where sandbox_id = $1`, sandboxID).Scan(&runnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return runnerID, err
}

func (s *PostgresStore) DeleteRoute(ctx context.Context, sandboxID string) error {
	_, err := s.pool.Exec(ctx, `delete from sandbox_routes where sandbox_id = $1`, sandboxID)
	return err
}
