//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vorash/vora/internal/secret"
)

// TestPostgresSecretStore runs the brokered-credential store against a real
// PostgreSQL started with testcontainers; this is the same code path used
// against Supabase when VORA_API_DSN is set.
func TestPostgresSecretStore(t *testing.T) {
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

	sealer, err := secret.NewSealer("integration-master-key-long-enough")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store, err := secret.OpenPostgres(ctx, dsn, sealer)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Re-opening must be safe (schema is idempotent).
	second, err := secret.OpenPostgres(ctx, dsn, sealer)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	second.Close()

	const value = "ghp_postgres_secret"
	rec := secret.Record{TenantID: "t1", Alias: "github", Host: "api.github.com", Scheme: secret.SchemeBearer}
	if err := store.Put(ctx, rec, value); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, revealed, err := store.Reveal(ctx, "t1", "github")
	if err != nil || revealed != value {
		t.Fatalf("reveal = %q, %v", revealed, err)
	}
	if got.ID == "" || got.Host != "api.github.com" {
		t.Fatalf("record = %+v", got)
	}

	// The value must be encrypted at rest, not merely hidden from the API.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("raw pool: %v", err)
	}
	defer pool.Close()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM vora_secrets WHERE tenant_id = 't1' AND alias = 'github'`).Scan(&raw); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if strings.Contains(string(raw), value) {
		t.Fatal("stored ciphertext contains the plaintext")
	}

	// A replacement keeps the identity and is readable again (upsert path).
	if err := store.Put(ctx, rec, "rotated"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	replaced, err := store.Get(ctx, "t1", "github")
	if err != nil || replaced.ID != got.ID || !replaced.CreatedAt.Equal(got.CreatedAt) {
		t.Fatalf("replace changed identity: %+v, %v", replaced, err)
	}
	if _, value, _ := store.Reveal(ctx, "t1", "github"); value != "rotated" {
		t.Fatalf("reveal after replace = %q", value)
	}

	// Tenant isolation: same alias in another tenant is a separate credential.
	if err := store.Put(ctx, secret.Record{TenantID: "t2", Alias: "github", Host: "api.other.com", Scheme: secret.SchemeBearer}, "t2_value"); err != nil {
		t.Fatalf("put t2: %v", err)
	}
	if _, _, err := store.Reveal(ctx, "t1", "missing"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("cross-tenant reveal = %v, want ErrNotFound", err)
	}
	if _, value, err := store.Reveal(ctx, "t2", "github"); err != nil || value != "t2_value" {
		t.Fatalf("t2 reveal = %q, %v", value, err)
	}
	if list, err := store.List(ctx, "t1"); err != nil || len(list) != 1 || list[0].TenantID != "t1" {
		t.Fatalf("list t1 = %+v, %v", list, err)
	}
	if err := store.Delete(ctx, "t2", "github"); err != nil {
		t.Fatalf("delete t2: %v", err)
	}
	if _, value, err := store.Reveal(ctx, "t1", "github"); err != nil || value != "rotated" {
		t.Fatalf("t1 credential disturbed by t2 delete: %q, %v", value, err)
	}

	// A different master key must not be able to read existing rows.
	otherSealer, err := secret.NewSealer("a-different-integration-master-key")
	if err != nil {
		t.Fatalf("other sealer: %v", err)
	}
	other, err := secret.OpenPostgres(ctx, dsn, otherSealer)
	if err != nil {
		t.Fatalf("open with other key: %v", err)
	}
	defer other.Close()
	if _, err := other.Get(ctx, "t1", "github"); err != nil {
		t.Fatalf("metadata read with other key = %v", err)
	}
	if _, _, err := other.Reveal(ctx, "t1", "github"); err == nil {
		t.Fatal("reveal with the wrong master key succeeded")
	}
}
