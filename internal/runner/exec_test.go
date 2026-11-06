package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
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
	if allowed == nil || denied == nil || *allowed != 1 || *denied != 1 {
		t.Fatalf("allowed=%v denied=%v, want 1/1", allowed, denied)
	}
}

type fakeHandle struct{ id string }

func (h fakeHandle) ID() string { return h.id }

// fakeExecStream yields its chunks, then holds the stream open before EOF so a
// test can observe external state at the exact moment the client would have
// seen the exit chunk.
type fakeExecStream struct {
	chunks  []runtime.ExecChunk
	i       int
	release chan struct{}
	once    sync.Once
	// recvErr, when set, is returned after the chunks instead of EOF.
	recvErr error
}

func (f *fakeExecStream) Recv() (runtime.ExecChunk, error) {
	if f.i < len(f.chunks) {
		c := f.chunks[f.i]
		f.i++
		return c, nil
	}
	if f.recvErr != nil {
		return runtime.ExecChunk{}, f.recvErr
	}
	<-f.release
	return runtime.ExecChunk{}, io.EOF
}

func (f *fakeExecStream) Close() error {
	f.once.Do(func() { close(f.release) })
	return nil
}

type fakeExecBackend struct {
	stream *fakeExecStream
	// execErr, when set, fails Exec before any chunk.
	execErr error
	// meter is the window every exec opens; meterErr, when set, refuses it.
	meter    fakeMeter
	meterErr error
}

// fakeMeter reports a chosen usage and counts its stops, so a test can check
// that every way an exec ends closes its window exactly once.
type fakeMeter struct {
	usage runtime.Usage
	err   error
	stops int
}

func (m *fakeMeter) Stop(context.Context) (runtime.Usage, error) {
	m.stops++
	return m.usage, m.err
}

func (b *fakeExecBackend) Name() string { return "fake" }
func (b *fakeExecBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{EnforcesResourceLimits: true, NetworkModes: []runtime.NetworkMode{runtime.NetworkNone}}
}
func (b *fakeExecBackend) Create(_ context.Context, spec runtime.SandboxSpec) (runtime.Handle, error) {
	return fakeHandle{id: spec.ID}, nil
}
func (b *fakeExecBackend) Start(context.Context, runtime.Handle) error { return nil }
func (b *fakeExecBackend) Exec(context.Context, runtime.Handle, runtime.ExecRequest) (runtime.ExecStream, error) {
	if b.execErr != nil {
		return nil, b.execErr
	}
	return b.stream, nil
}
func (b *fakeExecBackend) Signal(context.Context, runtime.Handle, int32) error { return nil }
func (b *fakeExecBackend) Stats(context.Context, runtime.Handle) (*runtime.Stats, error) {
	return &runtime.Stats{}, nil
}
func (b *fakeExecBackend) Meter(context.Context, runtime.Handle) (runtime.UsageMeter, error) {
	if b.meterErr != nil {
		return nil, b.meterErr
	}
	return &b.meter, nil
}
func (b *fakeExecBackend) Stop(context.Context, runtime.Handle) error    { return nil }
func (b *fakeExecBackend) Destroy(context.Context, runtime.Handle) error { return nil }

// execServerStream captures what a gRPC stream would hand a client.
type execServerStream struct {
	grpc.ServerStream
	mu   sync.Mutex
	sent []*v1.ExecChunk
	got  chan struct{}
}

func (s *execServerStream) Context() context.Context { return context.Background() }
func (s *execServerStream) Send(c *v1.ExecChunk) error {
	s.mu.Lock()
	s.sent = append(s.sent, c)
	s.mu.Unlock()
	select {
	case s.got <- struct{}{}:
	default:
	}
	return nil
}
func (s *execServerStream) exitSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.sent {
		if c.GetExit() != nil {
			return true
		}
	}
	return false
}

// TestExecRecordPersistsBeforeExitChunk pins the ordering invariant: clients
// act on the exit chunk the instant it arrives (the API closes its response
// right there and callers destroy the sandbox), so the exec record must be on
// disk before the chunk is forwarded. Recording afterwards raced teardown and
// resurrected sandboxes/<id>/logs inside deleted state.
func TestExecRecordPersistsBeforeExitChunk(t *testing.T) {
	stream := &fakeExecStream{
		chunks:  []runtime.ExecChunk{{Exit: &runtime.ExecResult{ExitCode: 7, Reason: runtime.ReasonTimeout}}},
		release: make(chan struct{}),
	}
	reg := runtime.NewRegistry()
	if err := reg.Register(&fakeExecBackend{stream: stream}); err != nil {
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
	if _, err := s.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: created.GetSandbox().GetId()}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}

	id := created.GetSandbox().GetId()
	ss := &execServerStream{got: make(chan struct{}, 8)}
	done := make(chan error, 1)
	go func() {
		done <- s.ExecSandbox(&v1.ExecSandboxRequest{SandboxId: id, Exec: &v1.ExecRequest{Command: "true"}}, ss)
	}()

	deadline := time.After(5 * time.Second)
	for !ss.exitSeen() {
		select {
		case <-ss.got:
		case <-deadline:
			t.Fatal("exit chunk was never forwarded")
		}
	}

	// The moment a client could see the exit, the record must be readable.
	execs, err := s.GetSandboxExecs(ctx, &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(execs.GetExecs()) != 1 || execs.GetExecs()[0].GetReason() != v1.ExitReason_TIMEOUT_EXCEEDED {
		t.Fatalf("exec records at exit-chunk time = %v, want the finished record already persisted", execs.GetExecs())
	}

	stream.once.Do(func() { close(stream.release) })
	if err := <-done; err != nil {
		t.Fatalf("ExecSandbox: %v", err)
	}
}
