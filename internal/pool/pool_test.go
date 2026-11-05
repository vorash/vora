package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

func TestFateTable(t *testing.T) {
	spec := mustParse(t, rawSpec(2, 4), testLimits)
	unsafe := mustParse(t, func() RawSpec { r := rawSpec(2, 4); r.Reuse = ReuseUnsafeNoReset; return r }(),
		Limits{AllowUnsafeReuse: true})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := func() *member { return &member{runs: 1, createdAt: now} }

	cases := []struct {
		name     string
		spec     Spec
		m        *member
		live     int
		target   int
		draining bool
		want     Fate
	}{
		{"draining wins over everything", spec, &member{runs: 100, retireWhen: RetireExpired}, 9, 2, true, Fate{Retire: RetireDrain}},
		{"reaper mark", spec, &member{runs: 1, createdAt: now, retireWhen: RetireExpired}, 2, 2, false, Fate{Retire: RetireExpired}},
		{"max runs", spec, &member{runs: 100, createdAt: now}, 2, 2, false, Fate{Retire: RetireMaxRuns}},
		{"max age", spec, &member{runs: 1, createdAt: now.Add(-30 * time.Minute)}, 2, 2, false, Fate{Retire: RetireMaxAge}},
		{"more live than target", spec, fresh(), 3, 2, false, Fate{Retire: RetireShrink}},
		{"size 0 pool is always cold", spec, fresh(), 1, 0, false, Fate{Retire: RetireShrink}},
		{"unsafe reuse goes idle uncleaned", unsafe, fresh(), 2, 2, false, Fate{}},
		{"otherwise reset", spec, fresh(), 2, 2, false, Fate{Reset: true}},
		{"a run's outcome is not an input", spec, fresh(), 2, 2, false, Fate{Reset: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fate(c.spec, c.m, now, c.live, c.target, c.draining); got != c.want {
				t.Fatalf("fate = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestMemberStateMachine(t *testing.T) {
	allowed := map[memberState][]memberState{
		starting:  {idle, busy, retiring},
		idle:      {busy, retiring},
		busy:      {resetting, idle, retiring},
		resetting: {idle, retiring},
		retiring:  {},
	}
	all := []memberState{starting, idle, busy, resetting, retiring}
	for _, from := range all {
		for _, to := range all {
			want := false
			for _, a := range allowed[from] {
				want = want || a == to
			}
			if got := canTransition(from, to); got != want {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestParseDefaultsAndDerivedMemberSpec(t *testing.T) {
	raw := rawSpec(0, 0)
	raw.Size, raw.MaxSize, raw.BorrowTimeout = nil, nil, 0
	spec := mustParse(t, raw, testLimits)
	if spec.Size() != 8 || spec.MaxSize() != 8 || spec.MaxRuns() != 100 || spec.MaxRun() != 5*time.Minute {
		t.Fatalf("defaults: size %d max_size %d max_runs %d max_run %s", spec.Size(), spec.MaxSize(), spec.MaxRuns(), spec.MaxRun())
	}
	if spec.borrowTimeout != 2*time.Second || spec.maxAge != 30*time.Minute || spec.idleExpiry != time.Hour {
		t.Fatalf("defaults: borrow %s max_age %s idle_expiry %s", spec.borrowTimeout, spec.maxAge, spec.idleExpiry)
	}
	if spec.Reuse() != ReuseReset {
		t.Fatalf("zero reuse parsed as %v, want reset", spec.Reuse())
	}
	m := spec.MemberSpec()
	if m.TenantID != "t1" || m.AgentID != "a1" || m.PoolID != "pool_1" || m.ID != "" {
		t.Fatalf("member spec tenancy: %+v", m)
	}
	if !m.DenyIPC {
		t.Fatal("a reused member must deny IPC")
	}
	if m.TTL != spec.MemberTTL() || m.TTL != 36*time.Minute {
		t.Fatalf("member ttl %s", m.TTL)
	}
	if spec.Raw().Name != "py" {
		t.Fatal("Raw must return the accepted definition")
	}

	cold := mustParse(t, rawSpec(0, 4), Limits{})
	if cold.MemberSpec().DenyIPC {
		t.Fatal("a single-use member keeps IPC")
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RawSpec)
		limits Limits
		want   error
	}{
		{"unsafe without the runner's grant", func(r *RawSpec) { r.Reuse = ReuseUnsafeNoReset }, testLimits, ErrUnsafeRefused},
		{"resetting pool on a backend without Reusable", nil, Limits{Reusable: func(string) bool { return false }}, ErrNotReusable},
		{"pids below the agent's own tasks", func(r *RawSpec) { r.Template.Resources.Pids = 10 }, testLimits, ErrInvalidSpec},
		{"template names an id", func(r *RawSpec) { r.Template.ID = "sbx_x" }, testLimits, ErrInvalidSpec},
		{"template names a tenant", func(r *RawSpec) { r.Template.TenantID = "other" }, testLimits, ErrInvalidSpec},
		{"template names a ttl", func(r *RawSpec) { r.Template.TTL = time.Hour }, testLimits, ErrInvalidSpec},
		{"template without a backend", func(r *RawSpec) { r.Template.Backend = "" }, testLimits, ErrInvalidSpec},
		{"networked template", func(r *RawSpec) { r.Template.Network.Mode = runtime.NetworkRestricted }, testLimits, ErrInvalidSpec},
		{"template without an image", func(r *RawSpec) { r.Template.Image = "" }, testLimits, ErrInvalidSpec},
		{"size above max_size", func(r *RawSpec) { *r.Size = 5 }, testLimits, ErrInvalidSpec},
		{"max_size 0", func(r *RawSpec) { *r.MaxSize = 0 }, testLimits, ErrInvalidSpec},
		{"member ttl over the runner cap", nil, Limits{MaxTTL: 10 * time.Minute, Reusable: testLimits.Reusable}, ErrInvalidSpec},
		{"bad name", func(r *RawSpec) { r.Name = "Py Pool" }, testLimits, ErrInvalidSpec},
		{"no tenant", func(r *RawSpec) { r.TenantID = "" }, testLimits, ErrInvalidSpec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := rawSpec(2, 4)
			if c.mutate != nil {
				c.mutate(&raw)
			}
			_, err := Parse(raw, c.limits)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if _, err := Parse(rawSpec(0, 4), Limits{Reusable: func(string) bool { return false }}); err != nil {
		t.Fatalf("a cold pool needs no Reusable: %v", err)
	}
}

func TestWarmRunResetsAndReturnsMember(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	ctx := context.Background()

	first, done, err := h.run(ctx, nil)
	if err != nil || !first.Warm || first.SandboxRun != 1 || done.Reset != ResetOK {
		t.Fatalf("first run: %+v %+v %v", first, done, err)
	}
	h.waitIdle(2)
	// FIFO: the second run takes the other member, the third gets the first back.
	second, _, _ := h.run(ctx, nil)
	third, _, _ := h.run(ctx, nil)
	if second.SandboxID == first.SandboxID || third.SandboxID != first.SandboxID || third.SandboxRun != 2 {
		t.Fatalf("rotation: %s %s %s run %d", first.SandboxID, second.SandboxID, third.SandboxID, third.SandboxRun)
	}
	if started, destroyed := h.fake.counts(); started != 2 || destroyed != 0 {
		t.Fatalf("started %d destroyed %d, want 2 and 0", started, destroyed)
	}
}

func TestFailedResetNeverReturnsToIdle(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.fake.set(func(f *fakeSandboxes) {
		f.resetHook = func(_ context.Context, id string) error { return errBoom }
	})
	pl, done, err := h.run(context.Background(), nil)
	if err != nil || done.Reset != ResetFailed || done.Cause != RetireResetFailed {
		t.Fatalf("done %+v err %v", done, err)
	}
	waitFor(t, func() bool { return !h.fake.isLive(pl.SandboxID) }, "the unproven member to be destroyed")
	h.fake.set(func(f *fakeSandboxes) { f.resetHook = nil })
	h.waitIdle(1)
	next, _, _ := h.run(context.Background(), nil)
	if next.SandboxID == pl.SandboxID {
		t.Fatal("the member whose reset failed was lent again")
	}
	if s := h.pool.Stats(); s.ResetFailuresTotal != 1 || s.Retired[RetireResetFailed] != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestResetDeadlineDestroysHungMember(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{ResetTimeout: 20 * time.Millisecond})
	h.waitIdle(1)
	h.fake.set(func(f *fakeSandboxes) {
		f.resetHook = func(ctx context.Context, id string) error { <-ctx.Done(); return ctx.Err() }
	})
	began := time.Now()
	pl, done, err := h.run(context.Background(), nil)
	if err != nil || done.Reset != ResetFailed {
		t.Fatalf("done %+v err %v", done, err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("a hung reset held the run for %s", took)
	}
	waitFor(t, func() bool { return !h.fake.isLive(pl.SandboxID) }, "the hung member to be destroyed")
}

func TestRefillAfterDestroyAndMaxRuns(t *testing.T) {
	raw := rawSpec(2, 2)
	raw.MaxRuns = 3
	h := newHarness(t, raw, Options{})
	h.waitIdle(2)
	ids := map[string]int{}
	for i := 0; i < 6; i++ {
		pl, done, err := h.run(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[pl.SandboxID]++
		if pl.SandboxRun == 3 && (done.Reset != ResetRetired || done.Cause != RetireMaxRuns) {
			t.Fatalf("third run on %s: %+v", pl.SandboxID, done)
		}
		h.waitIdle(2)
	}
	for id, n := range ids {
		if n > 3 {
			t.Fatalf("%s served %d runs, max 3", id, n)
		}
	}
	if started, destroyed := h.fake.counts(); started != 4 || destroyed != 2 {
		t.Fatalf("started %d destroyed %d, want 4 and 2", started, destroyed)
	}
}

func TestBorrowWaitsThenTimesOut(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	hold := make(chan struct{})
	running := make(chan struct{})
	h.hold(func(ctx context.Context, pl Placement) error {
		close(running)
		<-hold
		return nil
	})
	<-running
	began := time.Now()
	_, _, err := h.run(context.Background(), nil)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if took := time.Since(began); took < 50*time.Millisecond {
		t.Fatalf("gave up after %s, before the borrow timeout", took)
	}
	if h.pool.Stats().ExhaustedTotal != 1 {
		t.Fatal("exhausted not counted")
	}
	close(hold)
}

func TestWaiterIsHandedTheFreedMember(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	hold := make(chan struct{})
	running := make(chan struct{})
	h.hold(func(ctx context.Context, pl Placement) error {
		close(running)
		<-hold
		return nil
	})
	<-running
	got := make(chan Placement, 1)
	go func() {
		pl, _, err := h.run(context.Background(), nil)
		if err != nil {
			t.Error(err)
		}
		got <- pl
	}()
	waitFor(t, func() bool { h.pool.mu.Lock(); defer h.pool.mu.Unlock(); return len(h.pool.waiters) == 1 }, "the waiter to queue")
	close(hold)
	pl := <-got
	if !pl.Warm || pl.SandboxRun != 2 {
		t.Fatalf("the waiter got %+v, want the reset member", pl)
	}
}

func TestDrainWhileRunsInFlight(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	release := make(chan struct{})
	inReset := make(chan struct{}, 1)
	h.fake.set(func(f *fakeSandboxes) {
		f.resetHook = func(ctx context.Context, id string) error {
			inReset <- struct{}{}
			<-release
			return nil
		}
	})
	finished := make(chan Done, 1)
	go func() {
		_, done, _ := h.run(context.Background(), nil)
		finished <- done
	}()
	<-inReset

	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if h.pool.Drain(short) {
		t.Fatal("drained while a reset was in flight")
	}
	if _, _, err := h.run(context.Background(), nil); !errors.Is(err, ErrDraining) {
		t.Fatalf("a run during drain got %v", err)
	}
	close(release)
	done := <-finished
	if done.Reset != ResetOK || done.Cause != RetireDrain {
		t.Fatalf("the in-flight run's trailer %+v, want ok then retired by drain", done)
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	first := h.pool.Drain(ctx)
	again := h.pool.Drain(ctx)
	if !first || !again {
		t.Fatalf("drain converged %v, repeated %v; DeletePool must be idempotent", first, again)
	}
	if n := h.fake.liveCount(); n != 0 {
		t.Fatalf("%d sandboxes survive the drain", n)
	}
}

func TestIdleMemberThatDiedIsRetired(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	var dead string
	h.fake.set(func(f *fakeSandboxes) {
		dead = "sbx_1"
		f.aliveHook = func(id string) error {
			if id == dead {
				return errBoom
			}
			return nil
		}
	})
	h.clock.Advance(idleProbeAfter)
	pl, _, err := h.run(context.Background(), nil)
	if err != nil || pl.SandboxID == dead {
		t.Fatalf("got %s err %v, want the live member", pl.SandboxID, err)
	}
	waitFor(t, func() bool { return !h.fake.isLive(dead) }, "the dead member to be destroyed")
	if h.pool.Stats().Retired[RetireUnhealthy] != 1 {
		t.Fatal("not counted as unhealthy")
	}
}

func TestBorrowerCancelledWhileWaiting(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	hold := make(chan struct{})
	running := make(chan struct{})
	h.hold(func(ctx context.Context, pl Placement) error {
		close(running)
		<-hold
		return nil
	})
	<-running
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, _, err := h.run(ctx, nil); errc <- err }()
	waitFor(t, func() bool { h.pool.mu.Lock(); defer h.pool.mu.Unlock(); return len(h.pool.waiters) == 1 }, "the waiter to queue")
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	close(hold)
	// The freed member is not lost on the cancelled waiter.
	h.waitIdle(1)
	if h.pool.Stats().RunsTotal != 1 {
		t.Fatalf("runs %d, the cancelled borrow must not count", h.pool.Stats().RunsTotal)
	}
}

func TestCancelledRunStillResets(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	ctx, cancel := context.WithCancel(context.Background())
	resets := 0
	h.fake.set(func(f *fakeSandboxes) {
		f.resetHook = func(ctx context.Context, id string) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			resets++
			return nil
		}
	})
	_, done, err := h.run(ctx, func(ctx context.Context, pl Placement) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || done.Reset != ResetOK {
		t.Fatalf("err %v done %+v", err, done)
	}
	if resets != 1 {
		t.Fatal("the reset ran on the client's cancelled context")
	}
	h.waitIdle(1)
}

func TestExecNotStartedIsRetriedOnceOnAnotherMember(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	var seen []string
	pl, done, err := h.run(context.Background(), func(ctx context.Context, pl Placement) error {
		seen = append(seen, pl.SandboxID)
		if len(seen) == 1 {
			return errors.New("dead agent: " + ErrNotStarted.Error())
		}
		return nil
	})
	if err == nil || len(seen) != 1 {
		t.Fatalf("an error that does not wrap ErrNotStarted must not retry: seen %v err %v", seen, err)
	}
	h.waitIdle(2)

	seen = nil
	pl, done, err = h.run(context.Background(), func(ctx context.Context, pl Placement) error {
		seen = append(seen, pl.SandboxID)
		if len(seen) == 1 {
			return errors.Join(ErrNotStarted, errBoom)
		}
		return nil
	})
	if err != nil || len(seen) != 2 || seen[0] == seen[1] || pl.SandboxID != seen[1] || done.Reset != ResetOK {
		t.Fatalf("seen %v pl %+v done %+v err %v", seen, pl, done, err)
	}
	waitFor(t, func() bool { return !h.fake.isLive(seen[0]) }, "the member that could not exec to be destroyed")

	h.waitIdle(2)
	seen = nil
	_, done, err = h.run(context.Background(), func(ctx context.Context, pl Placement) error {
		seen = append(seen, pl.SandboxID)
		return ErrNotStarted
	})
	if !errors.Is(err, ErrNotStarted) || len(seen) != 2 || done.Reset != ResetRetired {
		t.Fatalf("second failure must surface after one retry: seen %v done %+v err %v", seen, done, err)
	}
}

func TestColdPoolStartsEveryRunFresh(t *testing.T) {
	h := newHarness(t, rawSpec(0, 2), Options{})
	time.Sleep(5 * time.Millisecond)
	if n := h.fake.liveCount(); n != 0 {
		t.Fatalf("a size-0 pool pre-started %d members", n)
	}
	for i := 0; i < 3; i++ {
		pl, done, err := h.run(context.Background(), nil)
		if err != nil || pl.Warm || pl.SandboxRun != 1 || done.Reset != ResetRetired || done.Cause != RetireShrink {
			t.Fatalf("run %d: %+v %+v %v", i, pl, done, err)
		}
	}
	waitFor(t, func() bool { return h.fake.liveCount() == 0 }, "cold members to be destroyed")
	if s := h.pool.Stats(); s.WarmRunsTotal != 0 || s.RunsTotal != 3 {
		t.Fatalf("stats %+v", s)
	}
}

func TestColdStartFailureIsReported(t *testing.T) {
	h := newHarness(t, rawSpec(0, 2), Options{})
	h.fake.set(func(f *fakeSandboxes) { f.startErr = func() error { return errBoom } })
	_, _, err := h.run(context.Background(), nil)
	if !errors.Is(err, ErrColdStart) || !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if h.pool.Stats().LastStartError == "" {
		t.Fatal("start error not reported in stats")
	}
}

func TestUnsafePoolSkipsReset(t *testing.T) {
	raw := rawSpec(1, 1)
	raw.Reuse = ReuseUnsafeNoReset
	spec := mustParse(t, raw, Limits{AllowUnsafeReuse: true})
	if !spec.MemberSpec().DenyIPC {
		t.Fatal("the unsafe arm must differ from the reset arm only in the reset")
	}
	fake := newFake(t, 1)
	fake.resetHook = func(context.Context, string) error { t.Error("reset called on an unsafe pool"); return nil }
	p := New(spec, fake, Options{StartBackoff: time.Millisecond})
	t.Cleanup(p.Close)
	waitFor(t, func() bool { return p.Stats().Idle == 1 }, "member")
	for i := 1; i <= 2; i++ {
		var pl Placement
		done, err := p.Run(context.Background(), func(_ context.Context, got Placement) error { pl = got; return nil })
		if err != nil || pl.Reuse != ReuseUnsafeNoReset || pl.SandboxRun != i || done.Reset != ResetSkipped {
			t.Fatalf("%+v %+v %v", pl, done, err)
		}
	}
}

func TestIdleExpiryDrainsToZeroAndWakesOnRun(t *testing.T) {
	raw := rawSpec(2, 2)
	raw.MaxAge = 3 * time.Hour
	h := newHarness(t, raw, Options{})
	h.waitIdle(2)
	h.clock.Advance(time.Hour)
	h.pool.Sweep()
	waitFor(t, func() bool { return h.fake.liveCount() == 0 }, "idle members to expire")
	h.pool.Sweep()
	if s := h.pool.Stats(); !s.Dormant || s.Starting != 0 || s.Retired[RetireIdleExpiry] != 2 {
		t.Fatalf("a dormant pool must not refill: %+v", s)
	}
	pl, done, err := h.run(context.Background(), nil)
	if err != nil || done.Reset != ResetOK {
		t.Fatalf("the waking run: %+v %+v %v", pl, done, err)
	}
	if h.pool.Stats().Dormant {
		t.Fatal("a run must wake the pool")
	}
	h.waitIdle(2)
}

func TestSweepRetiresIdleMembersPastMaxAge(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.clock.Advance(30 * time.Minute)
	h.pool.Sweep()
	waitFor(t, func() bool { s := h.pool.Stats(); return s.Retired[RetireMaxAge] == 1 && s.DestroyedTotal == 1 }, "max_age retire")
	h.waitIdle(1)
	if started, _ := h.fake.counts(); started != 2 {
		t.Fatalf("started %d, want a replacement", started)
	}
}

func TestFailedDestroyKeepsTheSlotUntilSweepRetries(t *testing.T) {
	raw := rawSpec(1, 1)
	raw.MaxRuns = 1
	h := newHarness(t, raw, Options{})
	h.waitIdle(1)
	h.fake.set(func(f *fakeSandboxes) { f.destroyErr = func(string) error { return errBoom } })
	if _, done, _ := h.run(context.Background(), nil); done.Cause != RetireMaxRuns {
		t.Fatalf("%+v", done)
	}
	waitFor(t, func() bool {
		h.pool.mu.Lock()
		defer h.pool.mu.Unlock()
		for m := range h.pool.members {
			if m.destroyErr != nil {
				return true
			}
		}
		return false
	}, "the destroy to fail")
	if s := h.pool.Stats(); s.Retiring != 1 || s.Starting != 0 || s.Idle != 0 {
		t.Fatalf("the slot must stay taken: %+v", s)
	}
	h.fake.set(func(f *fakeSandboxes) { f.destroyErr = nil })
	h.pool.Sweep()
	h.waitIdle(1)
	if _, destroyed := h.fake.counts(); destroyed != 1 {
		t.Fatalf("destroyed %d", destroyed)
	}
}

func TestRetireExpiredHandsOff(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	hold := make(chan struct{})
	running := make(chan Placement, 1)
	finished := make(chan Done, 1)
	go func() {
		_, done, _ := h.run(context.Background(), func(ctx context.Context, pl Placement) error {
			running <- pl
			<-hold
			return nil
		})
		finished <- done
	}()
	busy := <-running
	h.pool.RetireExpired(busy.SandboxID)
	if h.fake.isLive(busy.SandboxID) == false {
		t.Fatal("a busy member was destroyed under a run")
	}
	h.pool.RetireExpired("sbx_" + map[string]string{"sbx_1": "2", "sbx_2": "1"}[busy.SandboxID])
	waitFor(t, func() bool { return h.pool.Stats().Retired[RetireExpired] == 1 }, "the idle member to retire now")
	close(hold)
	if done := <-finished; done.Reset != ResetRetired || done.Cause != RetireExpired {
		t.Fatalf("busy member's trailer %+v, want retired at check-in", done)
	}
	h.pool.RetireExpired("sbx_nope")
}

func TestStartFailuresBackOffAndRecover(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{StartBackoff: time.Millisecond, MaxStartBackoff: 2 * time.Millisecond})
	h.fake.set(func(f *fakeSandboxes) { f.startErr = func() error { return errBoom } })
	waitFor(t, func() bool { return h.pool.Stats().StartFailedTotal >= 3 }, "retries")
	h.fake.set(func(f *fakeSandboxes) { f.startErr = nil })
	h.waitIdle(1)
}
