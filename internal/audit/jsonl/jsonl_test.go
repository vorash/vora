package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

// TestActorRoundTrip asserts attribution survives the on-disk store: records
// that lose who acted are not an audit trail.
func TestActorRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := New(func(sandboxID string) string {
		dir := filepath.Join(root, sandboxID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	})
	actor := sandbox.Actor{
		TenantID:      "tenant-a",
		AgentIdentity: "spiffe://vora.sh/tenant/tenant-a/agent/analyst",
		KeyID:         "key_1",
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	const sandboxID = "sbx_jsonl"

	if err := store.AppendEvent(context.Background(), sandboxID, sandbox.Event{
		SandboxID: sandboxID, Actor: actor, From: sandbox.StateRequested, To: sandbox.StateRunning, Timestamp: now,
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := store.AppendExec(context.Background(), sandboxID, sandbox.ExecRecord{
		ExecutionID: "exe_1", SandboxID: sandboxID, Actor: actor, Command: "/bin/true",
		StartedAt: now, FinishedAt: now, Reason: runtime.ReasonExited,
	}); err != nil {
		t.Fatalf("append exec: %v", err)
	}
	if err := store.AppendEgress(context.Background(), sandboxID, sandbox.EgressRecord{
		SandboxID: sandboxID, Actor: actor, Host: "example.com", Allowed: true, Timestamp: now,
	}); err != nil {
		t.Fatalf("append egress: %v", err)
	}

	events, err := store.Events(context.Background(), sandboxID)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %v, %v", events, err)
	}
	execs, err := store.Execs(context.Background(), sandboxID)
	if err != nil || len(execs) != 1 {
		t.Fatalf("execs = %v, %v", execs, err)
	}
	egress, err := store.Egress(context.Background(), sandboxID)
	if err != nil || len(egress) != 1 {
		t.Fatalf("egress = %v, %v", egress, err)
	}
	for name, got := range map[string]sandbox.Actor{
		"event":  events[0].Actor,
		"exec":   execs[0].Actor,
		"egress": egress[0].Actor,
	} {
		if got != actor {
			t.Errorf("%s actor = %+v, want %+v", name, got, actor)
		}
	}
}

func TestEgressCountsOnlyCoversTheWindow(t *testing.T) {
	root := t.TempDir()
	store := New(func(sandboxID string) string {
		dir := filepath.Join(root, sandboxID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	})
	ctx := context.Background()
	const id = "sbx_counts"
	base := time.Now().UTC()
	for _, r := range []sandbox.EgressRecord{
		{Allowed: true, Timestamp: base.Add(-time.Minute)},
		{Allowed: true, Timestamp: base.Add(time.Second)},
		{Allowed: false, Timestamp: base.Add(2 * time.Second)},
		{Allowed: false, Timestamp: base.Add(time.Minute)},
	} {
		r.SandboxID = id
		if err := store.AppendEgress(ctx, id, r); err != nil {
			t.Fatal(err)
		}
	}
	allowed, denied, err := store.EgressCounts(ctx, id, base, base.Add(10*time.Second))
	if err != nil || allowed != 1 || denied != 1 {
		t.Fatalf("EgressCounts = %d allowed, %d denied, %v; want 1, 1", allowed, denied, err)
	}
}
