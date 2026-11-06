package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

// metricFamily returns one family from the runner's registry, nil if it has no
// series.
func (h *poolHarness) metricFamily(name string) *dto.MetricFamily {
	h.t.Helper()
	families, err := h.s.metrics.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// A backend refuses these for the request's own content, as gVisor refuses an
// empty command and exec(2) a NUL or an over-long string. A refused exec reads as
// a broken member, so without the up-front check one bad request destroys two.
func TestBadRunRequestCostsNoMember(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(context.Context, string, runtime.ExecRequest) (runtime.ExecStream, error) {
			t.Error("a bad request reached the backend")
			return nil, errors.New("refused")
		}
	})
	spec := poolSpec(2, 2)
	maxRun := int64(10)
	spec.MaxRunSeconds = &maxRun
	p := h.create(spec)
	h.waitIdle(p.GetId(), 2)
	long := strings.Repeat("x", 128<<10)
	half := strings.Repeat("x", 64<<10)
	var flood []string
	for range 20 {
		flood = append(flood, half)
	}
	for name, exec := range map[string]*v1.ExecRequest{
		"no exec at all":   nil,
		"empty command":    {TimeoutSeconds: 1},
		"no timeout":       {Command: "true"},
		"over max_run":     {Command: "true", TimeoutSeconds: 11},
		"negative timeout": {Command: "true", TimeoutSeconds: -1},
		// 2^62 seconds overflows a Duration into something that fits max_run.
		"overflowing timeout": {Command: "true", TimeoutSeconds: 1 << 62},
		"client-minted id":    {Command: "true", TimeoutSeconds: 1, ExecutionId: "exe_x"},
		"NUL in command":      {Command: "tr\x00ue", TimeoutSeconds: 1},
		"NUL in arg":          {Command: "true", Args: []string{"a\x00b"}, TimeoutSeconds: 1},
		"NUL in cwd":          {Command: "true", Cwd: "/tm\x00p", TimeoutSeconds: 1},
		"NUL in env":          {Command: "true", Env: []string{"K=a\x00b"}, TimeoutSeconds: 1},
		"over-long arg":       {Command: "true", Args: []string{long}, TimeoutSeconds: 1},
		"over-long env":       {Command: "true", Env: []string{"K=" + long}, TimeoutSeconds: 1},
		"too much in total":   {Command: "true", Args: flood, TimeoutSeconds: 1},
	} {
		stream, err := h.client.RunInPool(h.ctx(), &v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: h.tenant, Exec: exec})
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	st := h.get(p.GetId()).GetStats()
	if st.GetIdle() != 2 || st.GetDestroyedTotal() != 0 || st.GetRunsTotal() != 0 {
		t.Fatalf("stats after the bad requests = %v, want both members idle and untouched", st)
	}
}

// A client gone by Exec time makes the backend's Exec fail on a dead context,
// which says nothing about the member: it gets its reset and stays.
func TestClientGoneAtExecKeepsTheMember(t *testing.T) {
	h := newWarmHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(c context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			cancel()
			<-c.Done()
			return nil, c.Err()
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	id := h.backend.live()[0]

	if _, err := h.run(ctx, p.GetId(), 5); status.Code(err) != codes.Canceled {
		t.Fatalf("run: %v, want Canceled", err)
	}
	waitFor(t, func() bool { return h.backend.box(id).resets == 1 }, "the member's reset")
	h.waitIdle(p.GetId(), 1)
	if st := h.get(p.GetId()).GetStats(); st.GetDestroyedTotal() != 0 || h.backend.isGone(id) {
		t.Fatalf("stats = %v, gone = %v; a client leaving must not destroy a member", st, h.backend.isGone(id))
	}
}

// A retry that never reaches a program must not reach the run metrics, least of
// all with the first attempt's placement and a zero start time. This calls the
// handler itself: over gRPC the client's call ends before the handler hears of it,
// and a backend released in that gap really does start the run.
func TestRunThatNeverStartedIsNotObserved(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	// As a destroy racing the borrow leaves it: the first attempt finds no handle.
	h.s.delHandle(h.backend.live()[0])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	h.backend.set(func(b *poolBackend) {
		// The first start is the retry's cold start, or a refill the retry waits on.
		b.startHook = func(c context.Context, _ string) error {
			cancel()
			select {
			case <-released:
			case <-c.Done():
			}
			return c.Err()
		}
	})
	if err := h.s.RunInPool(&v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: h.tenant, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 5}}, &runServerStream{ctx: ctx}); err == nil {
		t.Fatal("a run with no member to run on succeeded")
	}
	for _, name := range []string{"vora_pool_runs_total", "vora_pool_run_duration_seconds", "vora_pool_startup_seconds", "vora_exec_duration_seconds"} {
		f := h.metricFamily(name)
		if f == nil {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetCounter().GetValue() != 0 || m.GetHistogram().GetSampleCount() != 0 {
				t.Errorf("%s has a sample %v from a run that never started", name, m)
			}
		}
	}
	close(released)
	h.waitIdle(p.GetId(), 1)
}

// runServerStream is the server's side of a RunInPool stream, for calling the
// handler directly.
type runServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (r *runServerStream) Context() context.Context { return r.ctx }
func (r *runServerStream) Send(*v1.RunChunk) error  { return r.ctx.Err() }

// Both attempts refused: still nothing reached a program.
func TestRefusedExecIsNotObserved(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(context.Context, string, runtime.ExecRequest) (runtime.ExecStream, error) {
			return nil, errors.New("agent unreachable")
		}
	})
	p := h.create(poolSpec(2, 2))
	h.waitIdle(p.GetId(), 2)
	if _, err := h.run(h.ctx(), p.GetId(), 5); status.Code(err) != codes.Unavailable {
		t.Fatalf("run: %v, want Unavailable", err)
	}
	if f := h.metricFamily("vora_pool_runs_total"); f != nil {
		t.Fatalf("vora_pool_runs_total = %v, want no series: no program started", f.GetMetric())
	}
}

// The design's section 9: vora_exec_* keep firing for pooled runs.
func TestPooledRunObservesExecDuration(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	if _, err := h.run(h.ctx(), p.GetId(), 5); err != nil {
		t.Fatal(err)
	}
	f := h.metricFamily("vora_exec_duration_seconds")
	if f == nil || f.GetMetric()[0].GetHistogram().GetSampleCount() != 1 {
		t.Fatalf("vora_exec_duration_seconds = %v, want one sample", f)
	}
}
