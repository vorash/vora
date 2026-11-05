package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
)

// poolBackend is a runtime.Backend that implements Reusable and Sweeper and
// keeps its own ledger of every sandbox, so a test can assert what the pool
// left behind from the backend's point of view: nothing live after a failed
// start, no sandbox serving two runs at once, no run on a sandbox whose last
// reset did not pass.
type poolBackend struct {
	mu    sync.Mutex
	name  string
	boxes map[string]*fakeBox
	gone  map[string]bool
	t     testing.TB

	// Hooks. Each sees the sandbox id. A nil hook is the happy path.
	startHook   func(ctx context.Context, id string) error
	prepareHook func(ctx context.Context, id string) error
	execHook    func(ctx context.Context, id string, req runtime.ExecRequest) (runtime.ExecStream, error)
	resetHook   func(ctx context.Context, id string) error
	// destroyIgnoresCancel makes Destroy succeed on a cancelled context. The
	// default refuses, as a backend honouring ctx would, which is what makes
	// a leaked cleanup visible.
	destroyIgnoresCancel bool
	// leftover are ids List reports beyond the ledger: a previous process's
	// sandboxes, for the restart test.
	leftover []string
}

type fakeBox struct {
	started  bool
	inUse    bool
	dirty    bool
	prepared bool
	resets   int
	runs     int
}

func newPoolBackend(t testing.TB) *poolBackend {
	return &poolBackend{name: "fake", boxes: map[string]*fakeBox{}, gone: map[string]bool{}, t: t}
}

func (b *poolBackend) Name() string { return b.name }
func (b *poolBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{EnforcesResourceLimits: true, NetworkModes: []runtime.NetworkMode{runtime.NetworkNone}}
}

func (b *poolBackend) Create(_ context.Context, spec runtime.SandboxSpec) (runtime.Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.boxes[spec.ID] != nil {
		b.t.Errorf("invariant: %s created twice", spec.ID)
	}
	b.boxes[spec.ID] = &fakeBox{}
	return fakeHandle{id: spec.ID}, nil
}

func (b *poolBackend) Start(ctx context.Context, h runtime.Handle) error {
	b.mu.Lock()
	hook := b.startHook
	b.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, h.ID()); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if box := b.boxes[h.ID()]; box != nil {
		box.started = true
	}
	return nil
}

func (b *poolBackend) Exec(ctx context.Context, h runtime.Handle, req runtime.ExecRequest) (runtime.ExecStream, error) {
	b.mu.Lock()
	box := b.boxes[h.ID()]
	hook := b.execHook
	switch {
	case box == nil || !box.started:
		b.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s is not running", h.ID())
	case box.inUse:
		b.t.Errorf("invariant: %s execs twice at once", h.ID())
	case box.dirty && box.prepared:
		b.t.Errorf("invariant: %s runs again with no passing reset since its last run", h.ID())
	}
	b.mu.Unlock()
	var es runtime.ExecStream
	var err error
	if hook != nil {
		es, err = hook(ctx, h.ID(), req)
	} else {
		es = newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("out")}, runtime.ExecChunk{Exit: &runtime.ExecResult{ExitCode: 0, Reason: runtime.ReasonExited}})
	}
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	box.inUse, box.dirty = true, true
	box.runs++
	return &releasing{ExecStream: es, release: func() { b.release(h.ID()) }}, nil
}

func (b *poolBackend) release(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if box := b.boxes[id]; box != nil {
		box.inUse = false
	}
}

func (b *poolBackend) Signal(context.Context, runtime.Handle, int32) error { return nil }
func (b *poolBackend) Stats(context.Context, runtime.Handle) (*runtime.Stats, error) {
	return &runtime.Stats{}, nil
}
func (b *poolBackend) Meter(context.Context, runtime.Handle) (runtime.UsageMeter, error) {
	return &fakeMeter{}, nil
}
func (b *poolBackend) Stop(context.Context, runtime.Handle) error { return nil }

