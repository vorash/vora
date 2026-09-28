package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
)

// freshExecBackend hands every exec its own finished stream.
type freshExecBackend struct{ fakeExecBackend }

func (b *freshExecBackend) Exec(context.Context, runtime.Handle, runtime.ExecRequest) (runtime.ExecStream, error) {
	s := &fakeExecStream{
		chunks:  []runtime.ExecChunk{{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}}},
		release: make(chan struct{}),
	}
	s.Close()
	return s, nil
}

func execServer(t *testing.T) (*Server, string) {
	t.Helper()
	reg := runtime.NewRegistry()
	if err := reg.Register(&freshExecBackend{}); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, reg)
	if err := os.MkdirAll(filepath.Join(s.cfg.StateDir, "images", "img", "latest", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := s.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{
		Image: "img", Backend: "fake",
		Resources: &v1.ResourceLimits{CpuMillis: 100, MemoryBytes: 1 << 20, Pids: 4},
		Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
	}})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	if _, err := s.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	return s, id
}

func execWithID(s *Server, sandboxID, executionID, command string) error {
	ss := &execServerStream{got: make(chan struct{}, 8)}
	return s.ExecSandbox(&v1.ExecSandboxRequest{
		SandboxId: sandboxID,
		Exec:      &v1.ExecRequest{ExecutionId: executionID, Command: command},
	}, ss)
}

// The execution ID is the audit record's identity. If a caller could pick it,
// a second exec reusing an earlier ID would replace that record and leave a
// command with no record at all. Same class as sandbox IDs (V-01).
func TestExecRejectsCallerChosenExecutionID(t *testing.T) {
	s, id := execServer(t)
	for _, executionID := range []string{ulid.NewExec(), "first", "../../escape", "exe_not-a-ulid"} {
		err := execWithID(s, id, executionID, "true")
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("execution_id %q: err = %v, want InvalidArgument", executionID, err)
		}
	}
	execs, err := s.GetSandboxExecs(context.Background(), &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(execs.GetExecs()) != 0 {
		t.Fatalf("rejected execs left %d records", len(execs.GetExecs()))
	}
}

func TestEveryExecGetsItsOwnRunnerAssignedRecord(t *testing.T) {
	s, id := execServer(t)
	for _, command := range []string{"first", "second"} {
		if err := execWithID(s, id, "", command); err != nil {
			t.Fatalf("exec %s: %v", command, err)
		}
	}
	execs, err := s.GetSandboxExecs(context.Background(), &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		t.Fatal(err)
	}
	got := execs.GetExecs()
	if len(got) != 2 || got[0].GetCommand() != "first" || got[1].GetCommand() != "second" {
		t.Fatalf("exec records = %v, want first and second", got)
	}
	if got[0].GetExecutionId() == got[1].GetExecutionId() {
		t.Fatalf("both execs share execution_id %q", got[0].GetExecutionId())
	}
	for _, e := range got {
		if !ulid.ValidPrefix(e.GetExecutionId(), ulid.PrefixExec) {
			t.Errorf("execution_id %q is not a runner-assigned exe_ ULID", e.GetExecutionId())
		}
	}
}
