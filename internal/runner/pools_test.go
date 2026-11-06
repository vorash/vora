package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// lockedBuffer collects logs from whichever goroutine writes them.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// How much of the runner is free depends on what other tenants hold, so a tenant
// must read the same refusal whatever they hold, and the operator reads the numbers.
func TestPoolCapacityRefusalSaysNothingOfOtherTenants(t *testing.T) {
	h := newWarmHarness(t)
	logs := &lockedBuffer{}
	h.s.logger = slog.New(slog.NewTextHandler(logs, nil))
	h.s.cfg.MaxSandboxes = 6
	h.s.cfg.MaxSandboxesPerTenant = 6
	refusal := func() string {
		t.Helper()
		mine := poolSpec(0, 2)
		mine.Name = "mine"
		_, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: mine})
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("declare past the runner's cap: %v, want ResourceExhausted", err)
		}
		return status.Convert(err).Message()
	}
	occupy := func(name string, maxSize uint32) {
		t.Helper()
		spec := poolSpec(0, maxSize)
		spec.Name, spec.TenantId = name, "someone-else"
		h.create(spec)
	}

	occupy("first", 5)
	withOneFree := refusal()
	if !strings.Contains(logs.String(), "1 free on the runner") {
		t.Fatalf("the log does not say how much was free:\n%s", logs)
	}
	occupy("second", 1)
	withNoneFree := refusal()
	if !strings.Contains(logs.String(), "0 free on the runner") {
		t.Fatalf("the log does not say how much was free:\n%s", logs)
	}
	if withOneFree != "sandbox capacity reached" || withNoneFree != withOneFree {
		t.Fatalf("refusals were %q and %q, want the same fixed text", withOneFree, withNoneFree)
	}
}

func TestRunInPoolStreamsRunOutputExitThenDone(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	if got, want := kinds(frames), []string{"run", "stdout", "exit", "run_done"}; !slices.Equal(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	run, exit, done := frames[0].GetRun(), frames[2].GetExit(), frames[3].GetRunDone()
	switch {
	case run.GetPoolId() != p.GetId(), run.GetSandboxId() == "", !run.GetWarm(), run.GetSandboxRun() != 1, run.GetResetMs() != 0:
		t.Fatalf("run frame = %v, want this pool, a warm member on its first run with no prior reset", run)
	case run.GetExecutionId() == "" || run.GetExecutionId() != exit.GetExecutionId():
		t.Fatalf("run frame execution id %q, exit %q: want the same minted id", run.GetExecutionId(), exit.GetExecutionId())
	case exit.GetReason() != v1.ExitReason_EXITED || exit.GetExitCode() != 0:
		t.Fatalf("exit = %v, want EXITED 0", exit)
	case done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_OK:
		t.Fatalf("run_done = %v, want a passing reset", done)
	}

	// The record is the audit trail of the run: it names the pool and the run
	// number, and it is readable through the pool.
	execs, err := h.client.GetSandboxExecs(h.ctx(), &v1.GetSandboxExecsRequest{SandboxId: run.GetSandboxId(), PoolId: p.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if len(execs.GetExecs()) != 1 || execs.GetExecs()[0].GetPoolId() != p.GetId() || execs.GetExecs()[0].GetSandboxRun() != 1 {
		t.Fatalf("exec records = %v, want one naming pool %s, run 1", execs.GetExecs(), p.GetId())
	}
	if _, err := h.client.GetSandboxExecs(h.ctx(), &v1.GetSandboxExecsRequest{SandboxId: run.GetSandboxId(), PoolId: "pool_01ARZ3NDEKTSV4RRFFQ69G5FAV"}); status.Code(err) != codes.NotFound {
		t.Fatalf("execs through the wrong pool: %v, want NotFound", err)
	}

	// A second run reuses the member after its reset: run 2, with the prior
	// reset's duration, and the backend saw exactly one reset in between.
	h.waitIdle(p.GetId(), 1)
	frames, err = h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatal(err)
	}
	second := frames[0].GetRun()
	if second.GetSandboxId() != run.GetSandboxId() || second.GetSandboxRun() != 2 {
		t.Fatalf("second run = %v, want run 2 on %s", second, run.GetSandboxId())
	}
	if box := h.backend.box(run.GetSandboxId()); box.runs != 2 || box.resets < 1 {
		t.Fatalf("backend saw %d runs and %d resets, want 2 runs after at least one reset", box.runs, box.resets)
	}
	if st := h.get(p.GetId()).GetStats(); st.GetRunsTotal() != 2 || st.GetWarmRunsTotal() != 2 {
		t.Fatalf("stats = %v, want 2 warm runs", st)
	}
}

func TestExecThatDoesNotStartIsRetriedBeforeTheClientSeesAnything(t *testing.T) {
	h := newWarmHarness(t)
	var mu sync.Mutex
	refused := ""
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, id string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			mu.Lock()
			defer mu.Unlock()
			if refused == "" {
				refused = id
			}
			if id == refused {
				return nil, errors.New("agent unreachable")
			}
			return newCtxStream(ctx, false, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}}), nil
		}
	})
	p := h.create(poolSpec(2, 2))
	h.waitIdle(p.GetId(), 2)

	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	mu.Lock()
	first := refused
	mu.Unlock()
	if got := kinds(frames); got[0] != "run" || frames[0].GetRun().GetSandboxId() == first {
		t.Fatalf("frames = %v on %s; want the run frame first, on a member other than %s", got, frames[0].GetRun().GetSandboxId(), first)
	}
	waitFor(t, func() bool { return h.backend.isGone(first) }, "the member whose exec did not start to be destroyed")
	// Both attempts are audited: the refused one as a RuntimeFailure on the
	// first member.
	execs, err := h.s.audit.Execs(context.Background(), first)
	if err != nil || len(execs) != 1 || execs[0].Reason != runtime.ReasonRuntimeFailure {
		t.Fatalf("records on the refused member = %v (%v), want one RuntimeFailure", execs, err)
	}
	if st := h.get(p.GetId()).GetStats(); st.GetRunsTotal() != 1 {
		t.Fatalf("runs_total = %d, want 1: a retried run is one program", st.GetRunsTotal())
	}
}

