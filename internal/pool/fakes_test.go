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
// sandbox never serves two runs at once, and live sandboxes never exceed the
// bound the test sets.
type fakeSandboxes struct {
	t       testing.TB
	mu      sync.Mutex
	next    int
	live    map[string]*fakeSandbox
	gone    map[string]bool
	maxLive int
	events  []string

	started, destroyed int

	startErr   func() error
	startBlock chan struct{}
	resetHook  func(ctx context.Context, id string) error
	aliveHook  func(id string) error
	destroyErr func(id string) error
	destroyBlk chan struct{}
}

type fakeSandbox struct {
	inUse       bool
	resetFailed bool
	prepared    bool
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
	if prepare {
		s.Reset = &fakeResettable{f: f, id: id}
	}
	return s, nil
}

func (f *fakeSandboxes) Destroy(ctx context.Context, id string) error {
	f.mu.Lock()
	blk, hook := f.destroyBlk, f.destroyErr
	f.mu.Unlock()
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if hook != nil {
		if err := hook(id); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[id] {
		f.t.Errorf("invariant: %s destroyed twice", id)
	}
	if s := f.live[id]; s != nil && s.inUse {
		f.t.Errorf("invariant: %s destroyed while a run uses it", id)
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

func (r *fakeResettable) Reset(ctx context.Context) (runtime.ResetReport, error) {
	r.f.mu.Lock()
	s := r.f.live[r.id]
	hook := r.f.resetHook
	if s == nil {
		r.f.t.Errorf("invariant: reset of %s, which is not live", r.id)
	} else if s.inUse {
		r.f.t.Errorf("invariant: reset of %s while a run uses it", r.id)
	}
	r.f.mu.Unlock()
	var err error
	if hook != nil {
		err = hook(ctx, r.id)
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		r.f.mu.Lock()
		if s != nil {
			s.resetFailed = true
		}
		r.f.mu.Unlock()
		return runtime.ResetReport{}, fmt.Errorf("%w: %w", runtime.ErrUnproven, err)
	}
	return runtime.ResetReport{ProcessesKilled: 1}, nil
}

func (r *fakeResettable) Alive(ctx context.Context) error {
	r.f.mu.Lock()
	hook := r.f.aliveHook
	r.f.mu.Unlock()
	if hook != nil {
		return hook(r.id)
	}
	return nil
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
	}
	s.inUse = true
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

func (f *fakeSandboxes) set(fn func(f *fakeSandboxes)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeClock is a settable time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
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
}

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
		BorrowTimeout: 50 * time.Millisecond,
	}
}

var testLimits = Limits{Reusable: func(string) bool { return true }}

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

func newHarness(t *testing.T, raw RawSpec, opts Options) *harness {
	t.Helper()
	spec := mustParse(t, raw, testLimits)
	h := &harness{t: t, fake: newFake(t, spec.MaxSize()), clock: newClock()}
	opts.Now = h.clock.Now
	if opts.ResetTimeout == 0 {
		opts.ResetTimeout = 200 * time.Millisecond
	}
	if opts.StartBackoff == 0 {
		opts.StartBackoff = time.Millisecond
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

func (h *harness) waitIdle(n int) {
	h.t.Helper()
	waitFor(h.t, func() bool { return h.pool.Stats().Idle == n }, "idle members to reach %d, stats %+v", n, h.pool.Stats())
}

func waitFor(t testing.TB, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for "+format, args...)
}

var errBoom = errors.New("boom")
