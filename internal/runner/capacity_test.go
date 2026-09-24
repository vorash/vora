package runner

import (
	"testing"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/snapshot"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

func TestSandboxCapacityEnforcesTenantAndRunnerLimits(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	s.cfg.MaxSandboxes = 2
	s.cfg.MaxSandboxesPerTenant = 1
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if code := status.Code(s.checkSandboxCapacity(tenant)); code != codes.OK {
			t.Fatalf("empty runner rejected %s: %v", tenant, code)
		}
		id := ulid.NewSandbox()
		if err := s.store.Create(&sandbox.Record{ID: id, State: sandbox.StateRequested, Spec: runtime.SandboxSpec{ID: id, TenantID: tenant}}); err != nil {
			t.Fatal(err)
		}
		if code := status.Code(s.checkSandboxCapacity(tenant)); code != codes.ResourceExhausted {
			t.Fatalf("tenant %s exceeded cap: %v", tenant, code)
		}
	}
	if code := status.Code(s.checkSandboxCapacity("tenant-c")); code != codes.ResourceExhausted {
		t.Fatalf("runner exceeded global cap: %v", code)
	}
}

func TestSnapshotCountIsLimitedPerTenantBeforeArchiving(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	s.cfg.MaxSnapshotsPerTenant = 1
	id := ulid.NewSandbox()
	quotaSandbox(t, s, id, 1<<20, 0)
	if err := s.snapshots.Create(&snapshot.Record{ID: ulid.NewSnapshot(), SandboxID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SnapshotSandbox(t.Context(), &v1.SnapshotSandboxRequest{SandboxId: id}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("snapshot count did not block unbounded per-tenant disk use: %v", err)
	}
}

func TestExcessiveDiskRequestIsRefused(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	s.cfg.MaxDiskBytes = 4096
	spec := runtime.SandboxSpec{Resources: runtime.ResourceLimits{DiskBytes: 4097}}
	if err := s.enforceResourceLimits(&spec); err == nil {
		t.Fatal("unbounded disk request accepted")
	}
}