// A sandbox that dies under a running command reports it as the exec's exit, after the run
// frame went out. The program may have run, so the run is not retried: the client sees the
// failure, the audit trail says so, and the member does not come back.
func TestExitWithRuntimeFailureEndsTheRunAndTheMember(t *testing.T) {
	h := newWarmHarness(t)
	var mu sync.Mutex
	dead := ""
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, id string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			mu.Lock()
			defer mu.Unlock()
			reason := runtime.ReasonExited
			if dead == "" {
				dead = id
				reason = runtime.ReasonRuntimeFailure
			}
			return newCtxStream(ctx, false, runtime.ExecChunk{Exit: &runtime.ExecResult{ExitCode: 128, Reason: reason}}), nil
		}
		b.resetHook = func(_ context.Context, id string) error {
			mu.Lock()
			defer mu.Unlock()
			if id == dead {
				return errors.New("the sandbox is gone")
			}
			return nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	if got := kinds(frames); !slices.Equal(got, []string{"run", "exit", "run_done"}) {
		t.Fatalf("frames = %v", got)
	}
	id := frames[0].GetRun().GetSandboxId()
	if got := frames[1].GetExit().GetReason(); got != v1.ExitReason_RUNTIME_FAILURE {
		t.Fatalf("exit reason = %v, want RUNTIME_FAILURE", got)
	}
	if got := frames[2].GetRunDone(); got.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_FAILED || got.GetRetireCause() != "reset_failed" {
		t.Fatalf("run_done = %v, want a failed reset retiring the member", got)
	}
	execs, err := h.s.audit.Execs(context.Background(), id)
	if err != nil || len(execs) != 1 || execs[0].Reason != runtime.ReasonRuntimeFailure {
		t.Fatalf("records = %v (%v), want one RuntimeFailure", execs, err)
	}
	failed := h.metricFamily("vora_exec_failed_total")
	if failed == nil || len(failed.GetMetric()) != 1 || failed.GetMetric()[0].GetCounter().GetValue() != 1 {
		t.Fatalf("vora_exec_failed_total = %v, want 1: a dead sandbox is not a good exec", failed)
	}

	h.waitIdle(p.GetId(), 1)
	frames, err = h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatalf("the next run: %v", err)
	}
	if got := frames[0].GetRun().GetSandboxId(); got == id {
		t.Fatalf("member %s came back after its sandbox died", id)
	}
}

// A member destroyed behind the pool's back (the disk-quota enforcer still
// acts on records directly) has no handle. The run must not reach the client
// on it: the pool retires it and retries on another member.
func TestMemberDestroyedOutsideThePoolIsRetiredOnBorrow(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 2))
	h.waitIdle(p.GetId(), 1)
	recs, _ := h.s.store.List()
	victim := recs[0].ID
	if err := h.s.destroy(context.Background(), victim, ""); err != nil {
		t.Fatal(err)
	}
	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	run := frames[0].GetRun()
	// Not asserting its warmth: the retry cold-starts, unless the victim's destroy
	// got in first and a refill is on its way, which the borrow rule prefers.
	if got := kinds(frames); got[0] != "run" || run.GetSandboxId() == victim {
		t.Fatalf("frames = %v, run = %v; want a run frame on a member other than the destroyed %s", got, run, victim)
	}
	waitFor(t, func() bool {
		st := h.get(p.GetId()).GetStats()
		return st.GetIdle() == 1 && st.GetDestroyedTotal() == 1
	}, "the pool to replace the destroyed member")
}

