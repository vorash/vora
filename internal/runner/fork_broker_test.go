package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/snapshot"
	"github.com/vorash/vora/internal/ulid"
)

func brokeredSpec() runtime.SandboxSpec {
	return runtime.SandboxSpec{
		ID:        ulid.NewSandbox(),
		Image:     "img",
		Backend:   "bwrap",
		TenantID:  "tenant-a",
		Resources: runtime.ResourceLimits{CPUMillis: 100, MemoryBytes: 1 << 20, Pids: 4},
		Network: runtime.NetworkPolicy{
			Mode:           runtime.NetworkRestricted,
			AllowedDomains: []string{"api.example.com"},
			BrokerAliases:  []string{"github"},
		},
	}
}

// The control plane decides which credentials a fork needs by reading the
// aliases off the spec the runner returns. Dropping them in conversion means
// no credentials are ever sent and every brokered fork fails closed.
func TestProtoSpecKeepsBrokerAliases(t *testing.T) {
	spec := brokeredSpec()
	want := []string{"github"}

	sb := toProtoSandbox(&sandbox.Record{ID: spec.ID, Spec: spec})
	if got := sb.GetSpec().GetNetwork().GetBrokerAliases(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("sandbox spec broker_aliases = %v, want %v", got, want)
	}
	snap := toProtoSnapshot(&snapshot.Record{ID: ulid.NewSnapshot(), SandboxID: spec.ID, Spec: spec})
	if got := snap.GetSpec().GetNetwork().GetBrokerAliases(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("snapshot spec broker_aliases = %v, want %v", got, want)
	}
}

// A fork restores the snapshot's files before it starts. If it cannot start,
// the caller never learns the fork's ID (the control plane writes no route), so
// nobody can destroy it: the runner must clean up after itself, or repeated
// failed forks fill the disk and the tenant's capacity.
func TestFailedForkLeavesNothingBehind(t *testing.T) {
	s := testServer(t, testRegistry(t))
	s.cfg.MaxSandboxesPerTenant = 1
	if err := os.MkdirAll(filepath.Join(s.cfg.StateDir, "images", "img", "latest", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	for _, d := range []string{"workspace", "home"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "workspace", "data"), []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapRec := &snapshot.Record{ID: ulid.NewSnapshot(), SandboxID: ulid.NewSandbox(), TenantID: "tenant-a", Spec: brokeredSpec()}
	if err := s.snapshots.Create(snapRec); err != nil {
		t.Fatal(err)
	}
	if _, err := writeArchive(s.snapshots.DataPath(snapRec.ID), snapshot.Folders{
		snapshot.WorkspaceDir: filepath.Join(src, "workspace"),
		snapshot.HomeDir:      filepath.Join(src, "home"),
	}, 1<<20); err != nil {
		t.Fatal(err)
	}

	// No BrokerSecrets: the start fails closed on the granted alias.
	for i := 0; i < 3; i++ {
		if _, err := s.ForkSnapshot(context.Background(), &v1.ForkSnapshotRequest{SnapshotId: snapRec.ID}); err == nil {
			t.Fatal("brokered fork without credentials succeeded")
		}
	}

	recs, err := s.store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.State.Active() {
			t.Errorf("failed fork %s left in state %s", rec.ID, rec.State)
		}
		for _, dir := range []string{rec.Spec.Workspace.WorkspaceDir, rec.Spec.Workspace.HomeDir} {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("failed fork %s left restored state at %s", rec.ID, dir)
			}
		}
	}
	if err := s.checkSandboxCapacity("tenant-a"); err != nil {
		t.Fatalf("failed forks still count against tenant capacity: %v", err)
	}
}
