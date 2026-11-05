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
	"testing"
	"time"
)

// TestStress drives many borrowers through random failures and checks the
// pool's promises from the fake's own books: no sandbox serves two runs at
// once, a sandbox whose reset failed never runs again, live sandboxes stay
// within max_size, and shutdown leaves no goroutine behind.
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
	raw.BorrowTimeout = 20 * time.Millisecond
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
		case chance(10):
			return errBoom
		}
		time.Sleep(jitter(time.Millisecond))
		return nil
	}
	fake.aliveHook = func(string) error {
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
		ResetTimeout: 10 * time.Millisecond,
		StartBackoff: time.Millisecond,
	})

	stop := make(chan struct{})
	sweeperDone := make(chan struct{})
	var wg sync.WaitGroup
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
	var outcomes sync.Map
	count := func(k string) {
		v, _ := outcomes.LoadOrStore(k, new(int64))
		rngMu.Lock()
		*v.(*int64)++
		rngMu.Unlock()
	}
	for b := 0; b < borrowers; b++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < runsEach; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				done, err := p.Run(ctx, func(ctx context.Context, pl Placement) error {
					fake.use(pl.SandboxID)
					defer fake.release(pl.SandboxID)
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
				cancel()
				switch {
				case err == nil:
					count("ok " + done.Reset.String())
				case errors.Is(err, ErrExhausted), errors.Is(err, ErrColdStart), errors.Is(err, ErrNotStarted), errors.Is(err, context.Canceled):
					count("err " + err.Error()[:12])
				default:
					t.Errorf("unexpected error %v", err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-sweeperDone

	outcomes.Range(func(k, v any) bool {
		rngMu.Lock()
		defer rngMu.Unlock()
		t.Logf("%-24s %d", k, *v.(*int64))
		return true
	})
	for _, want := range []string{"ok ok", "ok failed", "ok retired"} {
		if _, ok := outcomes.Load(want); !ok {
			t.Errorf("the run never produced outcome %q, so it did not exercise that path", want)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		drained := p.Drain(ctx)
		cancel()
		if drained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain did not converge: %+v", p.Stats())
		}
		p.Sweep()
	}
	p.Close()
	if n := fake.liveCount(); n != 0 {
		t.Fatalf("%d sandboxes left after drain", n)
	}
	waitFor(t, func() bool { return goruntime.NumGoroutine() <= baseline }, "goroutines to return to %d, now %d", baseline, goruntime.NumGoroutine())
}