func TestFailedResetRetiresTheMember(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.resetHook = func(context.Context, string) error { return errors.New("control socket gone") }
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatal(err)
	}
	id, done := frames[0].GetRun().GetSandboxId(), frames[len(frames)-1].GetRunDone()
	if done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_FAILED || done.GetRetireCause() != "reset_failed" {
		t.Fatalf("run_done = %v, want a failed reset retiring the member", done)
	}
	waitFor(t, func() bool { return h.backend.isGone(id) }, "%s to be destroyed", id)
	// The pool refills with a new member and never lends the old one again.
	waitFor(t, func() bool {
		st := h.get(p.GetId()).GetStats()
		return st.GetIdle() == 1 && st.GetDestroyedTotal() == 1
	}, "refill after the failed reset")
	rec, err := h.s.store.Load(id)
	if err != nil || rec.State != sandbox.StateDestroyed {
		t.Fatalf("record of the retired member = %v (%v), want DESTROYED", rec, err)
	}
	h.s.events.flush()
	events, _ := h.s.audit.Events(context.Background(), id)
	var kinds []string
	for _, e := range events {
		if e.Kind != "" {
			kinds = append(kinds, e.Kind+":"+e.Detail)
		}
	}
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "reset:failed") || !strings.Contains(joined, "retire:reset_failed") {
		t.Fatalf("pool events = %q, want a failed reset and the retire", joined)
	}
}

func TestConcurrentRunsNeverShareAMember(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			gate := make(chan struct{})
			time.AfterFunc(5*time.Millisecond, func() { close(gate) })
			s := newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("x")}, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}})
			s.gate = gate
			return s, nil
		}
	})
	size := uint32(4)
	spec := poolSpec(size, size)
	borrow := int64(5000)
	spec.BorrowTimeoutMs = &borrow
	p := h.create(spec)

	const runs = 24
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	seen := make(chan string, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames, err := h.run(h.ctx(), p.GetId(), 5)
			if err != nil {
				errs <- err
				return
			}
			if got := kinds(frames); !slices.Equal(got, []string{"run", "stdout", "exit", "run_done"}) {
				errs <- errors.New("frames " + strings.Join(got, ","))
				return
			}
			seen <- frames[0].GetRun().GetSandboxId()
		}()
	}
	wg.Wait()
	close(errs)
	close(seen)
	for err := range errs {
		t.Errorf("run: %v", err)
	}
	members := map[string]bool{}
	for id := range seen {
		members[id] = true
	}
	if len(members) > int(size) {
		t.Fatalf("%d distinct members served runs, pool max_size is %d", len(members), size)
	}
	if st := h.get(p.GetId()).GetStats(); st.GetRunsTotal() != runs {
		t.Fatalf("runs_total = %d, want %d", st.GetRunsTotal(), runs)
	}
}

func TestDeletePoolWhileARunIsInFlight(t *testing.T) {
	h := newWarmHarness(t)
	release := make(chan struct{})
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			s := newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("x")}, runtime.ExecChunk{Exit: &runtime.ExecResult{Reason: runtime.ReasonExited}})
			s.gate = release
			return s, nil
		}
	})
	p := h.create(poolSpec(2, 2))
	h.waitIdle(p.GetId(), 2)

	type result struct {
		frames []*v1.RunChunk
		err    error
	}
	got := make(chan result, 1)
	go func() {
		frames, err := h.run(h.ctx(), p.GetId(), 5)
		got <- result{frames, err}
	}()
	waitFor(t, func() bool { return h.get(p.GetId()).GetStats().GetBusy() == 1 }, "the run to hold a member")

	resp, err := h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 200})
	if err != nil || resp.GetDrained() {
		t.Fatalf("DeletePool with a run in flight = %v, %v; want drained=false", resp, err)
	}
	if h.get(p.GetId()).GetState() != v1.PoolState_POOL_DRAINING {
		t.Fatal("pool is not reported draining")
	}
	// A new run on a draining pool is refused, and the one in flight finishes
	// with every frame.
	if _, err := h.run(h.ctx(), p.GetId(), 5); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("run on a draining pool: %v, want FailedPrecondition", err)
	}
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: poolSpec(2, 2)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("redeclare while draining: %v, want FailedPrecondition", err)
	}
	close(release)
	r := <-got
	if r.err != nil || !slices.Equal(kinds(r.frames), []string{"run", "stdout", "exit", "run_done"}) {
		t.Fatalf("in-flight run = %v, %v; want every frame", kinds(r.frames), r.err)
	}
	if r.frames[3].GetRunDone().GetRetireCause() != "drain" {
		t.Fatalf("run_done = %v, want the member retired by the drain", r.frames[3].GetRunDone())
	}

	resp, err = h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: h.tenant, WaitMs: 5000})
	if err != nil || !resp.GetDrained() {
		t.Fatalf("second DeletePool = %v, %v; want drained=true", resp, err)
	}
	if _, err := h.client.GetPool(h.ctx(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: h.tenant}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetPool after delete: %v, want NotFound", err)
	}
	if live := h.backend.live(); len(live) != 0 {
		t.Fatalf("backend still has %v after the pool was deleted", live)
	}
	if global, _, _ := h.s.committed(h.tenant); global != 0 {
		t.Fatalf("capacity still committed after delete: %d", global)
	}
}

