package runner

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/runtime"
)

func TestDenyFloodDoesNotFillRunnerAuditDisk(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	id := "sbx_flood"
	if err := os.MkdirAll(s.workspaces.LogsDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if err := s.RecordEgress(egress.Event{SandboxID: id, Host: "blocked.example.com", Reason: "denied"}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := s.audit.Egress(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) > 100 {
		t.Fatalf("workload generated %d audit writes in one burst", len(records))
	}
	time.Sleep(time.Second)
	if err := s.RecordEgress(egress.Event{SandboxID: id, Reason: "denied"}); err != nil {
		t.Fatal(err)
	}
	records, err = s.audit.Egress(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		if strings.Contains(r.Reason, "additional denials suppressed") {
			found = true
		}
	}
	if !found {
		t.Fatal("audit lost denied event count without a suppression marker")
	}
}
