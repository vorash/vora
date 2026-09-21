package runner

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

func TestEgressInWindowCountsOnlyTheExecution(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	ctx := context.Background()
	base := time.Now().UTC()
	if err := os.MkdirAll(s.workspaces.LogsDir("sbx_e"), 0o755); err != nil {
		t.Fatal(err)
	}

	records := []sandbox.EgressRecord{
		{SandboxID: "sbx_e", Allowed: true, Timestamp: base.Add(-time.Second)},            // before
		{SandboxID: "sbx_e", Allowed: true, Timestamp: base.Add(100 * time.Millisecond)},  // inside
		{SandboxID: "sbx_e", Allowed: false, Timestamp: base.Add(200 * time.Millisecond)}, // inside
		{SandboxID: "sbx_e", Allowed: false, Timestamp: base.Add(2 * time.Second)},        // after
	}
	for _, r := range records {
		if err := s.audit.AppendEgress(ctx, "sbx_e", r); err != nil {
			t.Fatal(err)
		}
	}

	allowed, denied := s.egressInWindow("sbx_e", base, base.Add(time.Second))
	if allowed != 1 || denied != 1 {
		t.Fatalf("allowed=%d denied=%d, want 1/1", allowed, denied)
	}
}

func TestUsageDeltaWithoutBaselineIsZero(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	cpu, peak := s.usageDelta(context.Background(), nil, nil, usageBaseline{})
	if cpu != 0 || peak != 0 {
		t.Fatalf("cpu=%d peak=%d, want 0/0", cpu, peak)
	}
}