func TestHardDeadlineEndsAnExecTheBackendNeverReturns(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			// A process holding stdout: the stream ignores ctx entirely.
			return newCtxStream(ctx, true, runtime.ExecChunk{Stdout: []byte("partial")}), nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	began := time.Now()
	frames, err := h.run(h.ctx(), p.GetId(), 1)
	took := time.Since(began)
	if err != nil {
		t.Fatalf("RunInPool: %v", err)
	}
	if got := kinds(frames); !slices.Equal(got, []string{"run", "stdout", "exit", "run_done"}) {
		t.Fatalf("frames = %v", got)
	}
	exit := frames[2].GetExit()
	if exit.GetReason() != v1.ExitReason_TIMEOUT_EXCEEDED || exit.GetExitCode() != timedOutExitCode {
		t.Fatalf("exit = %v, want a synthesized TIMEOUT_EXCEEDED", exit)
	}
	if took > 3*time.Second {
		t.Fatalf("run took %s; the hard deadline is timeout (1s) + grace (%s)", took, h.s.execGrace)
	}
	id := frames[0].GetRun().GetSandboxId()
	execs, _ := h.s.audit.Execs(context.Background(), id)
	if len(execs) != 1 || execs[0].Reason != runtime.ReasonTimeout {
		t.Fatalf("exec records = %v, want one TIMEOUT_EXCEEDED", execs)
	}
	// The member was reset after the run ended, which is what frees the holder.
	waitFor(t, func() bool { return h.backend.box(id).resets == 1 }, "the member to be reset")
}

func TestClientDisconnectMidRunResetsTheMember(t *testing.T) {
	h := newWarmHarness(t)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			return newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("partial")}), nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.client.RunInPool(ctx, &v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: h.tenant, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetRun() == nil {
		t.Fatalf("first frame = %v, %v", first, err)
	}
	id := first.GetRun().GetSandboxId()
	cancel()

	waitFor(t, func() bool { return h.backend.box(id).resets == 1 }, "the member to be reset after the client left")
	h.waitIdle(p.GetId(), 1)
	execs, _ := h.s.audit.Execs(context.Background(), id)
	if len(execs) != 1 || execs[0].Reason != runtime.ReasonRuntimeFailure {
		t.Fatalf("exec records = %v, want one RuntimeFailure for the broken run", execs)
	}
	if !h.backend.box(id).started || h.backend.isGone(id) {
		t.Fatal("the member was destroyed; a disconnect needs a reset, not a replacement")
	}
}

