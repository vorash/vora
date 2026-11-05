package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// Errors Run returns. fn's own error is returned unchanged.
var (
	// ErrNotStarted is what fn returns (wrapped) when the backend refused the
	// exec before any byte reached the client. The pool retires that member
	// and calls fn once more on another.
	ErrNotStarted = errors.New("pool: exec did not start")
	ErrExhausted  = errors.New("pool: no sandbox free within the borrow timeout")
	ErrDraining   = errors.New("pool: draining")
	ErrColdStart  = errors.New("pool: cold start failed")
)

// Started is one sandbox the adapter provisioned for the pool.
type Started struct {
	SandboxID string
	// Reset is the backend's Resettable for this member when the pool asked
	// for one (prepare was true), nil otherwise.
	Reset runtime.Resettable
}

// Sandboxes is what a pool needs from the runner.
type Sandboxes interface {
	// Start provisions one member from spec (record, workspace, backend
	// Create and Start). The pool has reserved its own capacity, so no
	// per-sandbox capacity check. When prepare is true it must also call
	// runtime.Reusable.Prepare and return the Resettable, destroying the
	// sandbox and returning an error if Prepare fails.
	//
	// After an error the pool has nothing to destroy, so Start cleans up after
	// itself. It must do that on context.WithoutCancel(ctx): a cold start runs
	// on the client's context, which may already be cancelled.
	Start(ctx context.Context, spec runtime.SandboxSpec, prepare bool) (Started, error)
	// Destroy is the runner's idempotent destroy.
	Destroy(ctx context.Context, sandboxID string) error
	// Event appends a lifecycle event to the member's audit trail.
	Event(sandboxID, kind, detail string)
}

// Placement is how a run got its sandbox. The runner sends it as the run's
// first frame.
type Placement struct {
	SandboxID string
	Warm      bool
	// Startup is the time from Run's call to this member being handed over:
	// the borrow wait, the cold start when !Warm, and a first attempt whose
	// exec did not start. It is what the client waited before its program ran.
	Startup time.Duration
	// PriorReset is the reset that prepared this member; 0 on its first run.
	PriorReset time.Duration
	SandboxRun int // 1-based, this run included
	Reuse      Reuse
}

// ResetOutcome is what happened to the member after the run.
type ResetOutcome int

const (
	// ResetOK means the member was proven clean and is idle again.
	ResetOK ResetOutcome = iota + 1
	// ResetFailed means the reset returned an error or hit its deadline; the
	// member is being destroyed.
	ResetFailed
	// ResetSkipped means the pool does not reset (unsafe reuse).
	ResetSkipped
	// ResetRetired means fate retired the member without a reset.
	ResetRetired
)

