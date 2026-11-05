package pool

import (
	"context"
	"errors"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestFateTable(t *testing.T) {
	spec := mustParse(t, rawSpec(2, 4), testLimits)
	unsafe := mustParse(t, func() RawSpec { r := rawSpec(2, 4); r.Reuse = ReuseUnsafeNoReset; return r }(), unsafeLimits)
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
		{"unsafe reuse still ages out", unsafe, &member{runs: 100, createdAt: now}, 2, 2, false, Fate{Retire: RetireMaxRuns}},
		{"otherwise reset", spec, fresh(), 2, 2, false, Fate{Reset: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fate(c.spec, c.m, now, c.live, c.target, c.draining); got != c.want {
				t.Fatalf("fate = %+v, want %+v", got, c.want)
			}
		})
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

// The central safety rule, from the fake's books: use() fails any run on a
// sandbox with no nil Reset since its last run.
func TestEveryRunOnAMemberFollowsANilReset(t *testing.T) {
	const runs = 8
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	for i := 1; i <= runs; i++ {
		pl, done, err := h.run(context.Background(), nil)
		if err != nil || pl.SandboxRun != i || done.Reset != ResetOK {
			t.Fatalf("run %d: %+v %+v %v", i, pl, done, err)
		}
	}
	if ok, failed := h.fake.resets(); ok != runs || failed != 0 {
		t.Fatalf("the fake saw %d nil resets and %d failed, want %d and 0", ok, failed, runs)
	}
	if started, _ := h.fake.counts(); started != 1 {
		t.Fatalf("started %d sandboxes, the point is reuse", started)
	}
}

// A workload decides how its run ends, so no ending may let it skip the reset.
func TestEveryRunOutcomeIsFollowedByAReset(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	outcomes := []struct {
		name string
		body func(ctx context.Context, cancel func()) error
		want error
	}{
		{"success", func(context.Context, func()) error { return nil }, nil},
		{"failure", func(context.Context, func()) error { return errBoom }, errBoom},
		{"client gone", func(ctx context.Context, cancel func()) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}, context.Canceled},
	}
	for i, o := range outcomes {
		ctx, cancel := context.WithCancel(context.Background())
		_, done, err := h.run(ctx, func(ctx context.Context, _ Placement) error { return o.body(ctx, cancel) })
		cancel()
		if !errors.Is(err, o.want) || done.Reset != ResetOK {
			t.Fatalf("%s: err %v done %+v", o.name, err, done)
		}
		if ok, failed := h.fake.resets(); ok != i+1 || failed != 0 {
			t.Fatalf("%s: the fake saw %d nil resets and %d failed", o.name, ok, failed)
		}
	}
	h.waitIdle(1)
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
	type result struct {
		pl   Placement
		done Done
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		pl, done, err := h.run(context.Background(), nil)
		finished <- result{pl, done, err}
	}()
	r := receive(t, finished, "a hung reset to give up the run")
	if r.err != nil || r.done.Reset != ResetFailed {
		t.Fatalf("done %+v err %v", r.done, r.err)
	}
	waitFor(t, func() bool { return !h.fake.isLive(r.pl.SandboxID) }, "the hung member to be destroyed")
}

// A workload can SIGSTOP the agent, and a backend call may block. A Reset that
// ignores its context must not hold the run, the slot or Drain, and its
// goroutine must end once it does return.
func TestResetThatIgnoresItsContext(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	h := newHarness(t, rawSpec(1, 1), Options{ResetTimeout: 10 * time.Millisecond, CallGrace: 10 * time.Millisecond})
	h.waitIdle(1)
	release := make(chan struct{})
	h.fake.set(func(f *fakeSandboxes) {
		f.resetHook = func(context.Context, string) error { <-release; return nil }
	})

	type result struct {
		pl   Placement
		done Done
	}
	finished := make(chan result, 1)
	go func() {
		pl, done, _ := h.run(context.Background(), nil)
		finished <- result{pl, done}
	}()
	r := receive(t, finished, "Run to return past the reset deadline")
	if r.done.Reset != ResetFailed || r.done.Cause != RetireResetFailed {
		t.Fatalf("done %+v", r.done)
	}
	waitFor(t, func() bool { return !h.fake.isLive(r.pl.SandboxID) }, "the member to be destroyed while its Reset still blocks")
	h.fake.set(func(f *fakeSandboxes) { f.resetHook = nil })
	h.waitIdle(1)
	next, done, err := h.run(context.Background(), nil)
	if err != nil || next.SandboxID == r.pl.SandboxID || done.Reset != ResetOK {
		t.Fatalf("the slot was lost: %+v %+v %v", next, done, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	if !h.pool.Drain(ctx) {
		t.Fatal("Drain must finish while an abandoned Reset still blocks")
	}
	h.pool.Close()
	close(release)
	waitFor(t, func() bool { return goruntime.NumGoroutine() <= baseline }, "the abandoned Reset's goroutine to end")
}

// A nil from a Reset whose deadline has passed proves nothing, even when the
// pool is still waiting for its answer.
func TestLateNilFromResetIsIgnored(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{ResetTimeout: 10 * time.Millisecond, CallGrace: patience})
	h.waitIdle(1)
	h.fake.set(func(f *fakeSandboxes) {
		f.resetHook = func(ctx context.Context, _ string) error { <-ctx.Done(); return nil }
	})
	pl, done, err := h.run(context.Background(), nil)
	if err != nil || done.Reset != ResetFailed {
		t.Fatalf("done %+v err %v", done, err)
	}
	waitFor(t, func() bool { return !h.fake.isLive(pl.SandboxID) }, "the member to be destroyed")
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
	raw := rawSpec(1, 1)
	raw.BorrowTimeout = 50 * time.Millisecond
	h := newHarness(t, raw, Options{})
	h.waitIdle(1)
	defer h.holdMember()()
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
}

func TestTimedOutBorrowerColdStartsIntoAFreeSlot(t *testing.T) {
	raw := rawSpec(1, 2)
	raw.BorrowTimeout = 10 * time.Millisecond
	h := newHarness(t, raw, Options{})
	h.waitIdle(1)
	defer h.holdMember()()
	pl, _, err := h.run(context.Background(), nil)
	if err != nil || pl.Warm {
		t.Fatalf("placement %+v err %v, want a cold start", pl, err)
	}
}

func TestWaiterIsHandedTheFreedMember(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	release := h.holdMember()
	defer release()
	got := make(chan Placement, 1)
	go func() {
		pl, _, err := h.run(context.Background(), nil)
		if err != nil {
			t.Error(err)
		}
		got <- pl
	}()
	h.waitWaiters(1)
	release()
	pl := receive(t, got, "the waiter's run")
	if !pl.Warm || pl.SandboxRun != 2 {
		t.Fatalf("the waiter got %+v, want the reset member", pl)
	}
}

func TestWaitersAreServedInOrder(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	release := h.holdMember()
	defer release()
	served := make(chan string, 3)
	for i, name := range []string{"first", "second", "third"} {
		go func() {
			if _, _, err := h.run(context.Background(), func(context.Context, Placement) error {
				served <- name
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
		h.waitWaiters(i + 1)
	}
	release()
	for _, want := range []string{"first", "second", "third"} {
		if got := receive(t, served, "the next waiter"); got != want {
			t.Fatalf("served %s, want %s", got, want)
		}
	}
}

// In a cold pool nothing is on its way to a waiter, so the slot a destroy
// frees is the only thing that can serve it before its timeout.
func TestFreedSlotGoesToAWaiter(t *testing.T) {
	h := newHarness(t, rawSpec(0, 1), Options{})
	release := make(chan struct{})
	running := make(chan struct{})
	h.hold(func(context.Context, Placement) error {
		close(running)
		<-release
		return nil
	})
	receive(t, running, "the run to begin")
	got := make(chan Placement, 1)
	go func() {
		pl, _, err := h.run(context.Background(), nil)
		if err != nil {
			t.Error(err)
		}
		got <- pl
	}()
	h.waitWaiters(1)
	close(release)
	if pl := receive(t, got, "the waiter to get the freed slot"); pl.Warm {
		t.Fatalf("placement %+v, want a cold start", pl)
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
	receive(t, inReset, "the reset to begin")

	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if h.pool.Drain(short) {
		t.Fatal("drained while a reset was in flight")
	}
	if _, _, err := h.run(context.Background(), nil); !errors.Is(err, ErrDraining) {
		t.Fatalf("a run during drain got %v", err)
	}
	close(release)
	done := receive(t, finished, "the in-flight run")
	if done.Reset != ResetOK || done.Cause != RetireDrain {
		t.Fatalf("the in-flight run's trailer %+v, want ok then retired by drain", done)
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), patience)
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

func TestDrainFailsWaiters(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	release := h.holdMember()
	defer release()
	errc := make(chan error, 1)
	go func() { _, _, err := h.run(context.Background(), nil); errc <- err }()
	h.waitWaiters(1)
	short, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	h.pool.Drain(short)
	if err := receive(t, errc, "the waiter to fail"); !errors.Is(err, ErrDraining) {
		t.Fatalf("err = %v, want ErrDraining", err)
	}
}

// A start that was in flight when drain began comes back to a pool that must
// not keep it.
func TestRefillFinishingDuringDrainIsRetired(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, rawSpec(1, 1), Options{}, func(f *fakeSandboxes) { f.startBlock = block })
	short, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if h.pool.Drain(short) {
		t.Fatal("drained with a start in flight")
	}
	close(block)
	ctx, cancel2 := context.WithTimeout(context.Background(), patience)
	defer cancel2()
	if !h.pool.Drain(ctx) {
		t.Fatalf("drain did not converge: %+v", h.pool.Stats())
	}
	if started, destroyed := h.fake.counts(); started != 1 || destroyed != 1 {
		t.Fatalf("started %d destroyed %d, want the late member destroyed", started, destroyed)
	}
}

func TestDrainWaitsForRetiringMembers(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.fake.set(func(f *fakeSandboxes) { f.destroyBlk = block })
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if h.pool.Drain(short) {
		t.Fatal("Drain reported success with a destroy still running")
	}
	close(block)
	ctx, cancel2 := context.WithTimeout(context.Background(), patience)
	defer cancel2()
	if !h.pool.Drain(ctx) || h.fake.liveCount() != 0 {
		t.Fatalf("drain did not converge: %+v", h.pool.Stats())
	}
}

// Nothing calls Sweep here: Drain alone has to get past failing destroys.
func TestDrainRetriesFailedDestroys(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	var failures atomic.Int32
	h.fake.set(func(f *fakeSandboxes) {
		f.destroyErr = func(string) error {
			if failures.Add(1) <= 5 {
				return errBoom
			}
			return nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	if !h.pool.Drain(ctx) {
		t.Fatalf("drain gave up: %+v", h.pool.Stats())
	}
	if n := h.fake.liveCount(); n != 0 {
		t.Fatalf("%d sandboxes survive the drain", n)
	}
	if s := h.pool.Stats(); s.DestroyFailuresTotal != 5 || s.DestroyedTotal != 2 {
		t.Fatalf("destroy failures %d destroyed %d, want 5 and 2", s.DestroyFailuresTotal, s.DestroyedTotal)
	}
}

// "Drain timed out, then Close" is how a runner shuts down, so runs are still
// in flight. Run this under -race: a check-in that started a destroy after
// Close began would race its WaitGroup.
func TestCloseWithRunsInFlight(t *testing.T) {
	for range 50 {
		h := newHarness(t, rawSpec(1, 1), Options{}, func(f *fakeSandboxes) {
			f.resetHook = func(context.Context, string) error { return errBoom }
		})
		h.waitIdle(1)
		go func() { _, _, _ = h.run(context.Background(), nil) }()
		short, cancel := context.WithTimeout(context.Background(), time.Microsecond)
		h.pool.Drain(short)
		cancel()
		h.pool.Close()
	}
}

func TestCheckInAfterCloseLeavesTheMemberToTheRunner(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	hold := make(chan struct{})
	running := make(chan struct{})
	finished := make(chan Done, 1)
	go func() {
		_, done, _ := h.run(context.Background(), func(context.Context, Placement) error {
			close(running)
			<-hold
			return nil
		})
		finished <- done
	}()
	receive(t, running, "the run to begin")
	h.pool.Close()
	close(hold)
	if done := receive(t, finished, "the run to check in after Close"); done.Cause != RetireDrain {
		t.Fatalf("done %+v, want retired by drain", done)
	}
	h.pool.Close() // waits for any destroy that check-in wrongly started
	if _, destroyed := h.fake.counts(); destroyed != 0 {
		t.Fatalf("destroyed %d after Close, which must not start destroys", destroyed)
	}
}

func TestIdleMemberThatDiedIsRetired(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	h.pool.mu.Lock()
	dead := h.pool.idleQ[0].id
	h.pool.mu.Unlock()
	h.fake.set(func(f *fakeSandboxes) {
		f.aliveHook = func(_ context.Context, id string) error {
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

func TestRecentlyIdleMemberIsNotProbed(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.clock.Advance(idleProbeAfter - time.Nanosecond)
	if _, _, err := h.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.probes(); n != 0 {
		t.Fatalf("%d probes", n)
	}
}

// One client that went away must not cost the pool its healthy members.
func TestCancelledClientLeavesIdleMembersAlone(t *testing.T) {
	const size = 5
	h := newHarness(t, rawSpec(size, size), Options{})
	h.waitIdle(size)
	h.clock.Advance(idleProbeAfter)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := h.run(ctx, func(context.Context, Placement) error {
		t.Error("a cancelled client was lent a member")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s := h.pool.Stats(); s.Idle != size || s.Retired[RetireUnhealthy] != 0 {
		t.Fatalf("stats %+v", s)
	}
	if n := h.fake.probes(); n != 0 {
		t.Fatalf("%d probes for a client that had already gone", n)
	}
}

func TestClientCancelDuringProbeKeepsTheMember(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.clock.Advance(idleProbeAfter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probing := make(chan struct{})
	finish := make(chan struct{})
	h.fake.set(func(f *fakeSandboxes) {
		f.aliveHook = func(context.Context, string) error {
			close(probing)
			<-finish
			return nil
		}
	})
	errc := make(chan error, 1)
	go func() { _, _, err := h.run(ctx, nil); errc <- err }()
	receive(t, probing, "the probe to begin")
	cancel()
	close(finish)
	if err := receive(t, errc, "the cancelled run"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	h.waitIdle(1)
	s := h.pool.Stats()
	if s.Retired[RetireUnhealthy] != 0 {
		t.Fatalf("a client's cancel retired a healthy member: %+v", s)
	}
	h.fake.set(func(f *fakeSandboxes) { f.aliveHook = nil })
	if pl, _, err := h.run(context.Background(), nil); err != nil || pl.SandboxRun != 1 {
		t.Fatalf("the kept member: %+v %v, want its first run", pl, err)
	}
}

// A probe can outlast the start of a drain, which has stopped lending by then.
func TestDrainDuringProbeStopsTheLend(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.clock.Advance(idleProbeAfter)
	probing := make(chan struct{})
	finish := make(chan struct{})
	h.fake.set(func(f *fakeSandboxes) {
		f.aliveHook = func(context.Context, string) error {
			close(probing)
			<-finish
			return nil
		}
	})
	errc := make(chan error, 1)
	go func() {
		_, _, err := h.run(context.Background(), func(context.Context, Placement) error {
			t.Error("a run was lent a member after drain began")
			return nil
		})
		errc <- err
	}()
	receive(t, probing, "the probe to begin")
	short, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	h.pool.Drain(short)
	close(finish)
	if err := receive(t, errc, "the probing run"); !errors.Is(err, ErrDraining) {
		t.Fatalf("err = %v, want ErrDraining", err)
	}
	waitFor(t, func() bool { return h.fake.liveCount() == 0 }, "the member to be destroyed")
}

// A probe gets its own deadline: a client with none must not hang on a member
// that stopped answering.
func TestProbeThatNeverAnswersRetiresTheMember(t *testing.T) {
	cases := []struct {
		name string
		hook func(ctx context.Context, release <-chan struct{}) error
	}{
		{"honours its context", func(ctx context.Context, _ <-chan struct{}) error { <-ctx.Done(); return ctx.Err() }},
		{"ignores its context", func(_ context.Context, release <-chan struct{}) error { <-release; return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, rawSpec(2, 2), Options{ProbeTimeout: 5 * time.Millisecond, CallGrace: 5 * time.Millisecond})
			h.waitIdle(2)
			h.pool.mu.Lock()
			hung := h.pool.idleQ[0].id
			h.pool.mu.Unlock()
			release := make(chan struct{})
			defer close(release)
			h.fake.set(func(f *fakeSandboxes) {
				f.aliveHook = func(ctx context.Context, id string) error {
					if id == hung {
						return c.hook(ctx, release)
					}
					return nil
				}
			})
			h.clock.Advance(idleProbeAfter)
			got := make(chan Placement, 1)
			go func() {
				pl, _, err := h.run(context.Background(), nil)
				if err != nil {
					t.Error(err)
				}
				got <- pl
			}()
			if pl := receive(t, got, "the run to move on from the hung member"); pl.SandboxID == hung {
				t.Fatalf("lent the hung member %s", hung)
			}
			if h.pool.Stats().Retired[RetireUnhealthy] != 1 {
				t.Fatal("not counted as unhealthy")
			}
		})
	}
}

func TestBorrowerCancelledWhileWaiting(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	release := h.holdMember()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, _, err := h.run(ctx, nil); errc <- err }()
	h.waitWaiters(1)
	cancel()
	if err := receive(t, errc, "the cancelled waiter"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	release()
	// The freed member is not lost on the cancelled waiter.
	h.waitIdle(1)
	if h.pool.Stats().RunsTotal != 1 {
		t.Fatalf("runs %d, the cancelled borrow must not count", h.pool.Stats().RunsTotal)
	}
}

// takeOnlyMember takes the only member out of the pool, as a run would. It
// counts no run, because none will happen unless a test hands the member on.
func takeOnlyMember(h *harness) *member {
	h.t.Helper()
	h.waitIdle(1)
	p := h.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.popIdle()
	m.runs--
	return m
}

// grantWhileGivingUp waits for a borrower to queue, then holds mu while
// giveUp makes it stop waiting and hands it m anyway. The borrower wakes to a
// grant it no longer expects, by whichever path the scheduler picks, and each
// path has to deal with it. Checking the queue and granting under one hold of
// mu means the borrower that is handed m is certainly still queued.
func grantWhileGivingUp(h *harness, m *member, giveUp func()) {
	h.t.Helper()
	p := h.pool
	granted := false
	waitFor(h.t, func() bool {
		p.withLock(func() {
			if len(p.waiters) == 1 {
				giveUp()
				p.toIdle(m)
				granted = true
			}
		})
		return granted
	}, "a borrower to queue")
}

func TestCancelRacingAGrantReturnsTheMember(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	for i := range 40 {
		m := takeOnlyMember(h)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { _, _, err := h.run(ctx, nil); errc <- err }()
		grantWhileGivingUp(h, m, func() {
			cancel()
			time.Sleep(time.Millisecond) // lets the borrower reach mu, not required for the outcome
		})
		if err := receive(t, errc, "the cancelled borrower"); !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: err = %v", i, err)
		}
	}
	h.waitIdle(1)
	if s := h.pool.Stats(); s.RunsTotal != 0 {
		t.Fatalf("stats %+v, a borrower that gave up ran nothing", s)
	}
	if pl, _, err := h.run(context.Background(), nil); err != nil || pl.SandboxRun != 1 {
		t.Fatalf("placement %+v err %v: an unused grant must not count against max_runs", pl, err)
	}
}

// A waiter can also be handed a slot to cold-start into. If it has gone by
// then, the slot must not stay reserved for it.
func TestCancelRacingASlotGrantFreesTheSlot(t *testing.T) {
	h := newHarness(t, rawSpec(0, 1), Options{})
	p := h.pool
	var taken *member
	p.withLock(func() { taken = p.placeholder(true) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { _, _, err := h.run(ctx, nil); errc <- err }()
	h.waitWaiters(1)
	p.withLock(func() {
		cancel()
		time.Sleep(time.Millisecond) // lets the borrower reach mu, not required for the outcome
		p.remove(taken)
	})
	if err := receive(t, errc, "the cancelled borrower"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s := p.Stats(); s.Starting != 0 {
		t.Fatalf("the slot stayed reserved: %+v", s)
	}
	if pl, _, err := h.run(context.Background(), nil); err != nil || pl.Warm {
		t.Fatalf("the next client: %+v %v, want a cold start", pl, err)
	}
}

func TestTimeoutRacingAGrantUsesTheMember(t *testing.T) {
	raw := rawSpec(1, 1)
	raw.BorrowTimeout = time.Millisecond
	h := newHarness(t, raw, Options{})
	for i := range 40 {
		m := takeOnlyMember(h)
		var granted atomic.Bool
		got := make(chan error, 1)
		go func() {
			for {
				_, _, err := h.run(context.Background(), nil)
				// With so short a timeout it may give up before it is seen in
				// the queue. Once granted it must never report exhaustion.
				if !errors.Is(err, ErrExhausted) || granted.Load() {
					got <- err
					return
				}
			}
		}()
		grantWhileGivingUp(h, m, func() {
			granted.Store(true)
			time.Sleep(2 * time.Millisecond) // lets the timer fire, not required for the outcome
		})
		if err := receive(t, got, "the timed-out borrower"); err != nil {
			t.Fatalf("round %d: a borrower holding a grant got %v", i, err)
		}
	}
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

// The first frame tells the client how long it waited, and the run counters
// count its program. A retry the client never saw must not change either.
func TestRetriedExecIsOneProgramAndItsStartupIsCumulative(t *testing.T) {
	h := newHarness(t, rawSpec(2, 2), Options{})
	h.waitIdle(2)
	const lost = 7 * time.Second
	var startups []time.Duration
	_, err := h.pool.Run(context.Background(), func(_ context.Context, pl Placement) error {
		startups = append(startups, pl.Startup)
		if len(startups) == 1 {
			h.clock.Advance(lost)
			return ErrNotStarted
		}
		return nil
	})
	if err != nil || len(startups) != 2 || startups[1] != lost {
		t.Fatalf("err %v startups %v, want the second to include the %s lost", err, startups, lost)
	}
	if s := h.pool.Stats(); s.RunsTotal != 1 || s.WarmRunsTotal != 1 {
		t.Fatalf("runs %d warm %d, want one program", s.RunsTotal, s.WarmRunsTotal)
	}
}

func TestColdPoolStartsEveryRunFresh(t *testing.T) {
	h := newHarness(t, rawSpec(0, 2), Options{})
	if s := h.pool.Stats(); s.Starting != 0 || s.Idle != 0 {
		t.Fatalf("a size-0 pool pre-started members: %+v", s)
	}
	for i := range 3 {
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

// The experiment reports the reset's price as the gap between its warm arms,
// so the unsafe arm must differ only in the post-run reset.
func TestUnsafePoolDiffersOnlyInThePostRunReset(t *testing.T) {
	raw := rawSpec(1, 1)
	raw.Reuse = ReuseUnsafeNoReset
	spec := mustParse(t, raw, unsafeLimits)
	if !spec.MemberSpec().DenyIPC {
		t.Fatal("the unsafe arm must deny IPC like the reset arm")
	}
	fake := newFake(t, 1)
	fake.unsafe = true
	fake.resetHook = func(context.Context, string) error { t.Error("reset called on an unsafe pool"); return nil }
	clock := newClock()
	p := New(spec, fake, Options{Now: clock.Now, StartBackoff: time.Millisecond})
	t.Cleanup(p.Close)
	waitFor(t, func() bool { return p.Stats().Idle == 1 }, "member")
	fake.set(func(f *fakeSandboxes) {
		for id, s := range f.live {
			if !s.prepared {
				t.Errorf("%s was not prepared at admission", id)
			}
		}
	})
	for i := 1; i <= 2; i++ {
		var pl Placement
		done, err := p.Run(context.Background(), func(_ context.Context, got Placement) error {
			pl = got
			fake.use(got.SandboxID)
			defer fake.release(got.SandboxID)
			return nil
		})
		if err != nil || pl.Reuse != ReuseUnsafeNoReset || pl.SandboxRun != i || done.Reset != ResetSkipped {
			t.Fatalf("%+v %+v %v", pl, done, err)
		}
	}
	clock.Advance(idleProbeAfter)
	if _, err := p.Run(context.Background(), func(context.Context, Placement) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := fake.probes(); n != 1 {
		t.Fatalf("%d probes of a long-idle member, want 1", n)
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

// Sweep is on a coarse tick, so a borrow can get there first. The member's
// record TTL assumes it is never lent past max_age.
func TestMemberPastMaxAgeIsNotLent(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	h.pool.mu.Lock()
	old := h.pool.idleQ[0].id
	h.pool.mu.Unlock()
	h.clock.Advance(30 * time.Minute)
	pl, _, err := h.run(context.Background(), nil)
	if err != nil || pl.SandboxID == old {
		t.Fatalf("placement %+v err %v: lent %s past max_age", pl, err, old)
	}
	if h.pool.Stats().Retired[RetireMaxAge] != 1 {
		t.Fatal("not counted as max_age")
	}
}

func TestFailedDestroyKeepsTheSlotUntilSweepRetries(t *testing.T) {
	raw := rawSpec(1, 1)
	raw.MaxRuns = 1
	h := newHarness(t, raw, Options{})
	h.waitIdle(1)
	h.fake.set(func(f *fakeSandboxes) { f.destroyErr = func(string) error { return errBoom } })
	pl, done, _ := h.run(context.Background(), nil)
	if done.Cause != RetireMaxRuns {
		t.Fatalf("%+v", done)
	}
	waitFor(t, func() bool { return h.pool.Stats().DestroyFailuresTotal == 1 }, "the destroy to fail")
	if s := h.pool.Stats(); s.Retiring != 1 || s.Starting != 0 || s.Idle != 0 {
		t.Fatalf("the slot must stay taken: %+v", s)
	}
	h.fake.set(func(f *fakeSandboxes) {
		found := false
		for _, e := range f.events {
			found = found || e == pl.SandboxID+" destroy: failed: boom"
		}
		if !found {
			t.Errorf("no audit event for the failed destroy: %v", f.events)
		}
	})
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
	busy := receive(t, running, "the run to begin")
	if !h.pool.RetireExpired(busy.SandboxID) {
		t.Fatal("the pool disowned its busy member")
	}
	if h.fake.isLive(busy.SandboxID) == false {
		t.Fatal("a busy member was destroyed under a run")
	}
	if !h.pool.RetireExpired("sbx_" + map[string]string{"sbx_1": "2", "sbx_2": "1"}[busy.SandboxID]) {
		t.Fatal("the pool disowned its idle member")
	}
	waitFor(t, func() bool { return h.pool.Stats().Retired[RetireExpired] == 1 }, "the idle member to retire now")
	close(hold)
	if done := receive(t, finished, "the busy run"); done.Reset != ResetRetired || done.Cause != RetireExpired {
		t.Fatalf("busy member's trailer %+v, want retired at check-in", done)
	}
	if h.pool.RetireExpired("sbx_nope") {
		t.Fatal("a leaked record reported as the pool's")
	}
}

// A member the reaper marked goes on being marked if a borrower gives it back
// unused.
func TestGivenBackMemberKeepsTheReapersMark(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	p := h.pool
	p.mu.Lock()
	m := p.popIdle()
	p.mu.Unlock()
	p.RetireExpired(m.id)
	p.withLock(func() { p.giveBack(m) })
	if n := p.Stats().Retired[RetireExpired]; n != 1 {
		t.Fatalf("retired as expired %d times, want 1", n)
	}
	waitFor(t, func() bool { return !h.fake.isLive(m.id) }, "the marked member to be destroyed")
}

// An adapter bug must not breach max_size: the sandbox it returned is real, so
// it keeps its slot until it is destroyed, and a failing destroy is retried.
func TestStartWithoutResettableIsRetiredKeepingItsSlot(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, rawSpec(1, 1), Options{}, func(f *fakeSandboxes) {
		f.noResettable = true
		f.destroyBlk = block
	})
	waitFor(t, func() bool { return h.pool.Stats().Retiring == 1 }, "the unusable member to retire")
	s := h.pool.Stats()
	if s.StartFailedTotal != 1 || s.Idle != 0 || s.Retired[RetireUnhealthy] != 1 {
		t.Fatalf("stats %+v", s)
	}
	if started, _ := h.fake.counts(); started != 1 {
		t.Fatalf("started %d while the slot was taken", started)
	}
	h.fake.set(func(f *fakeSandboxes) {
		f.noResettable = false
		f.destroyBlk = nil
		f.destroyErr = func() func(string) error {
			failed := false
			return func(string) error {
				if !failed {
					failed = true
					return errBoom
				}
				return nil
			}
		}()
	})
	close(block)
	waitFor(t, func() bool { return h.pool.Stats().DestroyFailuresTotal == 1 }, "the destroy to fail")
	h.pool.Sweep()
	h.waitIdle(1)
	if _, destroyed := h.fake.counts(); destroyed != 1 {
		t.Fatalf("destroyed %d", destroyed)
	}
}

// fn is the runner's code, and a recovered panic there must not cost a slot.
func TestPanicInRunRetiresTheMember(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	var id string
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, the panic must continue", r)
			}
		}()
		_, _ = h.pool.Run(context.Background(), func(_ context.Context, pl Placement) error {
			id = pl.SandboxID
			panic("boom")
		})
	}()
	waitFor(t, func() bool { return !h.fake.isLive(id) }, "the member to be destroyed")
	h.waitIdle(1)
	if s := h.pool.Stats(); s.Busy != 0 || s.Retired[RetireUnhealthy] != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// transition's panic marks a bug here, but a caller may recover it. A pool
// that kept mu would then hang every later call.
func TestPanicUnderMuLeavesItUnlocked(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{})
	h.waitIdle(1)
	p := h.pool
	p.mu.Lock()
	m := p.popIdle()
	m.state = retiring
	p.mu.Unlock()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("an illegal transition did not panic")
			}
		}()
		p.checkIn(context.Background(), m)
	}()
	if !p.mu.TryLock() {
		t.Fatal("mu stayed locked after the panic")
	}
	p.mu.Unlock()
	m.state = busy
	func() {
		defer func() {
			if recover() == nil {
				t.Error("idling a dirty member did not panic")
			}
		}()
		m.dirty = true
		p.withLock(func() { p.toIdle(m) })
	}()
	if !p.mu.TryLock() {
		t.Fatal("mu stayed locked after the panic")
	}
	m.dirty = false
	p.mu.Unlock()
	p.withLock(func() { p.toIdle(m) })
}

func TestStartFailuresBackOffAndRecover(t *testing.T) {
	h := newHarness(t, rawSpec(1, 1), Options{StartBackoff: time.Millisecond, MaxStartBackoff: 2 * time.Millisecond}, func(f *fakeSandboxes) {
		f.startErr = func() error { return errBoom }
	})
	waitFor(t, func() bool { return h.pool.Stats().StartFailedTotal >= 3 }, "retries")
	h.fake.set(func(f *fakeSandboxes) { f.startErr = nil })
	h.waitIdle(1)
}

// waitBackoffArmed waits for a round of failed starts to settle: the delay
// asked for and nothing left starting. No timer fires until the test advances
// the clock, so the counts are final once this returns.
func waitBackoffArmed(t *testing.T, h *harness, delays int) {
	t.Helper()
	waitFor(t, func() bool {
		return len(h.clock.delays()) == delays && h.pool.Stats().Starting == 0
	}, "backoff delay %d", delays)
}

func TestBackoffDoublesPerRoundUpToTheCap(t *testing.T) {
	const size = 4
	h := newClockedHarness(t, rawSpec(size, size), Options{
		StartBackoff:    100 * time.Millisecond,
		MaxStartBackoff: 350 * time.Millisecond,
	}, func(f *fakeSandboxes) { f.startErr = func() error { return errBoom } })
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 350 * time.Millisecond, 350 * time.Millisecond}
	for round, delay := range want {
		waitBackoffArmed(t, h, round+1)
		if got := h.clock.delays()[round]; got != delay {
			t.Fatalf("round %d asked for %s, want %s (all %v)", round, got, delay, h.clock.delays())
		}
		// The round is over and the delay has not passed: every failure so far
		// belongs to a round, and no extra start slipped through.
		if failed, wantFailed := h.pool.Stats().StartFailedTotal, uint64((round+1)*size); failed != wantFailed {
			t.Fatalf("round %d: %d failed starts, want %d", round, failed, wantFailed)
		}
		h.clock.Advance(delay)
	}
}

func TestBackoffStartsOverAfterASuccess(t *testing.T) {
	h := newClockedHarness(t, rawSpec(1, 1), Options{
		StartBackoff:    100 * time.Millisecond,
		MaxStartBackoff: time.Second,
	}, func(f *fakeSandboxes) { f.startErr = func() error { return errBoom } })
	waitBackoffArmed(t, h, 1)
	h.clock.Advance(100 * time.Millisecond)
	waitBackoffArmed(t, h, 2)
	h.fake.set(func(f *fakeSandboxes) { f.startErr = nil })
	h.clock.Advance(200 * time.Millisecond)
	h.waitIdle(1)

	h.fake.set(func(f *fakeSandboxes) {
		f.startErr = func() error { return errBoom }
		f.resetHook = func(context.Context, string) error { return errBoom }
	})
	if _, done, _ := h.run(context.Background(), nil); done.Reset != ResetFailed {
		t.Fatalf("done %+v", done)
	}
	waitBackoffArmed(t, h, 3)
	if got := h.clock.delays()[2]; got != 100*time.Millisecond {
		t.Fatalf("after a success the backoff asked for %s, want it back at the start", got)
	}
}

// A client hanging up during its cold start says nothing about the backend:
// it must not read as a start failure, or one impatient client would throttle
// the pool's refill.
func TestClientCancelDuringColdStartIsNotAStartFailure(t *testing.T) {
	block := make(chan struct{})
	h := newClockedHarness(t, rawSpec(0, 2), Options{}, func(f *fakeSandboxes) { f.startBlock = block })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { _, _, err := h.run(ctx, nil); errc <- err }()
	waitFor(t, func() bool { return h.pool.Stats().Starting == 1 }, "the cold start to begin")
	cancel()
	err := receive(t, errc, "the cancelled cold start")
	if !errors.Is(err, ErrColdStart) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	s := h.pool.Stats()
	if s.StartFailedTotal != 0 || s.LastStartError != "" {
		t.Fatalf("counted as a start failure: %+v", s)
	}
	if d := h.clock.delays(); len(d) != 0 {
		t.Fatalf("armed the backoff: %v", d)
	}
	waitFor(t, func() bool { return h.pool.Stats().Starting == 0 }, "the slot to be free")
	close(block)
	if pl, _, err := h.run(context.Background(), nil); err != nil || pl.Warm {
		t.Fatalf("the next client: %+v %v", pl, err)
	}
}

func TestColdStartFailureArmsTheBackoff(t *testing.T) {
	h := newClockedHarness(t, rawSpec(0, 2), Options{StartBackoff: 100 * time.Millisecond}, func(f *fakeSandboxes) {
		f.startErr = func() error { return errBoom }
	})
	if _, _, err := h.run(context.Background(), nil); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if d := h.clock.delays(); len(d) != 1 || d[0] != 100*time.Millisecond {
		t.Fatalf("delays %v", d)
	}
}