// The reset's proof holds only for an exec that is over from the runner's side, so the exec's
// stream must be closed, and its context done, before the reset starts. The hook reads both as
// the reset begins, so this pins the order without a timing window. A disconnect cancels the
// context whether or not the stream was closed, which is why the stream is read as well.
func TestDisconnectEndsTheExecBeforeTheResetStarts(t *testing.T) {
	h := newWarmHarness(t)
	type atReset struct {
		ctxErr error
		closed bool
	}
	var (
		mu      sync.Mutex
		execCtx context.Context
		exec    *ctxStream
	)
	seen := make(chan atReset, 1)
	h.backend.set(func(b *poolBackend) {
		b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
			mu.Lock()
			defer mu.Unlock()
			execCtx, exec = ctx, newCtxStream(ctx, false, runtime.ExecChunk{Stdout: []byte("partial")})
			return exec, nil
		}
		b.resetHook = func(context.Context, string) error {
			mu.Lock()
			defer mu.Unlock()
			closed := false
			select {
			case <-exec.closed:
				closed = true
			default:
			}
			seen <- atReset{ctxErr: execCtx.Err(), closed: closed}
			return nil
		}
	})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := h.client.RunInPool(ctx, &v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: h.tenant, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := stream.Recv(); err != nil || first.GetRun() == nil {
		t.Fatalf("first frame = %v, %v", first, err)
	}
	cancel()

	select {
	case got := <-seen:
		if got.ctxErr == nil {
			t.Error("the reset started with the exec's context still live")
		}
		if !got.closed {
			t.Error("the reset started with the exec's stream still open")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the member was never reset")
	}
}

// A missing command is an ordinary exit from each backend, 127 on bwrap and 128 on gVisor. It
// must cost neither a member nor a retry: retiring the member would replace a sandbox for
// every typo, and a retry would run the typo twice.
func TestMissingCommandCostsNoMember(t *testing.T) {
	for _, code := range []int{127, 128} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			h := newWarmHarness(t)
			h.backend.set(func(b *poolBackend) {
				b.execHook = func(ctx context.Context, _ string, _ runtime.ExecRequest) (runtime.ExecStream, error) {
					return newCtxStream(ctx, false,
						runtime.ExecChunk{Stderr: []byte("no such command")},
						runtime.ExecChunk{Exit: &runtime.ExecResult{ExitCode: code, Reason: runtime.ReasonExited}}), nil
				}
			})
			p := h.create(poolSpec(1, 1))
			h.waitIdle(p.GetId(), 1)

			member := ""
			for run := 1; run <= 2; run++ {
				frames, err := h.run(h.ctx(), p.GetId(), 5)
				if err != nil {
					t.Fatalf("run %d: %v", run, err)
				}
				if got, want := kinds(frames), []string{"run", "stderr", "exit", "run_done"}; !slices.Equal(got, want) {
					t.Fatalf("run %d: frames = %v, want %v", run, got, want)
				}
				info, exit, done := frames[0].GetRun(), frames[2].GetExit(), frames[3].GetRunDone()
				if exit.GetExitCode() != int32(code) || exit.GetReason() != v1.ExitReason_EXITED {
					t.Fatalf("run %d: exit = %v, want %d with EXITED", run, exit, code)
				}
				if done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_OK {
					t.Fatalf("run %d: run_done = %v, want a passing reset", run, done)
				}
				if member != "" && info.GetSandboxId() != member {
					t.Fatalf("run %d landed on %s, not on %s", run, info.GetSandboxId(), member)
				}
				member = info.GetSandboxId()
				if !info.GetWarm() || int(info.GetSandboxRun()) != run {
					t.Fatalf("run %d: run frame = %v, want warm and its own number", run, info)
				}
			}
			if got := h.backend.box(member).runs; got != 2 {
				t.Fatalf("the member ran %d execs for 2 runs, so a run was retried", got)
			}
			if failed := h.metricFamily("vora_exec_failed_total"); failed != nil {
				t.Fatalf("vora_exec_failed_total = %v, want nothing counted", failed)
			}
			if st := h.get(p.GetId()).GetStats(); st.GetDestroyedTotal() != 0 {
				t.Fatalf("stats %v, want no member lost", st)
			}
		})
	}
}

func TestClientDisconnectMidColdStartLeaksNoSandbox(t *testing.T) {
	h := newWarmHarness(t)
	started := make(chan struct{}, 1)
	h.backend.set(func(b *poolBackend) {
		b.startHook = func(ctx context.Context, _ string) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
	})
	p := h.create(poolSpec(0, 1))

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.client.RunInPool(ctx, &v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: h.tenant, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("stream after cancel: %v, want Canceled", err)
	}

	// The slot frees last: the backend is gone, then the record says so, and only
	// then does the failed start return to the pool.
	waitFor(t, func() bool {
		st := h.get(p.GetId()).GetStats()
		return st.GetStarting()+st.GetBusy()+st.GetIdle()+st.GetRetiring() == 0
	}, "the pool to give up the slot")
	if live := h.backend.live(); len(live) != 0 {
		t.Fatalf("the backend still holds the half-started %v", live)
	}
	recs, _ := h.s.store.List()
	for _, rec := range recs {
		if rec.State != sandbox.StateDestroyed {
			t.Fatalf("record %s is %s, want DESTROYED", rec.ID, rec.State)
		}
	}
}