func (o ResetOutcome) String() string {
	switch o {
	case ResetOK:
		return "ok"
	case ResetFailed:
		return "failed"
	case ResetSkipped:
		return "skipped"
	case ResetRetired:
		return "retired"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// Done is the run's trailing frame: this run's own reset.
type Done struct {
	Reset    ResetOutcome
	Duration time.Duration // the reset's, when one ran
	Cause    RetireCause   // set when the member is being destroyed
}

// RunFunc runs one exec on the placed member. ctx is the client's request
// context.
type RunFunc func(ctx context.Context, p Placement) error

// Options tune timeouts. Zero means the default.
type Options struct {
	Now func() time.Time
	// After is the timer behind the refill backoff and Drain's destroy retries.
	// Default time.After.
	After func(time.Duration) <-chan time.Time
	// ResetTimeout bounds a reset. A workload can SIGSTOP the agent, so a
	// reset that does not finish in time is unproven and the member is
	// destroyed. Default 10 s.
	ResetTimeout time.Duration
	// ProbeTimeout bounds the Alive probe of a long-idle member. Default 2 s.
	ProbeTimeout time.Duration
	// CallGrace is how long past its deadline a Reset or Alive may take to
	// return before the pool stops waiting and retires the member. Default 200 ms.
	CallGrace time.Duration
	// StartTimeout bounds a start, which includes the admission reset.
	// Default 60 s.
	StartTimeout time.Duration
	// DestroyTimeout bounds a destroy. A failed destroy keeps its slot until
	// Sweep or Drain retries it. Default 30 s.
	DestroyTimeout time.Duration
	// DestroyRetry is how often Drain retries failed destroys. Default 1 s.
	DestroyRetry time.Duration
	// StartBackoff is the wait after a round of refill starts failed; it
	// doubles per round up to MaxStartBackoff. Defaults 1 s and 30 s.
	StartBackoff    time.Duration
	MaxStartBackoff time.Duration
}

// idleProbeAfter is how long a member may idle before a borrower probes it
// with Alive first.
const idleProbeAfter = 10 * time.Second

// Stats is a snapshot of the pool. Counters reset with the runner process.
type Stats struct {
	Starting, Idle, Busy, Resetting, Retiring int
	// RunsTotal counts programs, once each: a run whose exec did not start and
	// was retried is one, and a run still in fn is none yet.
	RunsTotal, WarmRunsTotal         uint64
	ResetsTotal, ResetFailuresTotal  uint64
	DestroyedTotal                   uint64
	DestroyFailuresTotal             uint64
	ExhaustedTotal, StartFailedTotal uint64
	Retired                          map[RetireCause]uint64
	LastStartError                   string
	Dormant                          bool
}

// Pool owns its members. Everything under mu is guarded by it, and mu is
// never held across a call into Sandboxes, a Resettable, or fn. Every critical
// section releases mu with a defer, because transition and toIdle panic on a
// bug in this package and a caller may recover.
type Pool struct {
	spec Spec
	sb   Sandboxes
	opts Options

	bg     context.Context
	cancel context.CancelFunc // called under mu, so bg.Err() tells a holder of mu that Close began
	wg     sync.WaitGroup

	mu      sync.Mutex
	members map[*member]struct{}
	idleQ   []*member // FIFO, so members age out evenly; empty whenever waiters exist
	// waiters are borrowers blocked in acquire, FIFO. Each is handed a member
	// directly, under mu, by whoever freed one. Its channel holds one and is
	// closed on drain.
	waiters  []chan *member
	draining bool
	dormant  bool // idle_expiry passed: the warm target is 0 until the next run
	lastRun  time.Time
	changed  chan struct{} // closed and replaced whenever the table shrinks
	stats    Stats

	backoff        time.Duration
	backoffPending bool
}

// New builds a pool and starts its refill. Members appear asynchronously.
func New(spec Spec, sb Sandboxes, opts Options) *Pool {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = time.After
	}
	if opts.ResetTimeout == 0 {
		opts.ResetTimeout = 10 * time.Second
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 2 * time.Second
	}
	if opts.CallGrace == 0 {
		opts.CallGrace = 200 * time.Millisecond
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 60 * time.Second
	}
	if opts.DestroyTimeout == 0 {
		opts.DestroyTimeout = 30 * time.Second
	}
	if opts.DestroyRetry == 0 {
		opts.DestroyRetry = time.Second
	}
	if opts.StartBackoff == 0 {
		opts.StartBackoff = time.Second
	}
	if opts.MaxStartBackoff == 0 {
		opts.MaxStartBackoff = 30 * time.Second
	}
	bg, cancel := context.WithCancel(context.Background())
	p := &Pool{
		spec:    spec,
		sb:      sb,
		opts:    opts,
		bg:      bg,
		cancel:  cancel,
		members: map[*member]struct{}{},
		changed: make(chan struct{}),
		stats:   Stats{Retired: map[RetireCause]uint64{}},
	}
	p.lastRun = p.now()
	p.withLock(p.refill)
	return p
}

func (p *Pool) now() time.Time { return p.opts.Now() }

func (p *Pool) withLock(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
}

// Run borrows a member, calls fn, then checks the member in on a context
// detached from ctx, so a client that disconnects mid-run can never strand a
// member. It returns once the member's fate is settled (its reset finished,
// or its destroy was started), so the caller can send the trailing frame.
// A member whose exec did not start is retired and fn runs once more on
// another member. If fn panics the member is retired and the panic continues.
func (p *Pool) Run(ctx context.Context, fn RunFunc) (Done, error) {
	start := p.now()
	p.withLock(func() {
		p.lastRun = start
		if p.dormant {
			p.dormant = false
			p.refill()
		}
	})

	for attempt := 0; ; attempt++ {
		m, pl, err := p.acquire(ctx, start)
		if err != nil {
			return Done{}, err
		}
		err = p.call(ctx, m, pl, fn)
		if !errors.Is(err, ErrNotStarted) {
			p.withLock(func() {
				p.stats.RunsTotal++
				if pl.Warm {
					p.stats.WarmRunsTotal++
				}
			})
			return p.checkIn(ctx, m), err
		}
		p.withLock(func() { p.retire(m, RetireUnhealthy) })
		if attempt > 0 {
			return Done{Reset: ResetRetired, Cause: RetireUnhealthy}, err
		}
	}
}

// call runs fn. A member whose run panicked or called Goexit has no known
// state, so it is retired rather than left busy forever.
func (p *Pool) call(ctx context.Context, m *member, pl Placement, fn RunFunc) error {
	returned := false
	defer func() {
		if !returned {
			p.withLock(func() { p.retire(m, RetireUnhealthy) })
		}
	}()
	err := fn(ctx, pl)
	returned = true
	return err
}

// acquire implements the borrow rule, bounded by MaxSize:
//  1. an idle member: take it (FIFO), probing Alive first if it idled long;
//  2. else, nothing in flight toward idle and a slot free: cold-start now;
//  3. else wait up to borrow_timeout for a hand-off; on timeout cold-start
//     if a slot is free, else ErrExhausted.
//
// It returns the member in state busy with runs already counted.
func (p *Pool) acquire(ctx context.Context, start time.Time) (*member, Placement, error) {
	for {
		m, grant, err := p.nextBorrow(ctx)
		if err != nil {
			return nil, Placement{}, err
		}
		if grant != nil {
			return p.await(ctx, grant, start)
		}
		if p.healthy(ctx, m) {
			return p.lend(ctx, m, start)
		}
	}
}

// nextBorrow applies one pass of the borrow rule: a member to use (idle, or a
// placeholder to cold-start into), or a queue position to wait on.
func (p *Pool) nextBorrow(ctx context.Context) (*member, chan *member, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if p.draining {
		return nil, nil, ErrDraining
	}
	if m := p.popIdle(); m != nil {
		return m, nil, nil
	}
	if !p.inFlight() && p.slotFree() {
		return p.placeholder(true), nil, nil
	}
	grant := make(chan *member, 1)
	p.waiters = append(p.waiters, grant)
	return nil, grant, nil
}

// healthy probes a member that idled long, and retires it if the probe fails.
// The probe has its own deadline and ignores the client's cancel, because a
// client going away says nothing about the member.
func (p *Pool) healthy(ctx context.Context, m *member) bool {
	if m.reset == nil || p.now().Sub(m.idleSince) < idleProbeAfter {
		return true
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opts.ProbeTimeout)
	defer cancel()
	_, err := bounded(pctx, p.opts.CallGrace, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, m.reset.Alive(ctx)
	})
	if err == nil {
		return true
	}
	p.sb.Event(m.id, "alive", err.Error())
	p.withLock(func() { p.retire(m, RetireUnhealthy) })
	return false
}

