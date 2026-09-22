//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vorash/vora/internal/audit/postgres"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

// TestPostgresAuditStore runs the audit store against a real PostgreSQL started
// with testcontainers; this is the same code path used against Supabase.
func TestPostgresAuditStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("vora"),
		tcpostgres.WithUsername("vora"),
		tcpostgres.WithPassword("vora"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Skipf("testcontainers unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Migrate must be idempotent.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	const sandboxID = "sbx_pg_test"
	now := time.Now().UTC().Truncate(time.Millisecond)
	actor := sandbox.Actor{
		TenantID:      "tenant-a",
		AgentIdentity: "spiffe://vora.sh/tenant/tenant-a/agent/analyst",
		KeyID:         "key_1",
	}

	if err := store.AppendEvent(ctx, sandboxID, sandbox.Event{
		SandboxID: sandboxID, Actor: actor, From: sandbox.StateRequested, To: sandbox.StateCreating,
		RunnerID: "run_test", Timestamp: now,
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	rec := sandbox.ExecRecord{
		ExecutionID: "exe_pg_1", SandboxID: sandboxID, Actor: actor, Command: "/bin/echo",
		Args: []string{"hi"}, Cwd: "/workspace", StartedAt: now, FinishedAt: now.Add(time.Second),
		ExitCode: 0, Reason: runtime.ReasonExited, StdoutBytes: 3, StderrBytes: 0,
	}
	if err := store.AppendExec(ctx, sandboxID, rec); err != nil {
		t.Fatalf("append exec: %v", err)
	}
	// Re-appending the same execution id updates rather than duplicates.
	rec.ExitCode = 7
	if err := store.AppendExec(ctx, sandboxID, rec); err != nil {
		t.Fatalf("re-append exec: %v", err)
	}

	events, err := store.Events(ctx, sandboxID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 || events[0].From != sandbox.StateRequested || events[0].To != sandbox.StateCreating {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Actor != actor {
		t.Fatalf("event actor = %+v, want %+v", events[0].Actor, actor)
	}

	execs, err := store.Execs(ctx, sandboxID)
	if err != nil {
		t.Fatalf("execs: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("execs = %d, want 1", len(execs))
	}
	if execs[0].ExitCode != 7 || execs[0].Command != "/bin/echo" || len(execs[0].Args) != 1 || execs[0].Args[0] != "hi" {
		t.Fatalf("exec record = %+v", execs[0])
	}
	if execs[0].Actor != actor {
		t.Fatalf("exec actor = %+v, want %+v", execs[0].Actor, actor)
	}

	allowed := sandbox.EgressRecord{
		SandboxID: sandboxID, Actor: actor, Host: "example.com", Remote: "93.184.216.34:443",
		Alias: "github", Allowed: true, Reason: "dns_resolved", Timestamp: now,
	}
	denied := sandbox.EgressRecord{
		SandboxID: sandboxID, Host: "evil.test", Allowed: false,
		Reason: "domain_not_allowed", Timestamp: now.Add(time.Millisecond),
	}
	if err := store.AppendEgress(ctx, sandboxID, allowed); err != nil {
		t.Fatalf("append allowed egress: %v", err)
	}
	if err := store.AppendEgress(ctx, sandboxID, denied); err != nil {
		t.Fatalf("append denied egress: %v", err)
	}
	egress, err := store.Egress(ctx, sandboxID)
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	if len(egress) != 2 || !egress[0].Allowed || egress[1].Reason != "domain_not_allowed" {
		t.Fatalf("egress = %+v", egress)
	}
	// Credential-broker decisions carry the alias, never a secret value: the
	// schema has no column a value could be stored in.
	if egress[0].Alias != "github" {
		t.Fatalf("brokered egress record lost its alias: %+v", egress[0])
	}
	if egress[0].Actor != actor {
		t.Fatalf("egress actor = %+v, want %+v", egress[0].Actor, actor)
	}
	var columns int
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("raw pool: %v", err)
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'sandbox_egress' AND column_name IN ('value','secret','credential')`).Scan(&columns); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if columns != 0 {
		t.Fatal("sandbox_egress has a column that could store a credential value")
	}

	// A different sandbox's records are isolated.
	other, err := store.Execs(ctx, "sbx_other")
	if err != nil {
		t.Fatalf("other execs: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("unexpected records for other sandbox: %+v", other)
	}
}
