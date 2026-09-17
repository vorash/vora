// Package postgres implements audit.Store on PostgreSQL, including hosted
// Supabase. The DSN is supplied by the caller (VORA_AUDIT_DSN); SSL and pooling
// are properties of the DSN.
package postgres

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/sandbox"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion identifies the embedded schema. Bump when schema.sql changes.
const schemaVersion = "0001_sandbox_audit"

// Store is a PostgreSQL-backed audit store.
type Store struct {
	pool *pgxpool.Pool
}

var _ audit.Store = (*Store)(nil)

// Open connects to PostgreSQL using dsn and returns a store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("audit/postgres: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Migrate applies the embedded schema. It is idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("audit/postgres: migrate: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`,
		schemaVersion); err != nil {
		return fmt.Errorf("audit/postgres: record migration: %w", err)
	}
	return nil
}

// AppendEvent implements audit.Store.
func (s *Store) AppendEvent(ctx context.Context, _ string, event sandbox.Event) error {
	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sandbox_events (sandbox_id, from_state, to_state, reason, runner_id, occurred_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		event.SandboxID, string(event.From), string(event.To), event.Reason, event.RunnerID, ts)
	if err != nil {
		return fmt.Errorf("audit/postgres: append event: %w", err)
	}
	return nil
}

// AppendExec implements audit.Store.
func (s *Store) AppendExec(ctx context.Context, sandboxID string, exec sandbox.ExecRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sandbox_execs
		   (execution_id, sandbox_id, command, args, cwd, started_at, finished_at, exit_code, reason, stdout_bytes, stderr_bytes)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (execution_id) DO UPDATE SET
		   finished_at  = EXCLUDED.finished_at,
		   exit_code    = EXCLUDED.exit_code,
		   reason       = EXCLUDED.reason,
		   stdout_bytes = EXCLUDED.stdout_bytes,
		   stderr_bytes = EXCLUDED.stderr_bytes`,
		exec.ExecutionID, sandboxID, exec.Command, exec.Args, exec.Cwd,
		exec.StartedAt, exec.FinishedAt, exec.ExitCode, string(exec.Reason),
		exec.StdoutBytes, exec.StderrBytes)
	if err != nil {
		return fmt.Errorf("audit/postgres: append exec: %w", err)
	}
	return nil
}

// Events implements audit.Store.
func (s *Store) Events(ctx context.Context, sandboxID string) ([]sandbox.Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT sandbox_id, from_state, to_state, reason, runner_id, occurred_at
		 FROM sandbox_events WHERE sandbox_id = $1 ORDER BY occurred_at, id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: query events: %w", err)
	}
	defer rows.Close()

	var events []sandbox.Event
	for rows.Next() {
		var e sandbox.Event
		if err := rows.Scan(&e.SandboxID, &e.From, &e.To, &e.Reason, &e.RunnerID, &e.Timestamp); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Execs implements audit.Store.
func (s *Store) Execs(ctx context.Context, sandboxID string) ([]sandbox.ExecRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT execution_id, command, args, cwd, started_at, finished_at, exit_code, reason, stdout_bytes, stderr_bytes
		 FROM sandbox_execs WHERE sandbox_id = $1 ORDER BY started_at`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: query execs: %w", err)
	}
	defer rows.Close()

	var execs []sandbox.ExecRecord
	for rows.Next() {
		var r sandbox.ExecRecord
		r.SandboxID = sandboxID
		if err := rows.Scan(&r.ExecutionID, &r.Command, &r.Args, &r.Cwd, &r.StartedAt, &r.FinishedAt,
			&r.ExitCode, &r.Reason, &r.StdoutBytes, &r.StderrBytes); err != nil {
			return nil, err
		}
		execs = append(execs, r)
	}
	return execs, rows.Err()
}

// Close implements audit.Store.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}