// await waits in the queue for a hand-off.
func (p *Pool) await(ctx context.Context, grant chan *member, start time.Time) (*member, Placement, error) {
	timer := time.NewTimer(p.spec.borrowTimeout)
	defer timer.Stop()
	select {
	case m, ok := <-grant:
		if !ok {
			return nil, Placement{}, ErrDraining
		}
		return p.lend(ctx, m, start)
	case <-ctx.Done():
		if m := p.unregister(grant); m != nil {
			p.withLock(func() { p.giveBack(m) })
		}
		return nil, Placement{}, ctx.Err()
	case <-timer.C:
		if m := p.unregister(grant); m != nil {
			return p.lend(ctx, m, start)
		}
		m, err := p.slotAfterWait()
		if err != nil {
			return nil, Placement{}, err
		}
		return p.coldStart(ctx, m, start)
	}
}

// slotAfterWait reserves a slot for a borrower whose wait timed out.
func (p *Pool) slotAfterWait() (*member, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draining {
		return nil, ErrDraining
	}
	if p.slotFree() {
		return p.placeholder(true), nil
	}
	p.stats.ExhaustedTotal++
	return nil, ErrExhausted
}

// lend turns a member the borrower now owns into a placement: a starting
// member is a slot reserved for this borrower's cold start, anything else is a
// warm member already in state busy. If the client left or drain began in the
// meantime the member goes back unused.
func (p *Pool) lend(ctx context.Context, m *member, start time.Time) (*member, Placement, error) {
	if err := p.claim(ctx, m); err != nil {
		return nil, Placement{}, err
	}
	if m.state == starting {
		return p.coldStart(ctx, m, start)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return m, p.placement(m, true, start), nil
}

func (p *Pool) claim(ctx context.Context, m *member) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := ctx.Err()
	if err == nil && p.draining {
		err = ErrDraining
	}
	if err != nil {
		p.giveBack(m)
	}
	return err
}

