package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
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
		ExecutionID: ulid.NewExec(), SandboxID: sandboxID, Actor: actor, Command: "/bin/true",
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

func execStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return New(func(sandboxID string) string {
		dir := filepath.Join(root, sandboxID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	})
}

// Records are append-only: reusing an execution ID must fail and leave the
// original untouched, or one command silently loses its record.
func TestAppendExecNeverReplacesARecord(t *testing.T) {
	store := execStore(t)
	ctx := context.Background()
	const sandboxID = "sbx_jsonl"
	id := ulid.NewExec()
	now := time.Now().UTC()

	if err := store.AppendExec(ctx, sandboxID, sandbox.ExecRecord{ExecutionID: id, SandboxID: sandboxID, Command: "first", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExec(ctx, sandboxID, sandbox.ExecRecord{ExecutionID: id, SandboxID: sandboxID, Command: "second", StartedAt: now}); err == nil {
		t.Fatal("second AppendExec with the same execution ID succeeded")
	}
	execs, err := store.Execs(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(execs) != 1 || execs[0].Command != "first" {
		t.Fatalf("records = %+v, want only the original", execs)
	}
}

// The execution ID becomes a filename, so the store checks it itself rather
// than trusting every caller to have validated it.
func TestAppendExecRejectsNonCanonicalID(t *testing.T) {
	store := execStore(t)
	for _, id := range []string{"", "first", "../escape", "exe_../x", "sbx_01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		err := store.AppendExec(context.Background(), "sbx_jsonl", sandbox.ExecRecord{ExecutionID: id, Command: "x"})
		if err == nil {
			t.Errorf("execution ID %q accepted", id)
		}
	}
}
