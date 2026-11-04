package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

// TestExecFailureIsAuditedAndNeverSilent covers the three ways an exec can end
// without an exit chunk from the backend. Each must fail the RPC, leave exactly
// one RuntimeFailure exec record with the bytes already forwarded, and count
// as a failed execution: a run that was killed or broken must never read as a
// success in the audit log or the metrics.
func TestExecFailureIsAuditedAndNeverSilent(t *testing.T) {
	cases := []struct {
		name        string
		backend     *fakeExecBackend
		wantStdout  int64
		wantMessage string
	}{
		{
			name:        "backend refuses the exec",
			backend:     &fakeExecBackend{execErr: errors.New("agent unreachable")},
			wantMessage: "exec: agent unreachable",
		},
		{
			name: "stream breaks after output",
			backend: &fakeExecBackend{stream: &fakeExecStream{
				chunks:  []runtime.ExecChunk{{Stdout: []byte("partial")}},
				recvErr: errors.New("connection reset"),
				release: make(chan struct{}),
			}},
			wantStdout:  int64(len("partial")),
			wantMessage: "exec stream: connection reset",
		},
		{
			name:        "stream ends without an exit",
			backend:     &fakeExecBackend{stream: endedStream(runtime.ExecChunk{Stdout: []byte("partial")})},
			wantStdout:  int64(len("partial")),
			wantMessage: "exec stream ended without an exit record (the run was killed)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, id := startFakeSandbox(t, tc.backend)
			failed := s.metrics.ExecFailedTotal.WithLabelValues(string(runtime.ReasonRuntimeFailure))
			before := counterValue(t, failed)

			err := s.ExecSandbox(&v1.ExecSandboxRequest{SandboxId: id, Exec: &v1.ExecRequest{Command: "true"}},
				&execServerStream{got: make(chan struct{}, 8)})

			if status.Code(err) != codes.Internal || status.Convert(err).Message() != tc.wantMessage {
				t.Fatalf("ExecSandbox error = %v, want Internal %q", err, tc.wantMessage)
			}
			execs, err := s.GetSandboxExecs(context.Background(), &v1.GetSandboxExecsRequest{SandboxId: id})
			if err != nil {
				t.Fatal(err)
			}
			if len(execs.GetExecs()) != 1 {
				t.Fatalf("exec records = %v, want exactly one", execs.GetExecs())
			}
			rec := execs.GetExecs()[0]
			if rec.GetReason() != v1.ExitReason_RUNTIME_FAILURE || rec.GetStdoutBytes() != tc.wantStdout || rec.GetExecutionId() == "" {
				t.Fatalf("exec record = %v, want RUNTIME_FAILURE with %d stdout bytes and an execution id", rec, tc.wantStdout)
			}
			if got := counterValue(t, failed) - before; got != 1 {
				t.Fatalf("vora_exec_failed_total{reason=RuntimeFailure} grew by %v, want 1", got)
			}
		})
	}
}

// endedStream yields chunks and then EOF, with no exit chunk.
func endedStream(chunks ...runtime.ExecChunk) *fakeExecStream {
	f := &fakeExecStream{chunks: chunks, release: make(chan struct{})}
	f.once.Do(func() { close(f.release) })
	return f
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// startFakeSandbox creates and starts one sandbox on backend and returns its ID.
func startFakeSandbox(t *testing.T, backend *fakeExecBackend) (*Server, string) {
	t.Helper()
	reg := runtime.NewRegistry()
	if err := reg.Register(backend); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, reg)
	// A cache-layout rootfs so StartSandbox resolves the image without containerd.
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