// unregister removes grant from the queue. It returns the member that was
// handed to it in the meantime, if any: hand-offs happen under mu, so after
// removal the buffered channel holds everything that will ever arrive.
func (p *Pool) unregister(grant chan *member) *member {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, q := range p.waiters {
		if q == grant {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			break
		}
	}
	select {
	case m, ok := <-grant:
		if ok {
			return m
		}
	default:
	}
	return nil
}

// giveBack returns a member a borrower was handed and never ran: no run
// touched it, so it needs no reset, and it gets the rest of its fate. mu held.
func (p *Pool) giveBack(m *member) {
	if m.state == starting {
		p.remove(m)
		return
	}
	m.runs--
	if f := p.fate(m); f.Retire != "" {
		p.retire(m, f.Retire)
		return
	}
	p.toIdle(m)
}

// coldStart runs the borrower's own start into the reserved placeholder. It
// runs on the borrower's context: a client that goes away mid-start aborts
// it, and the adapter cleans up a start that failed.
func (p *Pool) coldStart(ctx context.Context, m *member, start time.Time) (*member, Placement, error) {
	sctx, cancel := context.WithTimeout(ctx, p.opts.StartTimeout)
	defer cancel()
	got, err := p.sb.Start(sctx, p.spec.MemberSpec(), p.spec.reused())
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.admit(ctx, m, got, err); err != nil {
		return nil, Placement{}, fmt.Errorf("%w: %w", ErrColdStart, err)
	}
	p.transition(m, busy)
	m.runs = 1
	return m, p.placement(m, false, start), nil
}

// placement describes the run a member is being lent to. From here the member
// is dirty until a nil Reset. mu held.
func (p *Pool) placement(m *member, warm bool, start time.Time) Placement {
	m.dirty = true
	return Placement{
		SandboxID:  m.id,
		Warm:       warm,
		Startup:    p.now().Sub(start),
		PriorReset: m.lastReset,
		SandboxRun: m.runs,
		Reuse:      p.spec.reuse,
	}
}

