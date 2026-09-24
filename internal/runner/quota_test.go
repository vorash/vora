package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

func quotaSandbox(t *testing.T, s *Server, id string, limit int64, fill int) {
	t.Helper()
	root := t.TempDir()
	ws := runtime.WorkspaceSpec{
		RootDir:      root,
		WorkspaceDir: filepath.Join(root, "workspace"),
		HomeDir:      filepath.Join(root, "home"),
	}
	for _, d := range []string{ws.WorkspaceDir, ws.HomeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if fill > 0 {
		if err := os.WriteFile(filepath.Join(ws.WorkspaceDir, "data"), make([]byte, fill), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := &sandbox.Record{
		ID:    id,
		State: sandbox.StateRunning,
		Spec: runtime.SandboxSpec{
			ID:        id,
			Resources: runtime.ResourceLimits{DiskBytes: limit},
			Workspace: ws,
		},
	}
	if err := s.store.Create(rec); err != nil {
		t.Fatal(err)
	}
}

func TestEnforceDiskQuotaStopsOverQuotaSandbox(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	quotaSandbox(t, s, id, 1024, 4096)

	if err := s.EnforceDiskQuota(context.Background()); err != nil {
		t.Fatalf("EnforceDiskQuota: %v", err)
	}
	got, err := s.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != sandbox.StateDestroyed {
		t.Fatalf("state = %s, want DESTROYED", got.State)
	}
	if got.Reason != runtime.ReasonDiskQuota {
		t.Fatalf("reason = %s, want %s", got.Reason, runtime.ReasonDiskQuota)
	}
}

func TestEnforceDiskQuotaLeavesUnderQuotaSandbox(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	quotaSandbox(t, s, id, 1<<20, 4096)

	if err := s.EnforceDiskQuota(context.Background()); err != nil {
		t.Fatalf("EnforceDiskQuota: %v", err)
	}
	got, err := s.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != sandbox.StateRunning {
		t.Fatalf("state = %s, want RUNNING", got.State)
	}
}

func TestEnforceDiskQuotaIgnoresUnlimited(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := ulid.NewSandbox()
	quotaSandbox(t, s, id, 0, 1<<20)

	if err := s.EnforceDiskQuota(context.Background()); err != nil {
		t.Fatalf("EnforceDiskQuota: %v", err)
	}
	got, err := s.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != sandbox.StateRunning {
		t.Fatalf("state = %s, want RUNNING", got.State)
	}
}
