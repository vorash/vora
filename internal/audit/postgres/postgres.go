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
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion identifies the embedded schema. Bump when schema.sql changes.
const schemaVersion = "0005_exec_usage"

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
		`INSERT INTO sandbox_events (sandbox_id, from_state, to_state, reason, runner_id, occurred_at, tenant_id, agent_identity, key_id, kind, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		event.SandboxID, string(event.From), string(event.To), event.Reason, event.RunnerID, ts,
		event.Actor.TenantID, event.Actor.AgentIdentity, event.Actor.KeyID, event.Kind, event.Detail)
	if err != nil {
		return fmt.Errorf("audit/postgres: append event: %w", err)
	}
	return nil
}

// AppendExec implements audit.Store.
func (s *Store) AppendExec(ctx context.Context, sandboxID string, exec sandbox.ExecRecord) error {
	if !ulid.ValidPrefix(exec.ExecutionID, ulid.PrefixExec) {
		return fmt.Errorf("audit/postgres: invalid execution id %q", exec.ExecutionID)
	}
	scope, err := peakScopeColumn(exec.MemoryPeakScope)
	if err != nil {
		return err
	}
	// Records are append-only: a reused execution_id violates the primary key
	// and fails rather than rewriting the earlier record.
	_, err = s.pool.Exec(ctx,
		`INSERT INTO sandbox_execs
		   (execution_id, sandbox_id, command, args, cwd, started_at, finished_at, exit_code, reason, stdout_bytes, stderr_bytes, tenant_id, agent_identity, key_id,
		    cpu_millis, memory_peak_bytes, memory_peak_scope, oom_kills, pids_limit_hits, egress_allowed, egress_denied, pool_id, sandbox_run)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		exec.ExecutionID, sandboxID, exec.Command, exec.Args, exec.Cwd,
		exec.StartedAt, exec.FinishedAt, exec.ExitCode, string(exec.Reason),
		exec.StdoutBytes, exec.StderrBytes,
		exec.Actor.TenantID, exec.Actor.AgentIdentity, exec.Actor.KeyID,
		exec.CPUMillis, exec.MemoryPeakBytes, scope, exec.OOMKills, exec.PidsLimitHits, exec.EgressAllowed, exec.EgressDenied,
		exec.PoolID, exec.SandboxRun)
	if err != nil {
		return fmt.Errorf("audit/postgres: append exec: %w", err)
	}
	return nil
}

