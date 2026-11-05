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
	// Startup is the time from Run's call to the member being handed over:
	// the borrow wait, plus the cold start when !Warm.
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
	// ResetTimeout bounds a reset. A workload can SIGSTOP the agent, so a
	// reset that does not finish in time is unproven and the member is
	// destroyed. Default 10 s.
	ResetTimeout time.Duration
	// StartTimeout bounds a start, which includes the admission reset.
	// Default 60 s.
	StartTimeout time.Duration
	// DestroyTimeout bounds a destroy. A failed destroy keeps its slot until
	// Sweep retries it. Default 30 s.
	DestroyTimeout time.Duration
	// StartBackoff is the first wait after a failed refill start; it doubles
	// up to MaxStartBackoff. Defaults 1 s and 30 s.
	StartBackoff    time.Duration
	MaxStartBackoff time.Duration
}

// idleProbeAfter is how long a member may idle before a borrower probes it
// with Alive first.
const idleProbeAfter = 10 * time.Second

// Stats is a snapshot of the pool. Counters reset with the runner process.
type Stats struct {
	Starting, Idle, Busy, Resetting, Retiring int
	RunsTotal, WarmRunsTotal                  uint64
	ResetsTotal, ResetFailuresTotal           uint64
	DestroyedTotal                            uint64
	ExhaustedTotal, StartFailedTotal          uint64
	Retired                                   map[RetireCause]uint64
	LastStartError                            string
	Dormant                                   bool
}

// waiter is a borrower blocked in acquire. It is handed a member directly,
// under the mutex, by whoever freed one. The channel is closed on drain.
type waiter struct{ grant chan *member }