func (b *poolBackend) Destroy(ctx context.Context, h runtime.Handle) error {
	if ctx.Err() != nil && !b.destroyIgnoresCancel {
		return ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if box := b.boxes[h.ID()]; box != nil && box.inUse {
		b.t.Errorf("invariant: %s destroyed while a run uses it", h.ID())
	}
	delete(b.boxes, h.ID())
	b.gone[h.ID()] = true
	return nil
}

// reusableBackend adds Reusable to poolBackend. The base type without it
// can host a cold pool only.
type reusableBackend struct{ *poolBackend }

// Prepare is the admission proof. It marks the sandbox prepared, so a later
// run on a dirty sandbox is an invariant violation.
func (r reusableBackend) Prepare(ctx context.Context, h runtime.Handle) (runtime.Resettable, error) {
	b := r.poolBackend
	b.mu.Lock()
	hook := b.prepareHook
	b.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, h.ID()); err != nil {
			return nil, err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	box := b.boxes[h.ID()]
	if box == nil {
		return nil, runtime.ErrNotFound
	}
	box.prepared = true
	return &fakeReset{b: b, id: h.ID()}, nil
}

// Sweeper: the ids a previous runner process left behind.
func (b *poolBackend) List(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := append([]string(nil), b.leftover...)
	for id := range b.boxes {
		ids = append(ids, id)
	}
	return ids, nil
}

func (b *poolBackend) Reap(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.boxes, id)
	b.gone[id] = true
	var kept []string
	for _, l := range b.leftover {
		if l != id {
			kept = append(kept, l)
		}
	}
	b.leftover = kept
	return nil
}

type fakeReset struct {
	b  *poolBackend
	id string
}

func (r *fakeReset) Reset(ctx context.Context) (runtime.ResetReport, error) {
	b := r.b
	b.mu.Lock()
	hook := b.resetHook
	box := b.boxes[r.id]
	if box != nil && box.inUse {
		b.t.Errorf("invariant: reset of %s while a run uses it", r.id)
	}
	b.mu.Unlock()
	var err error
	if hook != nil {
		err = hook(ctx, r.id)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if box == nil {
		return runtime.ResetReport{}, runtime.ErrUnproven
	}
	box.resets++
	if err != nil {
		return runtime.ResetReport{}, fmt.Errorf("%w: %w", runtime.ErrUnproven, err)
	}
	box.dirty = false
	return runtime.ResetReport{ProcessesKilled: 1}, nil
}

func (r *fakeReset) Alive(ctx context.Context) error { return ctx.Err() }

func (b *poolBackend) live() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id := range b.boxes {
		ids = append(ids, id)
	}
	return ids
}

func (b *poolBackend) box(id string) fakeBox {
	b.mu.Lock()
	defer b.mu.Unlock()
	if box := b.boxes[id]; box != nil {
		return *box
	}
	return fakeBox{}
}

func (b *poolBackend) isGone(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gone[id]
}

func (b *poolBackend) set(fn func(*poolBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b)
}

// ctxStream yields its chunks, then behaves as a backend would on the exec's
// context: once ctx ends (the client left, or the runner's deadline) Recv
// returns an error, as a killed exec does. With hang set it ignores ctx and
// returns only after Close, which is a process holding the exec's stdout.
type ctxStream struct {
	ctx    context.Context
	chunks []runtime.ExecChunk
	i      int
	hang   bool
	closed chan struct{}
	once   sync.Once
	// gate, when set, is waited on before the last chunk is yielded.
	gate <-chan struct{}
}

func newCtxStream(ctx context.Context, hang bool, chunks ...runtime.ExecChunk) *ctxStream {
	return &ctxStream{ctx: ctx, hang: hang, chunks: chunks, closed: make(chan struct{})}
}

func (s *ctxStream) Recv() (runtime.ExecChunk, error) {
	if s.i < len(s.chunks) {
		if s.gate != nil && s.i == len(s.chunks)-1 {
			select {
			case <-s.gate:
			case <-s.closed:
				return runtime.ExecChunk{}, errors.New("exec stream closed")
			}
		}
		c := s.chunks[s.i]
		s.i++
		return c, nil
	}
	if s.hang {
		<-s.closed
		return runtime.ExecChunk{}, errors.New("exec stream closed")
	}
	select {
	case <-s.ctx.Done():
		return runtime.ExecChunk{}, errors.New("exec killed: " + s.ctx.Err().Error())
	case <-s.closed:
		return runtime.ExecChunk{}, io.EOF
	}
}