// Events implements audit.Store.
func (s *Store) Events(ctx context.Context, sandboxID string) ([]sandbox.Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT sandbox_id, from_state, to_state, reason, runner_id, occurred_at, tenant_id, agent_identity, key_id, kind, detail
		 FROM sandbox_events WHERE sandbox_id = $1 ORDER BY occurred_at, id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: query events: %w", err)
	}
	defer rows.Close()

	var events []sandbox.Event
	for rows.Next() {
		var e sandbox.Event
		if err := rows.Scan(&e.SandboxID, &e.From, &e.To, &e.Reason, &e.RunnerID, &e.Timestamp,
			&e.Actor.TenantID, &e.Actor.AgentIdentity, &e.Actor.KeyID, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Execs implements audit.Store.
func (s *Store) Execs(ctx context.Context, sandboxID string) ([]sandbox.ExecRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT execution_id, command, args, cwd, started_at, finished_at, exit_code, reason, stdout_bytes, stderr_bytes, tenant_id, agent_identity, key_id,
		        cpu_millis, memory_peak_bytes, memory_peak_scope, oom_kills, pids_limit_hits, egress_allowed, egress_denied, pool_id, sandbox_run
		 FROM sandbox_execs WHERE sandbox_id = $1 ORDER BY started_at`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: query execs: %w", err)
	}
	defer rows.Close()

	var execs []sandbox.ExecRecord
	for rows.Next() {
		var r sandbox.ExecRecord
		r.SandboxID = sandboxID
		var scope *string
		if err := rows.Scan(&r.ExecutionID, &r.Command, &r.Args, &r.Cwd, &r.StartedAt, &r.FinishedAt,
			&r.ExitCode, &r.Reason, &r.StdoutBytes, &r.StderrBytes,
			&r.Actor.TenantID, &r.Actor.AgentIdentity, &r.Actor.KeyID,
			&r.CPUMillis, &r.MemoryPeakBytes, &scope, &r.OOMKills, &r.PidsLimitHits, &r.EgressAllowed, &r.EgressDenied,
			&r.PoolID, &r.SandboxRun); err != nil {
			return nil, err
		}
		if r.MemoryPeakScope, err = parsePeakScope(scope); err != nil {
			return nil, err
		}
		execs = append(execs, r)
	}
	return execs, rows.Err()
}

// peakScopeColumn names a scope for the column, which a person reading the table
// has to understand without the Go enum. Unspecified is NULL. A scope with no name
// fails the write: the alternative is a record that silently lost what it measured.
func peakScopeColumn(scope runtime.PeakScope) (*string, error) {
	var name string
	switch scope {
	case runtime.PeakUnspecified:
		return nil, nil
	case runtime.PeakWindow:
		name = "window"
	case runtime.PeakLifetime:
		name = "lifetime"
	default:
		return nil, fmt.Errorf("audit/postgres: unknown peak scope %d", scope)
	}
	return &name, nil
}

func parsePeakScope(column *string) (runtime.PeakScope, error) {
	if column == nil {
		return runtime.PeakUnspecified, nil
	}
	switch *column {
	case "window":
		return runtime.PeakWindow, nil
	case "lifetime":
		return runtime.PeakLifetime, nil
	default:
		return runtime.PeakUnspecified, fmt.Errorf("audit/postgres: unknown peak scope %q", *column)
	}
}

// AppendEgress implements audit.Store.
func (s *Store) AppendEgress(ctx context.Context, sandboxID string, record sandbox.EgressRecord) error {
	ts := record.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sandbox_egress (sandbox_id, host, remote, alias, allowed, reason, occurred_at, tenant_id, agent_identity, key_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		sandboxID, record.Host, record.Remote, record.Alias, record.Allowed, record.Reason, ts,
		record.Actor.TenantID, record.Actor.AgentIdentity, record.Actor.KeyID)
	if err != nil {
		return fmt.Errorf("audit/postgres: append egress: %w", err)
	}
	return nil
}

// Egress implements audit.Store.
func (s *Store) Egress(ctx context.Context, sandboxID string) ([]sandbox.EgressRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT sandbox_id, host, remote, alias, allowed, reason, occurred_at, tenant_id, agent_identity, key_id
		 FROM sandbox_egress WHERE sandbox_id = $1 ORDER BY occurred_at, id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("audit/postgres: query egress: %w", err)
	}
	defer rows.Close()
	var records []sandbox.EgressRecord
	for rows.Next() {
		var record sandbox.EgressRecord
		if err := rows.Scan(&record.SandboxID, &record.Host, &record.Remote, &record.Alias, &record.Allowed, &record.Reason, &record.Timestamp,
			&record.Actor.TenantID, &record.Actor.AgentIdentity, &record.Actor.KeyID); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// EgressCounts implements audit.Store.
func (s *Store) EgressCounts(ctx context.Context, sandboxID string, start, end time.Time) (allowed, denied int64, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE allowed), count(*) FILTER (WHERE NOT allowed)
		 FROM sandbox_egress WHERE sandbox_id = $1 AND occurred_at BETWEEN $2 AND $3`,
		sandboxID, start, end).Scan(&allowed, &denied)
	if err != nil {
		return 0, 0, fmt.Errorf("audit/postgres: count egress: %w", err)
	}
	return allowed, denied, nil
}

// Close implements audit.Store.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}
