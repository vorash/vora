//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runner"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestTTLReaperDestroysExpiredSandbox asserts a running sandbox past its TTL is
// expired and destroyed.
func TestTTLReaperDestroysExpiredSandbox(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), SystemdMode: config.SystemdUser, DefaultTTL: time.Minute}
	store := sandbox.NewStore(cfg.SandboxesDir())
	id := ulid.NewSandbox()
	rec := &sandbox.Record{
		ID:    id,
		State: sandbox.StateRunning,
		Spec:  runtime.SandboxSpec{ID: id, TTL: time.Second},
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}
	// Backdate creation so the TTL has elapsed.
	rec.CreatedAt = time.Now().Add(-time.Hour)
	if err := store.Save(rec); err != nil {
		t.Fatalf("save record: %v", err)
	}

	srv := runner.NewServer(cfg, store, runtime.NewRegistry(), ulid.NewRunner(), discardLogger())
	if err := srv.ReapExpired(context.Background()); err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}

	got, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.State != sandbox.StateDestroyed {
		t.Fatalf("state = %s, want DESTROYED", got.State)
	}
}

// TestReconcileCleansOrphans asserts startup reconciliation removes units left
// by a previous runner and repairs orphaned records.
func TestReconcileCleansOrphans(t *testing.T) {
	cgroups, err := cgroup.New(config.Config{SystemdMode: config.SystemdUser})
	if err != nil {
		t.Skipf("systemd user manager unavailable: %v", err)
	}

	cfg := config.Config{StateDir: t.TempDir(), SystemdMode: config.SystemdUser, DefaultTTL: time.Minute}
	ctx := context.Background()

	// A runtime unit from a "previous" runner.
	unitID := ulid.NewSandbox()
	name := cgroup.UnitName(unitID)
	u, err := cgroups.Start(ctx, cgroup.StartRequest{
		Name:   name,
		Limits: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 64 << 20, Pids: 16},
		Argv:   []string{"/usr/bin/sleep", "60"},
	})
	if err != nil {
		t.Fatalf("start orphan unit: %v", err)
	}
	defer func() { _ = u.Stop(ctx); _ = u.Remove(ctx) }()

	// A record left in RUNNING state.
	store := sandbox.NewStore(cfg.SandboxesDir())
	id := ulid.NewSandbox()
	rec := &sandbox.Record{ID: id, State: sandbox.StateRunning, Spec: runtime.SandboxSpec{ID: id}}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create record: %v", err)
	}

	srv := runner.NewServer(cfg, store, runtime.NewRegistry(), ulid.NewRunner(), discardLogger())
	srv.SetCgroups(cgroups)
	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := cgroups.Attach(ctx, name); !errors.Is(err, cgroup.ErrNotFound) {
		t.Fatalf("orphan unit still present: %v", err)
	}
	got, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.State != sandbox.StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State)
	}
}