func TestPoolsAreScopedToTheirTenant(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	other := "tenant-b"
	if _, err := h.client.GetPool(h.ctx(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: other}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetPool from another tenant: %v, want NotFound", err)
	}
	if _, err := h.client.DeletePool(h.ctx(), &v1.DeletePoolRequest{PoolId: p.GetId(), TenantId: other}); status.Code(err) != codes.NotFound {
		t.Fatalf("DeletePool from another tenant: %v, want NotFound", err)
	}
	stream, err := h.client.RunInPool(h.ctx(), &v1.RunInPoolRequest{PoolId: p.GetId(), TenantId: other, Exec: &v1.ExecRequest{Command: "true", TimeoutSeconds: 1}})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("RunInPool from another tenant: %v, want NotFound", err)
	}
	if _, err := h.client.GetPool(h.ctx(), &v1.GetPoolRequest{PoolId: p.GetId()}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GetPool with no tenant: %v, want InvalidArgument", err)
	}
	// The same name in another tenant is a separate pool.
	spec := poolSpec(0, 1)
	spec.TenantId = other
	q, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: spec})
	if err != nil || q.GetId() == p.GetId() {
		t.Fatalf("CreatePool in another tenant = %v, %v", q, err)
	}
}

// vora-api records a pool's route before it asks for the pool, so the id it chose
// is the only handle on a pool whose create answer was lost.
func TestCreatePoolUsesTheCallersID(t *testing.T) {
	h := newWarmHarness(t)
	id := ulid.NewPool()
	p, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: poolSpec(0, 1), PoolId: id})
	if err != nil || p.GetId() != id {
		t.Fatalf("CreatePool = %v, %v, want id %s", p, err, id)
	}
	if got := h.get(id); got.GetId() != id {
		t.Fatalf("GetPool by the caller's id = %v", got)
	}

	// A repeat under another id finds the pool by name and reports its real id.
	again, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: poolSpec(0, 1), PoolId: ulid.NewPool()})
	if err != nil || again.GetId() != id {
		t.Fatalf("repeat = %v, %v, want the existing pool %s", again, err, id)
	}

	other := poolSpec(0, 1)
	other.Name = "other"
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: other, PoolId: id}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a second pool under a taken id: %v, want AlreadyExists", err)
	}
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: other, PoolId: "../../etc"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a pool id that is not one, which names the pool's record path: %v, want InvalidArgument", err)
	}
}

func TestCreatePoolIsIdempotentByNameAndReservesCapacity(t *testing.T) {
	h := newWarmHarness(t)
	h.s.cfg.MaxSandboxesPerTenant = 4
	p := h.create(poolSpec(2, 2))
	again := h.create(poolSpec(2, 2))
	if again.GetId() != p.GetId() {
		t.Fatalf("same spec declared twice gave %s and %s", p.GetId(), again.GetId())
	}
	// Spelling out a default is the same spec.
	spelled := poolSpec(2, 2)
	runs := uint32(100)
	spelled.MaxRuns = &runs
	if h.create(spelled).GetId() != p.GetId() {
		t.Fatal("a spec that spells out a default was not the same pool")
	}
	different := poolSpec(2, 2)
	runs = 7
	different.MaxRuns = &runs
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: different}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("different spec, same name: %v, want AlreadyExists", err)
	}
	h.waitIdle(p.GetId(), 2)

	// Two members are RUNNING records, but the pool's reservation of 2 is
	// what counts: plain creates fit until 2 + 2 = 4.
	plain := &v1.SandboxSpec{Image: "img", Backend: "fake", TenantId: h.tenant,
		Resources: &v1.ResourceLimits{CpuMillis: 100, MemoryBytes: 1 << 20, Pids: 4},
		Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE}}
	for i := 0; i < 2; i++ {
		if _, err := h.client.CreateSandbox(h.ctx(), &v1.CreateSandboxRequest{Spec: plain}); err != nil {
			t.Fatalf("plain create %d: %v", i, err)
		}
	}
	if _, err := h.client.CreateSandbox(h.ctx(), &v1.CreateSandboxRequest{Spec: plain}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("plain create past the cap: %v, want ResourceExhausted", err)
	}
	second := poolSpec(0, 1)
	second.Name = "second"
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: second}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("pool past the cap: %v, want ResourceExhausted", err)
	}
	// A max_size no tenant could ever hold is refused when declared.
	tooBig := poolSpec(0, 5)
	tooBig.Name = "big"
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: tooBig}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("max_size over the tenant cap: %v, want InvalidArgument", err)
	}
}

func TestUnsafeReuseNeedsTheRunnerGrant(t *testing.T) {
	h := newWarmHarness(t)
	spec := poolSpec(1, 1)
	spec.Reuse = v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: spec}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unsafe reuse without the grant: %v, want FailedPrecondition", err)
	}
	h.s.cfg.PoolUnsafeReuse = true
	p := h.create(spec)
	h.waitIdle(p.GetId(), 1)
	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].GetRun().GetReuse() != v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET || frames[3].GetRunDone().GetOutcome() != v1.PoolResetOutcome_POOL_RESET_SKIPPED {
		t.Fatalf("run = %v, done = %v; want the unsafe mode announced and no reset", frames[0].GetRun(), frames[3].GetRunDone())
	}
}

