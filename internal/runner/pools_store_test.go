package runner

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

// stallingAudit stands in for a stalled Postgres. Pool events and exec records
// wait for release, or for their context unless ignoreDeadline is set, and what
// gets through is kept in arrival order.
type stallingAudit struct {
	audit.Store
	release        chan struct{}
	ignoreDeadline bool
	delay          time.Duration

	mu     sync.Mutex
	events []sandbox.Event
	began  chan struct{} // receives once per stalled write, if set
}

func (a *stallingAudit) stall(ctx context.Context) error {
	if a.began != nil {
		select {
		case a.began <- struct{}{}:
		default:
		}
	}
	if a.delay > 0 {
		time.Sleep(a.delay)
	}
	if a.release == nil {
		return nil
	}
	if a.ignoreDeadline {
		<-a.release
		return nil
	}
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *stallingAudit) AppendEvent(ctx context.Context, id string, ev sandbox.Event) error {
	if ev.Kind == "" {
		return a.Store.AppendEvent(ctx, id, ev)
	}
	if err := a.stall(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *stallingAudit) details() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, ev := range a.events {
		out = append(out, ev.Detail)
	}
	return out
}

func numberedEvent(i int) sandbox.Event {
	return sandbox.Event{SandboxID: "sbx_x", Kind: "reset", Detail: strconv.Itoa(i)}
}

func numbered(from, to int) []string {
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, strconv.Itoa(i))
	}
	return out
}

func stalledServer(t *testing.T) (*Server, *stallingAudit) {
	t.Helper()
	s := testServer(t, runtime.NewRegistry())
	a := &stallingAudit{Store: s.audit, release: make(chan struct{})}
	s.SetAudit(a)
	return s, a
}

// A check-in appends an event per reset. It must return at once with the store
// stalled, losing the newest events past the queue, in order and counted.
func TestFullEventQueueDropsTheNewestAndNeverBlocks(t *testing.T) {
	s, a := stalledServer(t)
	a.began = make(chan struct{}, 1)
	s.events.writeTimeout = time.Minute

	s.events.append(numberedEvent(0))
	<-a.began // the writer holds event 0, so the queue is empty and capacity is exact

	const total = 1 + poolEventQueue + 75
	returned := make(chan struct{})
	go func() {
		for i := 1; i < total; i++ {
			s.events.append(numberedEvent(i))
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("append blocked on a stalled store")
	}
	if got := counterValue(t, s.metrics.PoolEventsDroppedTotal); got != 75 {
		t.Fatalf("vora_pool_events_dropped_total = %v, want 75", got)
	}

	close(a.release)
	s.events.flush()
	if got, want := a.details(), numbered(0, 1+poolEventQueue); !slices.Equal(got, want) {
		t.Fatalf("%d events written, want the first %d in order", len(got), len(want))
	}
}

// Nothing blocks on the store past write's deadline, and a write it ended is
// counted as failed.
func TestEventWriteHasADeadline(t *testing.T) {
	s, _ := stalledServer(t)
	s.events.writeTimeout = 10 * time.Millisecond
	for i := range 3 {
		s.events.append(numberedEvent(i))
	}
	waitFor(t, func() bool { return counterValue(t, s.metrics.AuditWriteFailedTotal) == 3 }, "the three writes to time out")
}

// Shutdown does not wait for a store that ignores even its deadline.
func TestFlushIsBoundedWhenTheStoreNeverAnswers(t *testing.T) {
	s, a := stalledServer(t)
	defer close(a.release)
	a.ignoreDeadline = true
	s.events.flushTimeout = 50 * time.Millisecond
	s.events.append(numberedEvent(0))

	done := make(chan struct{})
	go func() {
		s.ClosePools()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ClosePools hung on the stalled store")
	}
}

// A flush holding the queue's lock across its wait would block every check-in
// for the whole flush.
func TestAppendDoesNotWaitForAFlush(t *testing.T) {
	s, a := stalledServer(t)
	defer close(a.release)
	a.ignoreDeadline = true
	a.began = make(chan struct{}, 1)
	s.events.flushTimeout = time.Minute
	s.events.append(numberedEvent(0))
	<-a.began
	go s.events.flush()
	waitFor(t, func() bool {
		s.events.mu.Lock()
		defer s.events.mu.Unlock()
		return s.events.closed
	}, "the flush to begin")
	done := make(chan bool)
	go func() { done <- s.events.enqueue(numberedEvent(1)) }()
	select {
	case queued := <-done:
		if queued {
			t.Fatal("an event was queued after the flush closed the queue")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a check-in's event blocked behind a flush that is waiting on the store")
	}
}

// ClosePools is the last chance to write what is queued: the process exits next.
func TestClosePoolsWritesQueuedEventsInOrder(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	a := &stallingAudit{Store: s.audit, delay: 100 * time.Microsecond}
	s.SetAudit(a)
	const n = 200
	for i := range n {
		s.events.append(numberedEvent(i))
	}
	s.ClosePools()
	if got, want := a.details(), numbered(0, n); !slices.Equal(got, want) {
		t.Fatalf("after ClosePools the store has %d events: %v; want %d in order", len(got), got[:min(len(got), 10)], n)
	}
	s.events.append(numberedEvent(n))
	if got := a.details(); len(got) != n+1 {
		t.Fatalf("an event after ClosePools was not written: %d events", len(got))
	}
}

// A stalled store costs a run nothing it can see: the run ends, the member's
// reset passes and it comes back, and the lost record is counted.
func TestStalledAuditStoreDoesNotHoldARun(t *testing.T) {
	h := newWarmHarness(t)
	release := make(chan struct{})
	defer close(release)
	h.s.auditTimeout = 20 * time.Millisecond
	h.s.SetAudit(&stalledExecs{stallingAudit{Store: h.s.audit, release: release}})
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)

	began := time.Now()
	for i := range 3 {
		if _, err := h.run(h.ctx(), p.GetId(), 5); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		h.waitIdle(p.GetId(), 1)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("three runs took %s against a stalled store", took)
	}
	if got := counterValue(t, h.s.metrics.AuditWriteFailedTotal); got < 3 {
		t.Fatalf("audit_write_failed_total = %v, want a failure per lost exec record", got)
	}
}

type stalledExecs struct{ stallingAudit }

func (a *stalledExecs) AppendExec(ctx context.Context, _ string, _ sandbox.ExecRecord) error {
	<-ctx.Done()
	return ctx.Err()
}
