package secret

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// PostgresStore is a persistent Store backed by PostgreSQL/Supabase. It holds
// the sealer, so plaintext exists only inside a single Put/Reveal call.
type PostgresStore struct {
	pool   *pgxpool.Pool
	sealer *Sealer
}

var _ Store = (*PostgresStore)(nil)

// OpenPostgres connects, applies the schema and returns a store.
func OpenPostgres(ctx context.Context, dsn string, sealer *Sealer) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("secret: connect postgres: %w", err)
	}
	s := &PostgresStore{pool: pool, sealer: sealer}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Migrate applies the embedded schema. It is idempotent.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("secret: migrate: %w", err)
	}
	return nil
}

// Put implements Store.
func (s *PostgresStore) Put(ctx context.Context, rec Record, value string) error {
	stored, err := prepare(s.sealer, rec, value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO vora_secrets (id, tenant_id, alias, host, scheme, header, nonce, ciphertext, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (tenant_id, alias) DO UPDATE SET
		   host       = EXCLUDED.host,
		   scheme     = EXCLUDED.scheme,
		   header     = EXCLUDED.header,
		   nonce      = EXCLUDED.nonce,
		   ciphertext = EXCLUDED.ciphertext,
		   updated_at = EXCLUDED.updated_at`,
		stored.rec.ID, stored.rec.TenantID, stored.rec.Alias, stored.rec.Host, stored.rec.Scheme,
		stored.rec.Header, stored.nonce, stored.ct, stored.rec.CreatedAt, stored.rec.UpdatedAt)
	if err != nil {
		return fmt.Errorf("secret: put: %w", err)
	}
	return nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, tenant, alias string) (Record, error) {
	rec, _, err := s.load(ctx, tenant, alias, false)
	return rec, err
}

// Reveal implements Store.
func (s *PostgresStore) Reveal(ctx context.Context, tenant, alias string) (Record, string, error) {
	return s.load(ctx, tenant, alias, true)
}

func (s *PostgresStore) load(ctx context.Context, tenant, alias string, reveal bool) (Record, string, error) {
	var (
		rec   Record
		nonce []byte
		ct    []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, alias, host, scheme, header, nonce, ciphertext, created_at, updated_at
		   FROM vora_secrets WHERE tenant_id = $1 AND alias = $2`, tenant, alias).
		Scan(&rec.ID, &rec.TenantID, &rec.Alias, &rec.Host, &rec.Scheme, &rec.Header,
			&nonce, &ct, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, "", ErrNotFound
	}
	if err != nil {
		return Record{}, "", fmt.Errorf("secret: get: %w", err)
	}
	if !reveal {
		return rec, "", nil
	}
	value, err := s.sealer.Open(nonce, ct)
	if err != nil {
		return Record{}, "", err
	}
	return rec, value, nil
}

// List implements Store. Only metadata is selected, so a value can never be
// returned by accident.
func (s *PostgresStore) List(ctx context.Context, tenant string) ([]Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, alias, host, scheme, header, created_at, updated_at
		   FROM vora_secrets WHERE tenant_id = $1 ORDER BY created_at, alias`, tenant)
	if err != nil {
		return nil, fmt.Errorf("secret: list: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.Alias, &rec.Host, &rec.Scheme, &rec.Header,
			&rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Delete implements Store.
func (s *PostgresStore) Delete(ctx context.Context, tenant, alias string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vora_secrets WHERE tenant_id = $1 AND alias = $2`, tenant, alias)
	if err != nil {
		return fmt.Errorf("secret: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Close implements Store.
func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}
