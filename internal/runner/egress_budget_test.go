package runner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

func allowed(id, host string) egress.Event {
	return egress.Event{SandboxID: id, Host: host, Allowed: true, Reason: "dns_resolved", Time: time.Now()}
}

// countingAudit counts writes so a test can bound the audit load a workload
// generates, and records the window its egress counts were asked for.
type countingAudit struct {
	audit.Store
	writes     int
	countStart time.Time
	countEnd   time.Time
	countCalls int
	fullReads  int
}

func (c *countingAudit) AppendEgress(ctx context.Context, id string, r sandbox.EgressRecord) error {
	c.writes++
	return c.Store.AppendEgress(ctx, id, r)
}

func (c *countingAudit) Egress(ctx context.Context, id string) ([]sandbox.EgressRecord, error) {
	c.fullReads++
	return c.Store.Egress(ctx, id)
}

func (c *countingAudit) EgressCounts(ctx context.Context, id string, start, end time.Time) (int64, int64, error) {
	c.countCalls++
	c.countStart, c.countEnd = start, end
	return c.Store.EgressCounts(ctx, id, start, end)
}

func budgetServer(t *testing.T, id string) (*Server, *countingAudit) {
	t.Helper()
	s := testServer(t, runtime.NewRegistry())
	if err := os.MkdirAll(s.workspaces.LogsDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &countingAudit{Store: s.audit}
	s.SetAudit(c)
	s.allowedWindow = 100 * time.Millisecond
	return s, c
}

// An allowlisted name can be queried in a tight loop, and every allowed
// decision is a synchronous, fail-closed audit write. Without a budget one
// sandbox drives unbounded inserts into the audit database every tenant shares.
func TestAllowedFloodIsBoundedAndStillAccounted(t *testing.T) {
	const id, flood = "sbx_allowed_flood", 10000
	s, c := budgetServer(t, id)
	for i := 0; i < flood; i++ {
		if err := s.RecordEgress(allowed(id, "api.example.com")); err != nil {
			t.Fatal(err)
		}
	}
	if c.writes > 2*allowedBudget {
		t.Fatalf("%d allowed decisions caused %d audit writes, want a bounded number", flood, c.writes)
	}

	time.Sleep(150 * time.Millisecond)
	if err := s.RecordEgress(allowed(id, "api.example.com")); err != nil {
		t.Fatal(err)
	}
	records, err := s.audit.Egress(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var individual, aggregated int
	for _, r := range records {
		if r.Host != "api.example.com" || !r.Allowed {
			t.Fatalf("unexpected record %+v", r)
		}
		var n int
		if _, err := fmtSscanAggregated(r.Reason, &n); err == nil {
			aggregated += n
		} else {
			individual++
		}
	}
	if individual+aggregated != flood+1 {
		t.Fatalf("audit accounts for %d decisions (%d rows + %d aggregated), want %d", individual+aggregated, individual, aggregated, flood+1)
	}
}

func fmtSscanAggregated(reason string, n *int) (int, error) {
	return fmt.Sscanf(reason, "%d additional allowed decisions aggregated", n)
}

// Aggregation is per host: one noisy name must not hide a first contact with
// another, which is the record an investigator looks for.
func TestAllowedBudgetIsPerHost(t *testing.T) {
	const id = "sbx_allowed_hosts"
	s, _ := budgetServer(t, id)
	for i := 0; i < 1000; i++ {
		if err := s.RecordEgress(allowed(id, "noisy.example.com")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordEgress(allowed(id, "quiet.example.com")); err != nil {
		t.Fatal(err)
	}
	records, err := s.audit.Egress(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Host == "quiet.example.com" {
			return
		}
	}
	t.Fatal("first contact with a second host was not recorded")
}

// Counts of suppressed decisions must not depend on another event arriving: the
// periodic flush and destroy both write them out.
func TestSuppressedAllowedCountsAreFlushed(t *testing.T) {
	const id = "sbx_allowed_flush"
	s, _ := budgetServer(t, id)
	for i := 0; i < allowedBudget+5; i++ {
		if err := s.RecordEgress(allowed(id, "api.example.com")); err != nil {
			t.Fatal(err)
		}
	}
	summaries := func() int {
		records, err := s.audit.Egress(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range records {
			if strings.Contains(r.Reason, "additional allowed decisions aggregated") {
				n++
			}
		}
		return n
	}
	if got := summaries(); got != 0 {
		t.Fatalf("summary written before the window ended: %d", got)
	}
	time.Sleep(150 * time.Millisecond)
	if err := s.FlushEgress(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := summaries(); got != 1 {
		t.Fatalf("summaries after flush = %d, want 1", got)
	}
	if err := s.FlushEgress(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := summaries(); got != 1 {
		t.Fatalf("flush wrote the same counts twice: %d summaries", got)
	}
}

// Every exec used to load the sandbox's whole egress history to count one time
// window, so a flood made every later exec slower and hit the database harder.
func TestEgressInWindowAsksTheStoreForOnlyTheWindow(t *testing.T) {
	const id = "sbx_window"
	s, c := budgetServer(t, id)
	start := time.Now().Add(-time.Second)
	end := time.Now()
	s.egressInWindow(id, start, end)
	if c.fullReads != 0 {
		t.Fatalf("egressInWindow loaded the full history %d times", c.fullReads)
	}
	if c.countCalls != 1 || !c.countStart.Equal(start) || !c.countEnd.Equal(end) {
		t.Fatalf("store asked for [%v, %v] in %d calls, want [%v, %v] once", c.countStart, c.countEnd, c.countCalls, start, end)
	}
}