// checkIn applies fate after a run. The reset runs inline, on a context
// detached from the client's, under the reset deadline.
func (p *Pool) checkIn(ctx context.Context, m *member) Done {
	if done, settled := p.settleWithoutReset(m); settled {
		return done
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opts.ResetTimeout)
	defer cancel()
	began := p.now()
	report, err := bounded(rctx, p.opts.CallGrace, m.reset.Reset)
	took := p.now().Sub(began)

	if err != nil {
		p.sb.Event(m.id, "reset", "failed: "+err.Error())
	} else {
		p.sb.Event(m.id, "reset", fmt.Sprintf("ok: killed %d, removed %d entries, in %s",
			report.ProcessesKilled, report.EntriesRemoved, took))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.ResetsTotal++
	if err != nil {
		p.stats.ResetFailuresTotal++
		p.retire(m, RetireResetFailed)
		return Done{Reset: ResetFailed, Duration: took, Cause: RetireResetFailed}
	}
	m.dirty = false
	m.lastReset = took
	// Drain, the reaper or idle expiry may have arrived while the reset ran.
	if f := p.fate(m); f.Retire != "" {
		p.retire(m, f.Retire)
		return Done{Reset: ResetOK, Duration: took, Cause: f.Retire}
	}
	p.toIdle(m)
	return Done{Reset: ResetOK, Duration: took}
}

// settleWithoutReset applies fate at check-in. It reports false when fate
// wants a reset, with the member now resetting.
func (p *Pool) settleWithoutReset(m *member) (Done, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fate(m)
	switch {
	case f.Retire != "":
		p.retire(m, f.Retire)
		return Done{Reset: ResetRetired, Cause: f.Retire}, true
	case !f.Reset:
		p.toIdle(m)
		return Done{Reset: ResetSkipped}, true
	}
	p.transition(m, resetting)
	return Done{}, false
}

// bounded runs call in its own goroutine, so a backend call that ignores ctx
// cannot hold a member past its deadline. Once ctx ends it waits grace for
// call's own answer, then stops waiting; the goroutine ends when call returns.
// A nil that arrives after ctx ended proves nothing and is not returned.
func bounded[T any](ctx context.Context, grace time.Duration, call func(context.Context) (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	var zero T
	done := make(chan result, 1)
	go func() {
		val, err := call(ctx)
		done <- result{val, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case r = <-done:
		case <-timer.C:
			return zero, fmt.Errorf("backend call ignored its deadline: %w", ctx.Err())
		}
	}
	if r.err != nil {
		return zero, r.err
	}
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("backend call returned after its deadline: %w", err)
	}
	return r.val, nil
}

// fate evaluates the policy for m against the live table. mu held.
func (p *Pool) fate(m *member) Fate {
	return fate(p.spec, m, p.now(), p.live(), p.target(), p.draining)
}

// toIdle is the only entry to idle: a hand-off to the first waiter, or the
// tail of the idle queue. A member a run has used since its last nil Reset
// never gets past it, except on an unsafe pool. mu held.
func (p *Pool) toIdle(m *member) {
	if m.dirty && p.spec.reuse != ReuseUnsafeNoReset {
		panic(fmt.Sprintf("pool: member %s would go idle without a reset", m.id))
	}
	p.transition(m, idle)
	if len(p.waiters) > 0 {
		grant := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.transition(m, busy)
		m.runs++
		grant <- m
		return
	}
	m.idleSince = p.now()
	p.idleQ = append(p.idleQ, m)
}

// popIdle takes the oldest idle member, retiring any past max_age on the way:
// a Sweep tick may not have reached them yet. mu held.
func (p *Pool) popIdle() *member {
	for len(p.idleQ) > 0 {
		m := p.idleQ[0]
		p.idleQ = p.idleQ[1:]
		if p.now().Sub(m.createdAt) >= p.spec.maxAge {
			p.retire(m, RetireMaxAge)
			continue
		}
		p.transition(m, busy)
		m.runs++
		return m
	}
	return nil
}

// placeholder reserves a slot in the table for a start that has not returned
// yet. mu held.
func (p *Pool) placeholder(cold bool) *member {
	m := &member{state: starting, cold: cold}
	p.members[m] = struct{}{}
	return m
}

// admit fills a placeholder from a returned start. A failed start gives up the
// slot; a sandbox that came back unusable keeps it until it is destroyed. The
// backoff is armed before the slot is freed, because freeing it refills. A
// start that ended because ctx did (the client left, or Close) says nothing
// about the backend, so it is not counted as a failure. mu held.
func (p *Pool) admit(ctx context.Context, m *member, got Started, err error) error {
	if err == nil {
		m.id = got.SandboxID
		m.reset = got.Reset
		m.createdAt = p.now()
		if !p.spec.reused() || got.Reset != nil {
			p.backoff = 0
			return nil
		}
		err = errors.New("pool: adapter returned a member without a Resettable")
	}
	if ctx.Err() == nil {
		p.stats.StartFailedTotal++
		p.stats.LastStartError = err.Error()
		p.armBackoff()
	}
	if m.id != "" {
		p.retire(m, RetireUnhealthy)
	} else {
		p.remove(m)
	}
	return err
}

// startOne is a refill start. The member is handed to the first waiter or
// goes idle; under drain it is retired.
func (p *Pool) startOne(m *member) {
	defer p.wg.Done()
	sctx, cancel := context.WithTimeout(p.bg, p.opts.StartTimeout)
	defer cancel()
	got, err := p.sb.Start(sctx, p.spec.MemberSpec(), p.spec.reused())
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.admit(p.bg, m, got, err) != nil {
		return
	}
	if f := p.fate(m); f.Retire != "" {
		p.retire(m, f.Retire)
		return
	}
	p.toIdle(m)
}

// retire moves m to retiring and destroys it in the background. The slot
// stays taken until Destroy returns. mu held.
func (p *Pool) retire(m *member, cause RetireCause) {
	p.transition(m, retiring)
	p.stats.Retired[cause]++
	p.startDestroy(m, cause)
}

// startDestroy launches one destroy for a retiring member. A non-empty cause
// is announced on the member's audit trail; a retry passes none. After Close
// it starts nothing: the runner's reconcile owns what is left. mu held.
func (p *Pool) startDestroy(m *member, cause RetireCause) {
	if p.bg.Err() != nil {
		return
	}
	m.destroying = true
	p.wg.Add(1)
	go p.destroy(m, cause)
}

func (p *Pool) destroy(m *member, cause RetireCause) {
	defer p.wg.Done()
	if cause != "" {
		p.sb.Event(m.id, "retire", string(cause))
	}
	dctx, cancel := context.WithTimeout(p.bg, p.opts.DestroyTimeout)
	defer cancel()
	err := p.sb.Destroy(dctx, m.id)
	if err != nil {
		p.sb.Event(m.id, "destroy", "failed: "+err.Error())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m.destroying = false
	if err != nil {
		p.stats.DestroyFailuresTotal++
		return
	}
	p.stats.DestroyedTotal++
	p.remove(m)
}

// retryDestroys relaunches every destroy that failed. mu held.
func (p *Pool) retryDestroys() {
	for m := range p.members {
		if m.state == retiring && !m.destroying {
			p.startDestroy(m, "")
		}
	}
}

// remove takes m out of the table and gives its slot to the first waiter
// when nothing else is on its way to them, else to refill. mu held.
func (p *Pool) remove(m *member) {
	delete(p.members, m)
	close(p.changed)
	p.changed = make(chan struct{})
	if p.draining {
		return
	}
	if len(p.waiters) > 0 && !p.inFlight() && p.slotFree() {
		grant := p.waiters[0]
		p.waiters = p.waiters[1:]
		grant <- p.placeholder(true)
		return
	}
	p.refill()
}

// refill starts members until live members reach the target, never past
// MaxSize. It derives what to start from the table, so it is idempotent.
// mu held.
func (p *Pool) refill() {
	if p.draining || p.backoffPending || p.bg.Err() != nil {
		return
	}
	for p.live() < p.target() && p.slotFree() {
		m := p.placeholder(false)
		p.wg.Add(1)
		go p.startOne(m)
	}
}

// armBackoff delays the next refill after a failed start. The failures of one
// round of starts share a wait: the first arms it and doubles the delay, up to
// the cap. mu held.
func (p *Pool) armBackoff() {
	if p.backoffPending || p.draining {
		return
	}
	if p.backoff == 0 {
		p.backoff = p.opts.StartBackoff
	} else {
		p.backoff = min(p.backoff*2, p.opts.MaxStartBackoff)
	}
	p.backoffPending = true
	p.wg.Add(1)
	go func(wait <-chan time.Time) {
		defer p.wg.Done()
		select {
		case <-wait:
		case <-p.bg.Done():
		}
		p.withLock(func() {
			p.backoffPending = false
			p.refill()
		})
	}(p.opts.After(p.backoff))
}

// target is the warm size: 0 while dormant. mu held.
func (p *Pool) target() int {
	if p.dormant {
		return 0
	}
	return p.spec.size
}

// live counts members that are not on their way out. mu held.
func (p *Pool) live() int {
	n := 0
	for m := range p.members {
		if m.state != retiring {
			n++
		}
	}
	return n
}

func (p *Pool) slotFree() bool { return len(p.members) < p.spec.maxSize }

// inFlight reports whether some member will reach idle without a new start:
// a refill start, a reset, or (when the pool keeps members) a run. A cold
// start belongs to its borrower and counts as a run. mu held.
func (p *Pool) inFlight() bool {
	for m := range p.members {
		switch m.state {
		case resetting:
			return true
		case starting:
			if !m.cold || p.target() > 0 {
				return true
			}
		case busy:
			if p.target() > 0 {
				return true
			}
		}
	}
	return false
}

// transition applies one edge of the member state machine. An illegal edge
// is a bug in this package, not a runtime condition, so it panics.
func (p *Pool) transition(m *member, to memberState) {
	if !canTransition(m.state, to) {
		panic(fmt.Sprintf("pool: illegal transition %s -> %s", m.state, to))
	}
	m.state = to
}

// Sweep runs on the runner's periodic tick: idle expiry, max_age on idle
// members, retries of failed destroys, refill.
func (p *Pool) Sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.dormant && !p.draining && now.Sub(p.lastRun) >= p.spec.idleExpiry {
		p.dormant = true
	}
	for _, m := range append([]*member(nil), p.idleQ...) {
		switch {
		case now.Sub(m.createdAt) >= p.spec.maxAge:
			p.retireIdle(m, RetireMaxAge)
		case p.dormant:
			p.retireIdle(m, RetireIdleExpiry)
		}
	}
	p.retryDestroys()
	p.refill()
}

// retireIdle takes m off the idle queue and retires it. mu held.
func (p *Pool) retireIdle(m *member, cause RetireCause) {
	for i, q := range p.idleQ {
		if q == m {
			p.idleQ = append(p.idleQ[:i], p.idleQ[i+1:]...)
			break
		}
	}
	p.retire(m, cause)
}

// RetireExpired is how the runner's TTL reaper hands a pooled record to its
// owner: retired now if idle, at its next settle otherwise. It reports false
// if the pool has no such member, so the caller can clean up a leaked record.
func (p *Pool) RetireExpired(sandboxID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for m := range p.members {
		if m.id != sandboxID {
			continue
		}
		switch m.state {
		case idle:
			p.retireIdle(m, RetireExpired)
		case retiring:
		default:
			m.retireWhen = RetireExpired
		}
		return true
	}
	return false
}

// Drain stops lending, fails waiters, retires idle members and waits until
// ctx for the rest, retrying destroys that fail. It is idempotent and reports
// whether every member is gone, so a repeated call converges.
func (p *Pool) Drain(ctx context.Context) bool {
	p.withLock(func() {
		p.failWaiters()
		for _, m := range append([]*member(nil), p.idleQ...) {
			p.retireIdle(m, RetireDrain)
		}
	})
	for {
		gone, changed := p.drainStep()
		if gone {
			return true
		}
		select {
		case <-changed:
		case <-p.opts.After(p.opts.DestroyRetry):
		case <-p.bg.Done():
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (p *Pool) drainStep() (gone bool, changed <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.members) == 0 {
		return true, nil
	}
	p.retryDestroys()
	return false, p.changed
}

// failWaiters stops lending and sends every queued borrower ErrDraining.
// mu held.
func (p *Pool) failWaiters() {
	p.draining = true
	for _, grant := range p.waiters {
		close(grant)
	}
	p.waiters = nil
}

// Close stops lending and background work and waits for it to finish. It does
// not destroy members and does not wait for runs in flight: a run that checks
// in later gets its reset and its Done as usual, and whatever it retires stays
// for the runner's shutdown reconcile. So "Drain timed out, then Close" is safe.
func (p *Pool) Close() {
	p.withLock(func() {
		p.failWaiters()
		p.cancel()
	})
	p.wg.Wait()
}

// Stats snapshots the pool.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Retired = make(map[RetireCause]uint64, len(p.stats.Retired))
	for k, v := range p.stats.Retired {
		s.Retired[k] = v
	}
	for m := range p.members {
		switch m.state {
		case starting:
			s.Starting++
		case idle:
			s.Idle++
		case busy:
			s.Busy++
		case resetting:
			s.Resetting++
		case retiring:
			s.Retiring++
		}
	}
	s.Dormant = p.dormant
	return s
}
