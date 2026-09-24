package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vorash/vora/internal/sandbox"
)

func TestEgressLogIsBoundedUnderDenyFlood(t *testing.T) {
	root := t.TempDir()
	s := New(func(string) string { return root })
	for i := 0; i < 10000; i++ {
		if err := s.AppendEgress(context.Background(), "sbx_test", sandbox.EgressRecord{SandboxID: "sbx_test", Host: "blocked.example.com", Reason: "denied"}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(root, sandbox.EgressFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxEgressLogBytes {
		t.Fatalf("denied workload filled %d bytes of unquotaed audit disk", info.Size())
	}
	if err := s.AppendEgress(context.Background(), "sbx_test", sandbox.EgressRecord{Allowed: true}); err == nil {
		t.Fatal("allowed traffic proceeded without a durable audit entry")
	}
}
