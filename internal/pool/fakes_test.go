package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// fakeSandboxes stands in for the runner. It checks, from its own books and
// not from the pool's, the rules the pool must keep: a sandbox is destroyed
// at most once, a destroyed or reset-failed sandbox never runs again, one
// sandbox never serves two runs at once, a sandbox a run touched never runs
// again before a nil Reset, and live sandboxes never exceed the bound the test
// sets.
type fakeSandboxes struct {
	t       testing.TB
	mu      sync.Mutex
	next    int
	live    map[string]*fakeSandbox
	gone    map[string]bool
	maxLive int
	events  []string
	// unsafe declares a pool that reuses members uncleaned, the only pool
	// allowed to run on a dirty sandbox.
	unsafe bool
	// noResettable makes Start lose the Resettable it was asked for.
	noResettable bool

	started, destroyed      int
	resetsOK, resetsFailed  int
	aliveCalls, destroyErrs int

	startErr   func() error
	startBlock chan struct{}
	resetHook  func(ctx context.Context, id string) error
	aliveHook  func(ctx context.Context, id string) error
	destroyErr func(id string) error
	destroyBlk chan struct{}
}

type fakeSandbox struct {
	inUse       bool
	resetFailed bool
	prepared    bool
	dirty       bool // a run touched it since its last nil Reset
	resetCtx    context.Context
}

func newFake(t testing.TB, maxLive int) *fakeSandboxes {
	return &fakeSandboxes{t: t, live: map[string]*fakeSandbox{}, gone: map[string]bool{}, maxLive: maxLive}
}

