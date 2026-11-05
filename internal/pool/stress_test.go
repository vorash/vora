package pool

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	goruntime "runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStress drives many borrowers through random failures, cancels during
// waits and during runs, and a drain that starts while they are all busy. It
// checks the pool's promises from the fake's own books: no sandbox runs again
// without a nil Reset, no sandbox serves two runs at once, a sandbox whose
// reset failed never runs again, live sandboxes stay within max_size, and
// shutdown leaves no goroutine behind. What it exercised also comes from those
// books, never from what the pool reports about itself.
func TestStress(t *testing.T) {
	seed := time.Now().UnixNano()
	if s, err := strconv.ParseInt(os.Getenv("POOL_STRESS_SEED"), 10, 64); err == nil {
		seed = s
	}
	t.Logf("seed %d (rerun with POOL_STRESS_SEED=%d)", seed, seed)
	var rngMu sync.Mutex
	rng := rand.New(rand.NewSource(seed))
	chance := func(pct int) bool {
		rngMu.Lock()
		defer rngMu.Unlock()
		return rng.Intn(100) < pct
	}
	jitter := func(max time.Duration) time.Duration {
		rngMu.Lock()
		defer rngMu.Unlock()
		return time.Duration(rng.Int63n(int64(max)))
	}

	baseline := goruntime.NumGoroutine()

	raw := rawSpec(4, 6)
	raw.MaxRuns = 5
	raw.MaxAge = 2 * time.Minute
	raw.BorrowTimeout = 2 * time.Millisecond
	spec := mustParse(t, raw, testLimits)
	fake := newFake(t, spec.MaxSize())
	clock := newClock()
	fake.startErr = func() error {
		if chance(10) {
			return errBoom
		}
		return nil
	}
	fake.resetHook = func(ctx context.Context, id string) error {
		switch {
		case chance(3):
			<-ctx.Done()
			return ctx.Err()
		case chance(3):
			// A Reset that ignores its context and answers nil too late.
			time.Sleep(60*time.Millisecond + jitter(10*time.Millisecond))
			return nil
		case chance(10):
			return errBoom
		}
		time.Sleep(jitter(time.Millisecond))
		return nil
	}
	fake.aliveHook = func(context.Context, string) error {
		if chance(10) {
			return errBoom
		}
		return nil
	}
	fake.destroyErr = func(string) error {
		if chance(5) {
			return errBoom
		}
		return nil
	}
	p := New(spec, fake, Options{
		Now:          clock.Now,
		ResetTimeout: 50 * time.Millisecond,
		CallGrace:    5 * time.Millisecond,
		ProbeTimeout: 50 * time.Millisecond,
		StartBackoff: time.Millisecond,
		DestroyRetry: time.Millisecond,
	})

	stop := make(chan struct{})
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			clock.Advance(jitter(15 * time.Second))
			p.Sweep()
			if chance(20) {
				p.RetireExpired(fmt.Sprintf("sbx_%d", 1+jitter(40)))
			}
		}
	}()

	const borrowers, runsEach = 16, 40
	const drainAfter = borrowers * runsEach * 7 / 10
	var finished, ran atomic.Int64
	var draining sync.Once
	var drainer sync.WaitGroup
	var wg sync.WaitGroup
	for b := 0; b < borrowers; b++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < runsEach; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				var timer *time.Timer
				if chance(30) {
					// Lands in the wait, the probe, the cold start or the run.
					timer = time.AfterFunc(jitter(3*time.Millisecond), cancel)
				}
				_, err := p.Run(ctx, func(ctx context.Context, pl Placement) error {
					fake.use(pl.SandboxID)
					defer fake.release(pl.SandboxID)
					ran.Add(1)
					switch {
					case chance(5):
						return fmt.Errorf("agent: %w", ErrNotStarted)
					case chance(10):
						cancel()
						return ctx.Err()
					}
					time.Sleep(jitter(2 * time.Millisecond))
					return nil
				})
				if timer != nil {
					timer.Stop()
				}
				cancel()
				switch {
				case err == nil:
				case errors.Is(err, ErrExhausted), errors.Is(err, ErrColdStart), errors.Is(err, ErrNotStarted),
					errors.Is(err, context.Canceled), errors.Is(err, ErrDraining):
				default:
					t.Errorf("unexpected error %v", err)
				}
				if finished.Add(1) == drainAfter {
					draining.Do(func() {
						drainer.Add(1)
						go func() {
							defer drainer.Done()
							short, cancel := context.WithTimeout(context.Background(), jitter(5*time.Millisecond)+time.Microsecond)
							defer cancel()
							p.Drain(short)
						}()
					})
				}
			}
		}()
	}
	wg.Wait()
	drainer.Wait()
	close(stop)
	<-sweeperDone

	// Sweep has stopped, so Drain alone has to finish the failed destroys.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), patience)
	defer cancelDrain()
	if !p.Drain(drainCtx) {
		t.Fatalf("drain did not converge: %+v", p.Stats())
	}
	p.Close()

	started, destroyed := fake.counts()
	resetsOK, resetsFailed := fake.resets()
	t.Logf("runs %d, sandboxes started %d, destroyed %d, nil resets %d, failed resets %d",
		ran.Load(), started, destroyed, resetsOK, resetsFailed)
	for name, n := range map[string]int{
		"runs": int(ran.Load()), "nil resets": resetsOK, "failed resets": resetsFailed, "destroys": destroyed,
	} {
		if n == 0 {
			t.Errorf("the fake saw no %s, so the run did not exercise that path", name)
		}
	}
	if n := fake.liveCount(); n != 0 {
		t.Fatalf("%d sandboxes left after drain", n)
	}
	if started != destroyed {
		t.Fatalf("started %d, destroyed %d: every sandbox the fake started must be gone", started, destroyed)
	}
	waitFor(t, func() bool { return goruntime.NumGoroutine() <= baseline }, "goroutines to return to %d", baseline)
}