func (s *ctxStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// releasing clears the backend's in-use mark when the runner closes the
// stream, which is when the exec is over from the backend's side.
type releasing struct {
	runtime.ExecStream
	release func()
	once    sync.Once
}

func (r *releasing) Close() error {
	r.once.Do(r.release)
	return r.ExecStream.Close()
}

// poolHarness is a runner served over bufconn with one fake backend.
type poolHarness struct {
	t       *testing.T
	s       *Server
	backend *poolBackend
	client  v1.RunnerServiceClient
	tenant  string
}

const testTenant = "tenant-a"

func newPoolHarness(t *testing.T, backend runtime.Backend) *poolHarness {
	t.Helper()
	reg := runtime.NewRegistry()
	if err := reg.Register(backend); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, reg)
	tunePoolServer(t, s)
	if err := s.LoadPools(); err != nil {
		t.Fatal(err)
	}
	h := &poolHarness{t: t, s: s, tenant: testTenant}
	switch b := backend.(type) {
	case *poolBackend:
		h.backend = b
	case reusableBackend:
		h.backend = b.poolBackend
	}
	h.client = serveBufconn(t, s)
	return h
}

// newWarmHarness is the common case: a reusable fake backend.
func newWarmHarness(t *testing.T) *poolHarness {
	t.Helper()
	return newPoolHarness(t, reusableBackend{newPoolBackend(t)})
}

// tunePoolServer makes every pool timeout short, and the image resolvable
// without containerd (a cache-layout rootfs).
func tunePoolServer(t *testing.T, s *Server) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.cfg.StateDir, "images", "img", "latest", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.execGrace = 50 * time.Millisecond
	s.poolOptions = pool.Options{
		ResetTimeout: time.Second, ProbeTimeout: time.Second, StartTimeout: 5 * time.Second,
		DestroyTimeout: time.Second, DestroyRetry: 10 * time.Millisecond,
		StartBackoff: 10 * time.Millisecond, MaxStartBackoff: 50 * time.Millisecond,
	}
	t.Cleanup(s.ClosePools)
}

func serveBufconn(t *testing.T, s *Server) v1.RunnerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterRunnerServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})
	return v1.NewRunnerServiceClient(conn)
}

func poolSpec(size, maxSize uint32) *v1.PoolSpec {
	return &v1.PoolSpec{
		Name: "py", TenantId: testTenant,
		Sandbox: &v1.SandboxSpec{
			Image: "img", Backend: "fake",
			Resources: &v1.ResourceLimits{CpuMillis: 100, MemoryBytes: 1 << 20, Pids: 16},
			Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
		},
		Size: &size, MaxSize: &maxSize,
	}
}

func (h *poolHarness) create(spec *v1.PoolSpec) *v1.Pool {
	h.t.Helper()
	p, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: spec})
	if err != nil {
		h.t.Fatalf("CreatePool: %v", err)
	}
	return p
}

func (h *poolHarness) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	h.t.Cleanup(cancel)
	return ctx
}

func (h *poolHarness) get(id string) *v1.Pool {
	h.t.Helper()
	p, err := h.client.GetPool(h.ctx(), &v1.GetPoolRequest{PoolId: id, TenantId: h.tenant})
	if err != nil {
		h.t.Fatalf("GetPool: %v", err)
	}
	return p
}

// run streams one run and returns every frame, in order, plus the stream's
// final error.
func (h *poolHarness) run(ctx context.Context, id string, timeoutSeconds int64) ([]*v1.RunChunk, error) {
	h.t.Helper()
	stream, err := h.client.RunInPool(ctx, &v1.RunInPoolRequest{
		PoolId: id, TenantId: h.tenant,
		Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: timeoutSeconds},
	})
	if err != nil {
		return nil, err
	}
	return collect(stream)
}

func collect(stream grpc.ServerStreamingClient[v1.RunChunk]) ([]*v1.RunChunk, error) {
	var frames []*v1.RunChunk
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return frames, nil
		}
		if err != nil {
			return frames, err
		}
		frames = append(frames, c)
	}
}

// kinds names each frame's oneof arm, so a test can assert the order.
func kinds(frames []*v1.RunChunk) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		switch f.GetPayload().(type) {
		case *v1.RunChunk_Run:
			out = append(out, "run")
		case *v1.RunChunk_Stdout:
			out = append(out, "stdout")
		case *v1.RunChunk_Stderr:
			out = append(out, "stderr")
		case *v1.RunChunk_Exit:
			out = append(out, "exit")
		case *v1.RunChunk_RunDone:
			out = append(out, "run_done")
		default:
			out = append(out, "?")
		}
	}
	return out
}

func (h *poolHarness) waitIdle(id string, n int) {
	h.t.Helper()
	waitFor(h.t, func() bool { return h.get(id).GetStats().GetIdle() == uint32(n) }, "pool %s to have %d idle members", id, n)
}

const patience = 20 * time.Second

func waitFor(t testing.TB, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for "+format, args...)
}