func (f *fakeSandboxes) Start(ctx context.Context, spec runtime.SandboxSpec, prepare bool) (Started, error) {
	f.mu.Lock()
	block, startErr := f.startBlock, f.startErr
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return Started{}, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return Started{}, ctx.Err()
	}
	if startErr != nil {
		if err := startErr(); err != nil {
			return Started{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("sbx_%d", f.next)
	f.live[id] = &fakeSandbox{prepared: prepare}
	f.started++
	if len(f.live) > f.maxLive {
		f.t.Errorf("invariant: %d live sandboxes, bound %d", len(f.live), f.maxLive)
	}
	s := Started{SandboxID: id}
	if prepare && !f.noResettable {
		s.Reset = &fakeResettable{f: f, id: id}
	}
	return s, nil
}

func (f *fakeSandboxes) Destroy(ctx context.Context, id string) error {
	f.mu.Lock()
	blk := f.destroyBlk
	f.mu.Unlock()
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	hook := f.destroyErr
	f.mu.Unlock()
	if hook != nil {
		if err := hook(id); err != nil {
			f.mu.Lock()
			f.destroyErrs++
			f.mu.Unlock()
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[id] {
		f.t.Errorf("invariant: %s destroyed twice", id)
	}
	if s := f.live[id]; s != nil {
		if s.inUse {
			f.t.Errorf("invariant: %s destroyed while a run uses it", id)
		}
		if s.resetCtx != nil && s.resetCtx.Err() == nil {
			f.t.Errorf("invariant: %s destroyed during a reset inside its deadline", id)
		}
	}
	delete(f.live, id)
	f.gone[id] = true
	f.destroyed++
	return nil
}

func (f *fakeSandboxes) Event(id, kind, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, id+" "+kind+": "+detail)
}

type fakeResettable struct {
	f  *fakeSandboxes
	id string
}

// Reset is the only thing that clears a sandbox's dirty mark, and only by
// returning nil inside its deadline. A nil after the deadline counts as a
// failure in these books: the pool must have stopped believing it by then.
func (r *fakeResettable) Reset(ctx context.Context) (runtime.ResetReport, error) {
	f := r.f
	f.mu.Lock()
	s := f.live[r.id]
	hook := f.resetHook
	if s == nil {
		f.mu.Unlock()
		f.t.Errorf("invariant: reset of %s, which is not live", r.id)
		return runtime.ResetReport{}, runtime.ErrUnproven
	}
	if s.inUse {
		f.t.Errorf("invariant: reset of %s while a run uses it", r.id)
	}
	if s.resetCtx != nil {
		f.t.Errorf("invariant: %s reset twice at once", r.id)
	}
	s.resetCtx = ctx
	f.mu.Unlock()

	var err error
	if hook != nil {
		err = hook(ctx, r.id)
	} else {
		err = ctx.Err()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	s.resetCtx = nil
	if err != nil || ctx.Err() != nil {
		s.resetFailed = true
		f.resetsFailed++
	} else {
		s.dirty = false
		f.resetsOK++
	}
	if err != nil {
		return runtime.ResetReport{}, fmt.Errorf("%w: %w", runtime.ErrUnproven, err)
	}
	return runtime.ResetReport{ProcessesKilled: 1}, nil
}

// Alive honours ctx the way a gRPC call does.
func (r *fakeResettable) Alive(ctx context.Context) error {
	r.f.mu.Lock()
	hook := r.f.aliveHook
	r.f.aliveCalls++
	r.f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, r.id); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// use is what a test's RunFunc calls on entry. It is the fake's view of a
// run, so a double borrow or a run on a dead or unproven sandbox fails here
// whatever the pool believes.
func (f *fakeSandboxes) use(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.live[id]
	switch {
	case s == nil:
		f.t.Errorf("invariant: run on %s, which is not live", id)
		return
	case s.inUse:
		f.t.Errorf("invariant: %s borrowed twice at once", id)
	case s.resetFailed:
		f.t.Errorf("invariant: %s runs again after a failed reset", id)
	case s.resetCtx != nil:
		f.t.Errorf("invariant: %s runs during its reset", id)
	case s.dirty && !f.unsafe:
		f.t.Errorf("invariant: %s runs again with no nil Reset since its last run", id)
	}
	s.inUse = true
	s.dirty = true
}

func (f *fakeSandboxes) release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.live[id]; s != nil {
		s.inUse = false
	}
}

func (f *fakeSandboxes) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

func (f *fakeSandboxes) isLive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.live[id]
	return ok
}

func (f *fakeSandboxes) counts() (started, destroyed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.destroyed
}

func (f *fakeSandboxes) resets() (ok, failed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resetsOK, f.resetsFailed
}

func (f *fakeSandboxes) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aliveCalls
}

func (f *fakeSandboxes) set(fn func(f *fakeSandboxes)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeClock is a settable time source. After lets a test see every delay the
// pool asks for and decide when it passes.
type fakeClock struct {
	mu      sync.Mutex
	t       time.Time
	asked   []time.Duration
	pending []clockTimer
}

type clockTimer struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	var kept []clockTimer
	for _, timer := range c.pending {
		if timer.at.After(c.t) {
			kept = append(kept, timer)
			continue
		}
		timer.ch <- c.t
	}
	c.pending = kept
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.asked = append(c.asked, d)
	c.pending = append(c.pending, clockTimer{at: c.t.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.asked...)
}

// rawSpec's borrow timeout is long, so a slow runner cannot time a waiter out
// by accident. A test about the timeout sets its own.
func rawSpec(size, maxSize int) RawSpec {
	return RawSpec{
		ID: "pool_1", Name: "py", TenantID: "t1", AgentID: "a1",
		Template: runtime.SandboxSpec{
			Image:     "img",
			Backend:   "fake",
			Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 1 << 20, Pids: 16},
			Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		},
		Size: &size, MaxSize: &maxSize,
		BorrowTimeout: time.Minute,
	}
}

var testLimits = Limits{Reusable: func(string) bool { return true }}

var unsafeLimits = Limits{AllowUnsafeReuse: true, Reusable: testLimits.Reusable}

func mustParse(t testing.TB, raw RawSpec, limits Limits) Spec {
	t.Helper()
	spec, err := Parse(raw, limits)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

type harness struct {
	t     *testing.T
	fake  *fakeSandboxes
	clock *fakeClock
	pool  *Pool
}

// newHarness builds a pool over a fake. setup runs before the pool exists, so
// the very first refill already sees it. Real-time deadlines default to far
// beyond any test: a test about a deadline sets its own.
func newHarness(t *testing.T, raw RawSpec, opts Options, setup ...func(*fakeSandboxes)) *harness {
	t.Helper()
	return newHarnessOn(t, newClock(), raw, opts, setup...)
}

// newClockedHarness also puts the pool's backoff and retry timers on the fake
// clock, so a delay passes only when the test advances it.
func newClockedHarness(t *testing.T, raw RawSpec, opts Options, setup ...func(*fakeSandboxes)) *harness {
	t.Helper()
	clock := newClock()
	opts.After = clock.After
	return newHarnessOn(t, clock, raw, opts, setup...)
}

func newHarnessOn(t *testing.T, clock *fakeClock, raw RawSpec, opts Options, setup ...func(*fakeSandboxes)) *harness {
	t.Helper()
	spec := mustParse(t, raw, testLimits)
	h := &harness{t: t, fake: newFake(t, spec.MaxSize()), clock: clock}
	for _, fn := range setup {
		fn(h.fake)
	}
	opts.Now = h.clock.Now
	if opts.ResetTimeout == 0 {
		opts.ResetTimeout = time.Minute
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = time.Minute
	}
	if opts.StartBackoff == 0 {
		opts.StartBackoff = time.Millisecond
	}
	if opts.DestroyRetry == 0 {
		opts.DestroyRetry = time.Millisecond
	}
	h.pool = New(spec, h.fake, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.pool.Drain(ctx)
		h.pool.Close()
	})
	return h
}

// run borrows a member through the pool and records the run in the fake.
// Each returned Placement is the member the run saw.
func (h *harness) run(ctx context.Context, body func(ctx context.Context, pl Placement) error) (Placement, Done, error) {
	var got Placement
	done, err := h.pool.Run(ctx, func(ctx context.Context, pl Placement) error {
		got = pl
		h.fake.use(pl.SandboxID)
		defer h.fake.release(pl.SandboxID)
		if body != nil {
			return body(ctx, pl)
		}
		return nil
	})
	return got, done, err
}

// hold starts a run in the background and discards its result. The body
// decides when the member comes back.
func (h *harness) hold(body func(ctx context.Context, pl Placement) error) {
	go func() { _, _, _ = h.run(context.Background(), body) }()
}

// holdMember keeps a warm member busy until the returned func is called.
func (h *harness) holdMember() (release func()) {
	h.t.Helper()
	hold := make(chan struct{})
	running := make(chan struct{})
	h.hold(func(ctx context.Context, pl Placement) error {
		close(running)
		<-hold
		return nil
	})
	receive(h.t, running, "the held run to begin")
	return sync.OnceFunc(func() { close(hold) })
}

func (h *harness) waitIdle(n int) {
	h.t.Helper()
	waitFor(h.t, func() bool { return h.pool.Stats().Idle == n }, "idle members to reach %d", n)
}

func (h *harness) waitWaiters(n int) {
	h.t.Helper()
	waitFor(h.t, func() bool {
		h.pool.mu.Lock()
		defer h.pool.mu.Unlock()
		return len(h.pool.waiters) == n
	}, "%d borrowers to queue", n)
}

// patience bounds every wait in these tests. It is long because passing never
// reaches it, and a loaded runner must not fail a correct pool.
const patience = 30 * time.Second

func waitFor(t testing.TB, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for "+format, args...)
}

// receive turns a hang into a failure.
func receive[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(patience):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

var errBoom = errors.New("boom")