// Pool owns its members. Everything under mu is guarded by it, and mu is
// never held across a call into Sandboxes, a Resettable, or fn.
type Pool struct {
	spec Spec
	sb   Sandboxes
	opts Options

	bg     context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	members  map[*member]struct{}
	idleQ    []*member // FIFO, so members age out evenly; empty whenever waiters exist
	waiters  []*waiter // FIFO
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
	if opts.ResetTimeout == 0 {
		opts.ResetTimeout = 10 * time.Second
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 60 * time.Second
	}
	if opts.DestroyTimeout == 0 {
		opts.DestroyTimeout = 30 * time.Second
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
	p.mu.Lock()
	p.refill()
	p.mu.Unlock()
	return p
}

func (p *Pool) now() time.Time { return p.opts.Now() }

// Run borrows a member, calls fn, then checks the member in on a context
// detached from ctx, so a client that disconnects mid-run can never strand a
// member. It returns once the member's fate is settled (its reset finished,
// or its destroy was started), so the caller can send the trailing frame.
// A member whose exec did not start is retired and fn runs once more on
// another member.
func (p *Pool) Run(ctx context.Context, fn RunFunc) (Done, error) {
	p.mu.Lock()
	p.lastRun = p.now()
	if p.dormant {
		p.dormant = false
		p.refill()
	}
	p.mu.Unlock()

	for attempt := 0; ; attempt++ {
		m, pl, err := p.acquire(ctx)
		if err != nil {
			return Done{}, err
		}
		err = fn(ctx, pl)
		if errors.Is(err, ErrNotStarted) {
			p.mu.Lock()
			p.retire(m, RetireUnhealthy)
			p.mu.Unlock()
			if attempt == 0 {
				continue
			}
			return Done{Reset: ResetRetired, Cause: RetireUnhealthy}, err
		}
		return p.checkIn(ctx, m), err
	}
}

// acquire implements the borrow rule, bounded by MaxSize:
//  1. an idle member: take it (FIFO), probing Alive first if it idled long;
//  2. else, nothing in flight toward idle and a slot free: cold-start now;
//  3. else wait up to borrow_timeout for a hand-off; on timeout cold-start
//     if a slot is free, else ErrExhausted.
//
// It returns the member in state busy with runs already counted.
func (p *Pool) acquire(ctx context.Context) (*member, Placement, error) {
	start := p.now()
	p.mu.Lock()
	for {
		if p.draining {
			p.mu.Unlock()
			return nil, Placement{}, ErrDraining
		}
		if m := p.popIdle(); m != nil {
			idled := p.now().Sub(m.idleSince)
			p.mu.Unlock()
			if idled >= idleProbeAfter && m.reset != nil {
				if err := m.reset.Alive(ctx); err != nil {
					p.sb.Event(m.id, "alive", err.Error())
					p.mu.Lock()
					p.retire(m, RetireUnhealthy)
					continue
				}
			}
			return p.granted(ctx, m, start)
		}
		if !p.inFlight() && p.slotFree() {
			m := p.placeholder(true)
			p.mu.Unlock()
			return p.coldStart(ctx, m, start)
		}
		w := &waiter{grant: make(chan *member, 1)}
		p.waiters = append(p.waiters, w)
		p.mu.Unlock()

		timer := time.NewTimer(p.spec.borrowTimeout)
		select {
		case m, ok := <-w.grant:
			timer.Stop()
			if !ok {
				return nil, Placement{}, ErrDraining
			}
			return p.granted(ctx, m, start)
		case <-ctx.Done():
			timer.Stop()
			if m := p.unregister(w); m != nil {
				p.mu.Lock()
				p.giveBack(m)
				p.mu.Unlock()
			}
			return nil, Placement{}, ctx.Err()
		case <-timer.C:
			if m := p.unregister(w); m != nil {
				return p.granted(ctx, m, start)
			}
			p.mu.Lock()
			if p.draining {
				p.mu.Unlock()
				return nil, Placement{}, ErrDraining
			}
			if p.slotFree() {
				m := p.placeholder(true)
				p.mu.Unlock()
				return p.coldStart(ctx, m, start)
			}
			p.stats.ExhaustedTotal++
			p.mu.Unlock()
			return nil, Placement{}, ErrExhausted
		}
	}
}

// granted turns a hand-off into a placement. A starting member is a slot
// reserved for this borrower's cold start; anything else is a warm member
// already in state busy.
func (p *Pool) granted(ctx context.Context, m *member, start time.Time) (*member, Placement, error) {
	if m.state == starting {
		return p.coldStart(ctx, m, start)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return m, p.placement(m, true, start), nil
}

// unregister removes w from the queue. It returns the member that was handed
// to w in the meantime, if any: hand-offs happen under mu, so after removal
// the buffered channel holds everything that will ever arrive.
func (p *Pool) unregister(w *waiter) *member {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, q := range p.waiters {
		if q == w {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			break
		}
	}
	select {
	case m, ok := <-w.grant:
		if ok {
			return m
		}
	default:
	}
	return nil
}

// giveBack returns a member a cancelled borrower was handed. mu held.
func (p *Pool) giveBack(m *member) {
	if m.state == starting {
		p.remove(m)
		return
	}
	m.runs--
	if p.draining {
		p.retire(m, RetireDrain)
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
	got, err := p.sb.Start(sctx, p.spec.MemberSpec(), p.spec.resets())
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.admit(m, got, err); err != nil {
		return nil, Placement{}, fmt.Errorf("%w: %w", ErrColdStart, err)
	}
	p.transition(m, busy)
	m.runs = 1
	return m, p.placement(m, false, start), nil
}

// placement counts the run and describes it. mu held.
func (p *Pool) placement(m *member, warm bool, start time.Time) Placement {
	p.stats.RunsTotal++
	if warm {
		p.stats.WarmRunsTotal++
	}
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
	p.mu.Lock()
	f := p.fate(m)
	switch {
	case f.Retire != "":
		p.retire(m, f.Retire)
		p.mu.Unlock()
		return Done{Reset: ResetRetired, Cause: f.Retire}
	case !f.Reset:
		p.toIdle(m)
		p.mu.Unlock()
		return Done{Reset: ResetSkipped}
	}
	p.transition(m, resetting)
	p.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opts.ResetTimeout)
	began := p.now()
	report, err := m.reset.Reset(rctx)
	cancel()
	took := p.now().Sub(began)

	if err != nil {
		p.sb.Event(m.id, "reset", "failed: "+err.Error())
	} else {
		p.sb.Event(m.id, "reset", fmt.Sprintf("ok: killed %d, removed %d entries, %d ipc, in %s",
			report.ProcessesKilled, report.EntriesRemoved, report.IPCRemoved, took))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.ResetsTotal++
	if err != nil {
		p.stats.ResetFailuresTotal++
		p.retire(m, RetireResetFailed)
		return Done{Reset: ResetFailed, Duration: took, Cause: RetireResetFailed}
	}
	m.lastReset = took
	// Drain, the reaper or idle expiry may have arrived while the reset ran.
	if f := p.fate(m); f.Retire != "" {
		p.retire(m, f.Retire)
		return Done{Reset: ResetOK, Duration: took, Cause: f.Retire}
	}
	p.toIdle(m)
	return Done{Reset: ResetOK, Duration: took}
}

// fate evaluates the policy for m against the live table. mu held.
func (p *Pool) fate(m *member) Fate {
	return fate(p.spec, m, p.now(), p.live(), p.target(), p.draining)
}

// toIdle is the only entry to idle: a hand-off to the first waiter, or the
// tail of the idle queue. mu held.
func (p *Pool) toIdle(m *member) {
	p.transition(m, idle)
	if len(p.waiters) > 0 {
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.transition(m, busy)
		m.runs++
		w.grant <- m
		return
	}
	m.idleSince = p.now()
	p.idleQ = append(p.idleQ, m)
}

func (p *Pool) popIdle() *member {
	if len(p.idleQ) == 0 {
		return nil
	}
	m := p.idleQ[0]
	p.idleQ = p.idleQ[1:]
	p.transition(m, busy)
	m.runs++
	return m
}

// placeholder reserves a slot in the table for a start that has not returned
// yet. mu held.
func (p *Pool) placeholder(cold bool) *member {
	m := &member{state: starting, cold: cold}
	p.members[m] = struct{}{}
	return m
}

// admit fills a placeholder from a returned start, or drops it and arms the
// refill backoff. mu held.
func (p *Pool) admit(m *member, got Started, err error) error {
	if err == nil && p.spec.resets() && got.Reset == nil {
		err = errors.New("pool: adapter returned a member without a Resettable")
		p.wg.Add(1)
		go p.destroyOrphan(got.SandboxID)
	}
	if err != nil {
		p.remove(m)
		p.stats.StartFailedTotal++
		p.stats.LastStartError = err.Error()
		p.armBackoff()
		return err
	}
	p.backoff = 0
	m.id = got.SandboxID
	m.reset = got.Reset
	m.createdAt = p.now()
	return nil
}

// destroyOrphan destroys a sandbox the pool never admitted.
func (p *Pool) destroyOrphan(id string) {
	defer p.wg.Done()
	p.sb.Event(id, "retire", string(RetireUnhealthy))
	dctx, cancel := context.WithTimeout(p.bg, p.opts.DestroyTimeout)
	defer cancel()
	_ = p.sb.Destroy(dctx, id)
}

// startOne is a refill start. The member is handed to the first waiter or
// goes idle; under drain it is retired.
func (p *Pool) startOne(m *member) {
	defer p.wg.Done()
	sctx, cancel := context.WithTimeout(p.bg, p.opts.StartTimeout)
	defer cancel()
	got, err := p.sb.Start(sctx, p.spec.MemberSpec(), p.spec.resets())
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.admit(m, got, err) != nil {
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
// is announced on the member's audit trail; a Sweep retry passes none.
// mu held.
func (p *Pool) startDestroy(m *member, cause RetireCause) {
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
	p.mu.Lock()
	defer p.mu.Unlock()
	m.destroying = false
	m.destroyErr = err
	if err != nil {
		return
	}
	p.stats.DestroyedTotal++
	p.remove(m)
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
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		w.grant <- p.placeholder(true)
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

// armBackoff delays the next refill after a failed start, doubling up to the
// cap. mu held.
func (p *Pool) armBackoff() {
	if p.backoff == 0 {
		p.backoff = p.opts.StartBackoff
	} else {
		p.backoff = min(p.backoff*2, p.opts.MaxStartBackoff)
	}
	if p.backoffPending {
		return
	}
	p.backoffPending = true
	p.wg.Add(1)
	go func(wait time.Duration) {
		defer p.wg.Done()
		select {
		case <-time.After(wait):
		case <-p.bg.Done():
		}
		p.mu.Lock()
		p.backoffPending = false
		p.refill()
		p.mu.Unlock()
	}(p.backoff)
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
	for m := range p.members {
		if m.state == retiring && !m.destroying {
			p.startDestroy(m, "")
		}
	}
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
// owner: retired now if idle, at its next settle otherwise.
func (p *Pool) RetireExpired(sandboxID string) {
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
		return
	}
}

// Drain stops lending, fails waiters, retires idle members and waits until
// ctx for the rest. It is idempotent and reports whether every member is
// gone, so a repeated call converges.
func (p *Pool) Drain(ctx context.Context) bool {
	p.mu.Lock()
	p.draining = true
	for _, w := range p.waiters {
		close(w.grant)
	}
	p.waiters = nil
	for _, m := range append([]*member(nil), p.idleQ...) {
		p.retireIdle(m, RetireDrain)
	}
	for {
		if len(p.members) == 0 {
			p.mu.Unlock()
			return true
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
			p.mu.Lock()
		case <-ctx.Done():
			return false
		}
	}
}

// Close stops lending and background work and waits for it to finish. It
// does not destroy members: a runner shutdown reconciles them itself. Call it
// after Drain, or once no Run is in flight: a run checking in during Close
// would start a destroy that Close is no longer waiting for.
func (p *Pool) Close() {
	p.mu.Lock()
	p.draining = true
	for _, w := range p.waiters {
		close(w.grant)
	}
	p.waiters = nil
	p.mu.Unlock()
	p.cancel()
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
	s.Starting, s.Idle, s.Busy, s.Resetting, s.Retiring = 0, 0, 0, 0, 0
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
