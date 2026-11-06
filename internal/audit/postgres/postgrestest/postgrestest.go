// Package postgrestest holds the audit store's tests that need a real
// PostgreSQL. It is imported by the unprivileged test next to the store and by
// the testcontainers test CI runs, so one suite answers for both.
package postgrestest

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vorash/vora/internal/audit/audittest"
	"github.com/vorash/vora/internal/audit/postgres"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// schema0004 is schema.sql as v0.10.7 shipped it, the schema a deployed runner's
// database is on, and the statements below are that release's own.
//
//go:embed testdata/schema_0004_rls.sql
var schema0004 string

const (
	version0004 = "0004_rls"

	insertEvent0004 = `INSERT INTO sandbox_events (sandbox_id, from_state, to_state, reason, runner_id, occurred_at, tenant_id, agent_identity, key_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	insertExec0004 = `INSERT INTO sandbox_execs
		   (execution_id, sandbox_id, command, args, cwd, started_at, finished_at, exit_code, reason, stdout_bytes, stderr_bytes, tenant_id, agent_identity, key_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`
	insertEgress0004 = `INSERT INTO sandbox_egress (sandbox_id, host, remote, alias, allowed, reason, occurred_at, tenant_id, agent_identity, key_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	selectExecs0004 = `SELECT execution_id, command, args, cwd, started_at, finished_at, exit_code, reason, stdout_bytes, stderr_bytes, tenant_id, agent_identity, key_id
		 FROM sandbox_execs WHERE sandbox_id = $1 ORDER BY started_at`
)

// Run runs the suite. It only ever works in databases it creates, with random
// names, and drops them: baseDSN's own database is used to issue CREATE DATABASE.
func Run(t *testing.T, baseDSN string) {
	t.Run("contract on a fresh database", func(t *testing.T) {
		audittest.Run(t, migrated(t, freshDB(t, baseDSN)))
	})
	t.Run("upgrade from 0004", func(t *testing.T) { testUpgrade(t, baseDSN) })
	t.Run("upgrade matches a fresh install", func(t *testing.T) { testUpgradeMatchesFresh(t, baseDSN) })
	t.Run("two runners migrate at once", func(t *testing.T) { testConcurrentMigrate(t, baseDSN) })
	t.Run("migrate converges from a partial schema", func(t *testing.T) { testPartialSchema(t, baseDSN) })
	t.Run("a failed migration leaves nothing behind", func(t *testing.T) { testFailedMigrationRollsBack(t, baseDSN) })
	t.Run("a 0004 runner keeps working during the deploy", func(t *testing.T) { testOldRunnerAfterMigrate(t, baseDSN) })
	t.Run("new columns stay out of reach of supabase roles", func(t *testing.T) { testNoWiderRead(t, baseDSN) })
}

// freshDB creates an empty database and returns its DSN.
func freshDB(t *testing.T, baseDSN string) string {
	t.Helper()
	ctx := context.Background()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "vora_audit_test_" + hex.EncodeToString(suffix)
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin := func(sql string) error {
		c, err := pgx.Connect(ctx, baseDSN)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(ctx) }()
		_, err = c.Exec(ctx, sql)
		return err
	}
	if err := admin("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	u.Path = "/" + name
	return u.String()
}

func open(t *testing.T, dsn string) *postgres.Store {
	t.Helper()
	store, err := postgres.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func migrated(t *testing.T, dsn string) *postgres.Store {
	t.Helper()
	store := open(t, dsn)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func raw(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("raw pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// at0004 returns a database as v0.10.7's Migrate leaves it.
func at0004(t *testing.T, baseDSN string) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := freshDB(t, baseDSN)
	db := raw(t, dsn)
	ctx := context.Background()
	if _, err := db.Exec(ctx, schema0004); err != nil {
		t.Fatalf("apply 0004: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version0004); err != nil {
		t.Fatalf("record 0004: %v", err)
	}
	return dsn, db
}

var oldActor = sandbox.Actor{TenantID: "tenant-a", AgentIdentity: "spiffe://vora.sh/tenant/tenant-a/agent/analyst", KeyID: "key_1"}

func insertOldExec(t *testing.T, db *pgxpool.Pool, r sandbox.ExecRecord) {
	t.Helper()
	if _, err := db.Exec(context.Background(), insertExec0004,
		r.ExecutionID, r.SandboxID, r.Command, r.Args, r.Cwd, r.StartedAt, r.FinishedAt, r.ExitCode, string(r.Reason),
		r.StdoutBytes, r.StderrBytes, r.Actor.TenantID, r.Actor.AgentIdentity, r.Actor.KeyID); err != nil {
		t.Fatalf("insert old exec: %v", err)
	}
}

func insertOldEvent(t *testing.T, db *pgxpool.Pool, e sandbox.Event) {
	t.Helper()
	if _, err := db.Exec(context.Background(), insertEvent0004,
		e.SandboxID, string(e.From), string(e.To), e.Reason, e.RunnerID, e.Timestamp,
		e.Actor.TenantID, e.Actor.AgentIdentity, e.Actor.KeyID); err != nil {
		t.Fatalf("insert old event: %v", err)
	}
}

func testUpgrade(t *testing.T, baseDSN string) {
	ctx := context.Background()
	dsn, db := at0004(t, baseDSN)

	id := ulid.NewSandbox()
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	oldExecs := []sandbox.ExecRecord{
		{
			ExecutionID: ulid.NewExec(), SandboxID: id, Actor: oldActor, Command: "/bin/echo", Args: []string{"hi", "there"}, Cwd: "/workspace",
			StartedAt: start, FinishedAt: start.Add(time.Second), ExitCode: 3, Reason: runtime.ReasonExited, StdoutBytes: 11, StderrBytes: 13,
		},
		// A command with no arguments is a NULL array in the old rows.
		{
			ExecutionID: ulid.NewExec(), SandboxID: id, Actor: oldActor, Command: "/bin/true",
			StartedAt: start.Add(time.Minute), FinishedAt: start.Add(2 * time.Minute), Reason: runtime.ReasonTimeout,
		},
	}
	for _, r := range oldExecs {
		insertOldExec(t, db, r)
	}
	oldEvents := []sandbox.Event{
		{SandboxID: id, Actor: oldActor, From: sandbox.StateRequested, To: sandbox.StateCreating, RunnerID: "run_a", Timestamp: start},
		// What a pool event looks like with nowhere to put its kind and detail.
		{SandboxID: id, Actor: oldActor, From: sandbox.StateRunning, To: sandbox.StateRunning, Reason: "reset: ok: killed 1, removed 2 entries, in 3ms", RunnerID: "run_a", Timestamp: start.Add(time.Second)},
	}
	for _, e := range oldEvents {
		insertOldEvent(t, db, e)
	}
	oldEgress := sandbox.EgressRecord{SandboxID: id, Actor: oldActor, Host: "example.com", Remote: "192.0.2.1:443", Alias: "github", Allowed: true, Reason: "dns_resolved", Timestamp: start}
	if _, err := db.Exec(ctx, insertEgress0004, oldEgress.SandboxID, oldEgress.Host, oldEgress.Remote, oldEgress.Alias, oldEgress.Allowed, oldEgress.Reason, oldEgress.Timestamp,
		oldEgress.Actor.TenantID, oldEgress.Actor.AgentIdentity, oldEgress.Actor.KeyID); err != nil {
		t.Fatalf("insert old egress: %v", err)
	}

	store := open(t, dsn)
	for i := range 3 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", i+1, err)
		}
	}

	var execRows, eventRows, egressRows int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM sandbox_execs), (SELECT count(*) FROM sandbox_events), (SELECT count(*) FROM sandbox_egress)`).
		Scan(&execRows, &eventRows, &egressRows); err != nil {
		t.Fatal(err)
	}
	if execRows != len(oldExecs) || eventRows != len(oldEvents) || egressRows != 1 {
		t.Fatalf("rows after upgrade: %d execs, %d events, %d egress; want %d, %d, 1", execRows, eventRows, egressRows, len(oldExecs), len(oldEvents))
	}

	// Dashboards and the bench read these columns directly: an old row must say
	// NULL there too, or a backfilled zero reads as a real measurement.
	var measured int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM sandbox_execs WHERE num_nonnulls(
		cpu_millis, memory_peak_bytes, memory_peak_scope, oom_kills, pids_limit_hits, egress_allowed, egress_denied) > 0`).Scan(&measured); err != nil {
		t.Fatal(err)
	}
	if measured != 0 {
		t.Fatalf("%d old rows claim a measurement nobody took", measured)
	}

	gotExecs, err := store.Execs(ctx, id)
	if err != nil {
		t.Fatalf("Execs after upgrade: %v", err)
	}
	if len(gotExecs) != len(oldExecs) {
		t.Fatalf("Execs after upgrade = %d, want %d", len(gotExecs), len(oldExecs))
	}
	for i := range oldExecs {
		for _, d := range audittest.Diff(oldExecs[i], gotExecs[i]) {
			t.Errorf("old exec %d: %s", i, d)
		}
	}
	gotEvents, err := store.Events(ctx, id)
	if err != nil {
		t.Fatalf("Events after upgrade: %v", err)
	}
	if len(gotEvents) != len(oldEvents) {
		t.Fatalf("Events after upgrade = %d, want %d", len(gotEvents), len(oldEvents))
	}
	for i := range oldEvents {
		for _, d := range audittest.Diff(oldEvents[i], gotEvents[i]) {
			t.Errorf("old event %d: %s", i, d)
		}
	}
	gotEgress, err := store.Egress(ctx, id)
	if err != nil || len(gotEgress) != 1 {
		t.Fatalf("Egress after upgrade = %v, %v", gotEgress, err)
	}
	for _, d := range audittest.Diff(oldEgress, gotEgress[0]) {
		t.Errorf("old egress: %s", d)
	}

	rows, err := db.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0] != version0004 {
		t.Fatalf("schema_migrations = %v, want %s and the new version", versions, version0004)
	}

	// The upgraded database has rows in it and still meets the contract.
	audittest.Run(t, store)
}

// describe returns what makes two schemas the same to a query.
func describe(t *testing.T, db *pgxpool.Pool) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
		  FROM information_schema.columns WHERE table_schema = 'public'
		UNION ALL
		SELECT indexdef FROM pg_indexes WHERE schemaname = 'public'
		UNION ALL
		SELECT relname || ' rls=' || relrowsecurity::text FROM pg_class
		 WHERE relkind = 'r' AND relnamespace = 'public'::regnamespace
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func testUpgradeMatchesFresh(t *testing.T, baseDSN string) {
	freshDSN := freshDB(t, baseDSN)
	migrated(t, freshDSN)
	upgradedDSN, upgraded := at0004(t, baseDSN)
	migrated(t, upgradedDSN)
	fresh, up := describe(t, raw(t, freshDSN)), describe(t, upgraded)
	if !reflect.DeepEqual(fresh, up) {
		t.Fatalf("a fresh install and an upgrade differ:\nfresh:\n%s\nupgraded:\n%s", strings.Join(fresh, "\n"), strings.Join(up, "\n"))
	}
}

// vora-mini points both its runners at one database, and a deploy restarts them
// together, so this is the normal upgrade rather than an edge case.
func testConcurrentMigrate(t *testing.T, baseDSN string) {
	const rounds, runners = 5, 4
	for round := range rounds {
		dsn, _ := at0004(t, baseDSN)
		stores := make([]*postgres.Store, runners)
		for i := range stores {
			stores[i] = open(t, dsn)
		}
		errs := make([]error, runners)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, store := range stores {
			wg.Go(func() {
				<-start
				errs[i] = store.Migrate(context.Background())
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d runner %d: %v", round, i, err)
			}
		}
	}
}

func testPartialSchema(t *testing.T, baseDSN string) {
	ctx := context.Background()
	dsn, db := at0004(t, baseDSN)
	for _, sql := range []string{
		`ALTER TABLE sandbox_execs ADD COLUMN cpu_millis BIGINT`,
		`ALTER TABLE sandbox_events ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	audittest.Run(t, migrated(t, dsn))
}

// A runner killed mid-migration must leave a state the next boot can finish from.
// The script is one transaction, so a statement that fails rolls back the rest.
func testFailedMigrationRollsBack(t *testing.T, baseDSN string) {
	ctx := context.Background()
	dsn := freshDB(t, baseDSN)
	db := raw(t, dsn)
	// CREATE TABLE IF NOT EXISTS skips a view of that name, and ALTER TABLE on it fails.
	if _, err := db.Exec(ctx, `CREATE VIEW sandbox_egress AS SELECT 1 AS x`); err != nil {
		t.Fatal(err)
	}
	if err := open(t, dsn).Migrate(ctx); err == nil {
		t.Fatal("migrate succeeded over a view")
	}
	var created *string
	if err := db.QueryRow(ctx, `SELECT to_regclass('sandbox_execs')::text`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != nil {
		t.Fatal("a failed migration left sandbox_execs behind")
	}

	if _, err := db.Exec(ctx, `DROP VIEW sandbox_egress`); err != nil {
		t.Fatal(err)
	}
	audittest.Run(t, migrated(t, dsn))
}

func testOldRunnerAfterMigrate(t *testing.T, baseDSN string) {
	ctx := context.Background()
	dsn, db := at0004(t, baseDSN)
	store := migrated(t, dsn)

	id := ulid.NewSandbox()
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	fromOld := sandbox.ExecRecord{
		ExecutionID: ulid.NewExec(), SandboxID: id, Actor: oldActor, Command: "/bin/true",
		StartedAt: start, FinishedAt: start, Reason: runtime.ReasonExited,
	}
	insertOldExec(t, db, fromOld)
	insertOldEvent(t, db, sandbox.Event{SandboxID: id, Actor: oldActor, From: sandbox.StateRequested, To: sandbox.StateCreating, Timestamp: start})

	fromNew := audittest.Filled[sandbox.ExecRecord](t)
	fromNew.SandboxID, fromNew.ExecutionID = id, ulid.NewExec()
	fromNew.StartedAt = start.Add(time.Hour)
	if err := store.AppendExec(ctx, id, fromNew); err != nil {
		t.Fatalf("AppendExec: %v", err)
	}

	rows, err := db.Query(ctx, selectExecs0004, id)
	if err != nil {
		t.Fatalf("the old runner's select: %v", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("the old runner's select: %v", err)
	}

	got, err := store.Execs(ctx, id)
	if err != nil {
		t.Fatalf("Execs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Execs = %d, want 2", len(got))
	}
	for _, d := range audittest.Diff(fromOld, got[0]) {
		t.Errorf("row from the old runner: %s", d)
	}
	for _, d := range audittest.Diff(fromNew, got[1]) {
		t.Errorf("row from the new runner: %s", d)
	}
}

// Member events carry raw backend text in detail, as reason already did. On
// Supabase the public schema is world-readable unless revoked, so a role that
// held a grant on the old table must not be able to read the new columns.
func testNoWiderRead(t *testing.T, baseDSN string) {
	ctx := context.Background()
	dsn, db := at0004(t, baseDSN)
	for _, role := range []string{"anon", "authenticated"} {
		if _, err := db.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN CREATE ROLE %[1]s; END IF;
		END $$`, role)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, fmt.Sprintf(`GRANT SELECT ON sandbox_events, sandbox_execs TO %s`, role)); err != nil {
			t.Fatal(err)
		}
	}
	migrated(t, dsn)
	for _, column := range []struct{ table, name string }{
		{"sandbox_events", "detail"}, {"sandbox_events", "kind"},
		{"sandbox_execs", "cpu_millis"}, {"sandbox_execs", "pool_id"},
	} {
		for _, role := range []string{"anon", "authenticated"} {
			var readable bool
			if err := db.QueryRow(ctx, `SELECT has_column_privilege($1, $2, $3, 'SELECT')`, role, column.table, column.name).Scan(&readable); err != nil {
				t.Fatal(err)
			}
			if readable {
				t.Errorf("%s can read %s.%s", role, column.table, column.name)
			}
		}
	}
	var unprotected int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_class
		WHERE relname IN ('sandbox_events', 'sandbox_execs') AND relnamespace = 'public'::regnamespace AND NOT relrowsecurity`).Scan(&unprotected); err != nil {
		t.Fatal(err)
	}
	if unprotected != 0 {
		t.Errorf("%d audit tables have row-level security off", unprotected)
	}
}