func TestBackendWithoutReusableHostsColdPoolsOnly(t *testing.T) {
	h := newPoolHarness(t, newPoolBackend(t))
	if _, err := h.client.CreatePool(h.ctx(), &v1.CreatePoolRequest{Spec: poolSpec(1, 1)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("warm pool on a backend without Reusable: %v, want FailedPrecondition", err)
	}
	p := h.create(poolSpec(0, 2))
	frames, err := h.run(h.ctx(), p.GetId(), 5)
	if err != nil {
		t.Fatal(err)
	}
	run, done := frames[0].GetRun(), frames[3].GetRunDone()
	if run.GetWarm() || done.GetOutcome() != v1.PoolResetOutcome_POOL_RESET_RETIRED || done.GetRetireCause() != "shrink" {
		t.Fatalf("cold run = %v, done = %v; want a cold start retired after its run", run, done)
	}
	waitFor(t, func() bool { return len(h.backend.live()) == 0 }, "the cold member to be destroyed")
}

func TestReaperHandsAnExpiredMemberToItsPool(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	recs, _ := h.s.store.List()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want the one member", len(recs))
	}
	member := recs[0]
	member.Spec.TTL = time.Nanosecond
	if err := h.s.store.Save(member); err != nil {
		t.Fatal(err)
	}
	if err := h.s.ReapExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The backend is gone before the record says so, so wait for the record itself.
	waitFor(t, func() bool {
		rec, err := h.s.store.Load(member.ID)
		return err == nil && rec.State.Terminal()
	}, "the pool to retire the expired member")
	rec, _ := h.s.store.Load(member.ID)
	if rec.State != sandbox.StateDestroyed {
		t.Fatalf("expired member is %s, want DESTROYED through its pool, not EXPIRED by the reaper", rec.State)
	}
	if !h.backend.isGone(member.ID) {
		t.Fatal("the record says DESTROYED, but the backend still has the sandbox")
	}
	waitFor(t, func() bool {
		st := h.get(p.GetId()).GetStats()
		return st.GetIdle() == 1 && st.GetDestroyedTotal() == 1
	}, "the pool to refill")

	// A pooled record no loaded pool owns is the reaper's to destroy.
	orphan := &sandbox.Record{ID: "sbx_01ARZ3NDEKTSV4RRFFQ69G5FAV", State: sandbox.StateRunning,
		Spec: runtime.SandboxSpec{ID: "sbx_01ARZ3NDEKTSV4RRFFQ69G5FAV", TenantID: h.tenant, PoolID: "pool_01ARZ3NDEKTSV4RRFFQ69G5FAV", TTL: time.Nanosecond}}
	if err := h.s.store.Create(orphan); err != nil {
		t.Fatal(err)
	}
	if err := h.s.ReapExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := h.s.store.Load(orphan.ID); rec.State != sandbox.StateDestroyed {
		t.Fatalf("orphan pooled record is %s, want DESTROYED", rec.State)
	}
}

func TestPoolMetricsCarryTheDashboardNames(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	if _, err := h.run(h.ctx(), p.GetId(), 5); err != nil {
		t.Fatal(err)
	}
	families, err := h.s.metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "vora_pool_") {
			continue
		}
		for _, m := range f.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			got[f.GetName()] = append(got[f.GetName()], strings.Join(labels, ","))
		}
	}
	// The names and labels vora-arc's dashboard queries.
	want := map[string]string{
		"vora_pool_runs_total":             "pool=tenant-a/py,reason=Exited,start=warm",
		"vora_pool_run_duration_seconds":   "pool=tenant-a/py,start=warm",
		"vora_pool_startup_seconds":        "pool=tenant-a/py,start=warm",
		"vora_pool_reset_duration_seconds": "pool=tenant-a/py,result=ok",
		"vora_pool_sandboxes":              "pool=tenant-a/py,state=idle",
		"vora_pool_exhausted_total":        "pool=tenant-a/py",
		"vora_pool_start_failed_total":     "pool=tenant-a/py",
	}
	for name, labels := range want {
		if !slices.Contains(got[name], labels) {
			t.Errorf("%s{%s} not exported; have %v", name, labels, got[name])
		}
	}
	// vora_pool_retired_total is read from the pool's books too, so it has a
	// series once a member has been retired.
	h.s.cfg.PoolUnsafeReuse = true
	single := poolSpec(1, 1)
	single.Name = "single"
	runs := uint32(1)
	single.MaxRuns = &runs
	q := h.create(single)
	h.waitIdle(q.GetId(), 1)
	if _, err := h.run(h.ctx(), q.GetId(), 5); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		families, _ := h.s.metrics.Gather()
		for _, f := range families {
			if f.GetName() != "vora_pool_retired_total" {
				continue
			}
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "cause" && l.GetValue() == "max_runs" && m.GetCounter().GetValue() == 1 {
						return true
					}
				}
			}
		}
		return false
	}, "vora_pool_retired_total{cause=max_runs} to reach 1")
}

