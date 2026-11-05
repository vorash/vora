package runner

import (
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/snapshot"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vorash/vora/internal/pool"
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

// pool.Parse checks a pool's template when it is declared, so that a pool
// cannot parse and then fail every start here. That holds only if its idea of
// an unset cap matches enforceResourceLimits', so walk each cap's default from
// both sides.
func TestPoolParseAgreesWithRunnerLimitsOnUnsetCaps(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	s.cfg.MaxMemoryBytes, s.cfg.MaxCPUMillis, s.cfg.MaxPids, s.cfg.MaxDiskBytes, s.cfg.MaxTTL = 0, 0, 0, 0, 0

	atCap := runtime.ResourceLimits{CPUMillis: 8000, MemoryBytes: 8 << 30, Pids: 512, DiskBytes: 8 << 30}
	const atCapMaxAge = 24*time.Hour - 5*time.Minute - time.Minute
	cases := []struct {
		name   string
		over   func(*runtime.ResourceLimits)
		maxAge time.Duration
		accept bool
	}{
		{name: "at every cap", maxAge: atCapMaxAge, accept: true},
		{name: "cpu", over: func(r *runtime.ResourceLimits) { r.CPUMillis++ }, maxAge: atCapMaxAge},
		{name: "memory", over: func(r *runtime.ResourceLimits) { r.MemoryBytes++ }, maxAge: atCapMaxAge},
		{name: "pids", over: func(r *runtime.ResourceLimits) { r.Pids++ }, maxAge: atCapMaxAge},
		{name: "disk", over: func(r *runtime.ResourceLimits) { r.DiskBytes++ }, maxAge: atCapMaxAge},
		{name: "ttl", maxAge: atCapMaxAge + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resources := atCap
			if c.over != nil {
				c.over(&resources)
			}
			size := 1
			raw := pool.RawSpec{
				ID: "pool_1", Name: "p", TenantID: "t1", MaxAge: c.maxAge, MaxRun: 5 * time.Minute, Size: &size,
				Template: runtime.SandboxSpec{
					Image: "img", Backend: "fake", Resources: resources,
					Network: runtime.NetworkPolicy{Mode: runtime.NetworkNone},
				},
			}
			spec, err := pool.Parse(raw, pool.Limits{Reusable: func(string) bool { return true }})
			if accepted := err == nil; accepted != c.accept {
				t.Fatalf("pool.Parse accepted = %v (%v), want %v", accepted, err, c.accept)
			}

			member := raw.Template
			member.TTL = c.maxAge + 5*time.Minute + time.Minute
			if c.accept {
				member = spec.MemberSpec()
			}
			if err := s.enforceResourceLimits(&member); (err == nil) != c.accept {
				t.Fatalf("enforceResourceLimits err = %v, want accepted = %v", err, c.accept)
			}
		})
	}
}
