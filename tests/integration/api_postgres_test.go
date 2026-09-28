//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vorash/vora/internal/api"
)

// TestPostgresAPIStore runs the control-plane store against a real PostgreSQL
// started with testcontainers; this is the same code path used against Supabase
// when VORA_API_DSN is set.
func TestPostgresAPIStore(t *testing.T) {
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

	store, err := api.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Re-opening must be safe (schema is idempotent).
	second, err := api.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	second.Close()

	// --- API keys ---
	plaintext, hash, err := api.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	key := api.APIKey{
		ID: "key_pg_test", Name: "ci", TenantID: "tenant_pg",
		KeyHash: hash, Scopes: []string{api.ScopeAdmin, api.ScopeSandbox}, CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateKey(ctx, key); err != nil {
		t.Fatalf("create key: %v", err)
	}
	got, err := store.KeyByHash(ctx, api.HashKey(plaintext))
	if err != nil || got.ID != key.ID || !got.HasScope(api.ScopeAdmin) {
		t.Fatalf("KeyByHash = %+v, %v", got, err)
	}
	if byID, err := store.Key(ctx, key.ID); err != nil || byID.ID != key.ID || byID.KeyHash != "" {
		t.Fatalf("Key = %+v, %v", byID, err)
	}
	if err := store.TouchKey(ctx, key.ID, time.Now().UTC()); err != nil {
		t.Fatalf("touch: %v", err)
	}
	keys, err := store.ListKeys(ctx, "tenant_pg")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list keys = %d, %v", len(keys), err)
	}
	if err := store.RevokeKey(ctx, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, _ = store.KeyByHash(ctx, hash)
	if !got.Revoked {
		t.Fatal("expected the key to be revoked")
	}
	if err := store.RevokeKey(ctx, "key_missing"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoke missing key = %v, want ErrNotFound", err)
	}

	// --- Runners ---
	now := time.Now().UTC()
	runner := api.Runner{
		ID: "run_pg_test", Address: "10.0.0.5:9200", Backends: []string{"bwrap", "gvisor"},
		CPUMillis: 4000, MemoryBytes: 8 << 30, MaxSandboxes: 20, Version: "test",
		RegisteredAt: now, LastHeartbeat: now,
	}
	if err := store.UpsertRunner(ctx, runner); err != nil {
		t.Fatalf("upsert runner: %v", err)
	}
	runner.Running = 3
	if err := store.UpsertRunner(ctx, runner); err != nil {
		t.Fatalf("upsert runner (2): %v", err)
	}
	gotRunner, err := store.Runner(ctx, "run_pg_test")
	if err != nil || gotRunner.Running != 3 || len(gotRunner.Backends) != 2 {
		t.Fatalf("runner = %+v, %v", gotRunner, err)
	}
	runners, err := store.ListRunners(ctx)
	if err != nil || len(runners) != 1 {
		t.Fatalf("list runners = %d, %v", len(runners), err)
	}
	if _, err := store.Runner(ctx, "run_missing"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("missing runner = %v, want ErrNotFound", err)
	}

	// --- Sandbox routes ---
	route := api.SandboxRoute{SandboxID: "sbx_pg_test", RunnerID: "run_pg_test", TenantID: "tenant_pg"}
	if err := store.SetRoute(ctx, route); err != nil {
		t.Fatalf("set route: %v", err)
	}
	if got, err := store.Route(ctx, "sbx_pg_test"); err != nil || got != route {
		t.Fatalf("route = %+v, %v", got, err)
	}
	if err := store.SetRoute(ctx, route); err != nil {
		t.Fatalf("set route (idempotent): %v", err)
	}
	// A destroyed sandbox keeps its route as a tombstone so its audit trail
	// stays reachable; the flag must survive the upsert and both read paths,
	// or placement would keep charging the runner for it.
	tombstone := route
	tombstone.Destroyed = true
	if err := store.SetRoute(ctx, tombstone); err != nil {
		t.Fatalf("mark route destroyed: %v", err)
	}
	if got, err := store.Route(ctx, "sbx_pg_test"); err != nil || !got.Destroyed {
		t.Fatalf("destroyed route = %+v, %v", got, err)
	}
	listed, err := store.ListRoutes(ctx)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	for _, r := range listed {
		if r.SandboxID == "sbx_pg_test" && !r.Destroyed {
			t.Fatalf("listed route lost the destroyed flag: %+v", r)
		}
	}
	legacy := api.SandboxRoute{SandboxID: "sbx_pg_legacy", RunnerID: "run_pg_test"}
	if err := store.SetRoute(ctx, legacy); err != nil {
		t.Fatalf("set legacy route: %v", err)
	}
	if got, err := store.Route(ctx, legacy.SandboxID); err != nil || got.TenantID != "" {
		t.Fatalf("legacy route = %+v, %v", got, err)
	}
	if err := store.DeleteRoute(ctx, "sbx_pg_test"); err != nil {
		t.Fatalf("delete route: %v", err)
	}
	if _, err := store.Route(ctx, "sbx_pg_test"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("route after delete = %v, want ErrNotFound", err)
	}
}