func TestRestartFailsOldMembersAndReloadsPools(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(2, 2))
	h.waitIdle(p.GetId(), 2)
	// A crash between two DeletePool calls leaves a draining record.
	going := h.s.pools.get(pool.ID(p.GetId())).spec.Raw()
	going.ID, going.Name = pool.ID(ulid.NewPool()), "going"
	if err := h.s.savePool(poolRecord{ID: going.ID, Spec: going, CreatedAt: time.Now(), Draining: true}); err != nil {
		t.Fatal(err)
	}
	unsafe := poolSpec(1, 1)
	unsafe.Name = "unsafe"
	unsafe.Reuse = v1.PoolReuse_POOL_REUSE_UNSAFE_NO_RESET
	h.s.cfg.PoolUnsafeReuse = true
	granted := h.create(unsafe)
	// Close on a member mid-start would undo it, and only a settled one is old.
	h.waitIdle(granted.GetId(), 1)
	oldIDs := h.backend.live()

	// The old process stops: pools close, nothing is destroyed.
	h.s.ClosePools()

	// A new process over the same state, whose backend still lists the old
	// sandboxes, without the unsafe grant.
	backend := newPoolBackend(t)
	backend.leftover = oldIDs
	reg := runtime.NewRegistry()
	if err := reg.Register(reusableBackend{backend}); err != nil {
		t.Fatal(err)
	}
	s2 := NewServer(h.s.cfg, sandbox.NewStore(h.s.cfg.SandboxesDir()), reg, "run_2", h.s.logger)
	s2.cfg.PoolUnsafeReuse = false
	tunePoolServer(t, s2)
	if _, err := s2.GetPool(context.Background(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: h.tenant}); status.Code(err) != codes.Unavailable {
		t.Fatalf("pool RPC before load: %v, want Unavailable", err)
	}
	if err := s2.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s2.LoadPools(); err != nil {
		t.Fatal(err)
	}
	for _, id := range oldIDs {
		rec, err := s2.store.Load(id)
		if err != nil || rec.State != sandbox.StateFailed {
			t.Fatalf("old member %s = %v (%v), want FAILED by reconcile", id, rec, err)
		}
		if !backend.isGone(id) {
			t.Fatalf("old member %s was not reaped from the backend", id)
		}
	}
	reloaded, err := s2.GetPool(context.Background(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: h.tenant})
	if err != nil {
		t.Fatalf("pool after restart: %v", err)
	}
	if reloaded.GetSpec().GetName() != "py" || reloaded.GetSpec().GetSize() != 2 {
		t.Fatalf("reloaded pool = %v", reloaded)
	}
	waitFor(t, func() bool {
		r, err := s2.GetPool(context.Background(), &v1.GetPoolRequest{PoolId: p.GetId(), TenantId: h.tenant})
		return err == nil && r.GetStats().GetIdle() == 2
	}, "the reloaded pool to refill")
	for _, id := range backend.live() {
		if slices.Contains(oldIDs, id) {
			t.Fatalf("new members include old id %s", id)
		}
	}
	if _, err := s2.GetPool(context.Background(), &v1.GetPoolRequest{PoolId: string(going.ID), TenantId: h.tenant}); status.Code(err) != codes.NotFound {
		t.Fatalf("pool that was draining at shutdown: %v, want NotFound", err)
	}
	// The definition outlives a boot that cannot honour it. The draining record
	// is the only one removed.
	lost, err := s2.GetPool(context.Background(), &v1.GetPoolRequest{PoolId: granted.GetId(), TenantId: h.tenant})
	if err != nil || lost.GetState() != v1.PoolState_POOL_NOT_LOADED {
		t.Fatalf("unsafe pool on a runner that lost the grant: %v, %v; want POOL_NOT_LOADED", lost, err)
	}
	recs, _ := s2.loadPoolRecords()
	if len(recs) != 2 {
		t.Fatalf("pool records after load = %d, want the reloaded pool and the one not loaded", len(recs))
	}
}
