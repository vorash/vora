package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vorash/vora/internal/pool"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// poolRecord is what survives a restart: the definition Parse accepted, so
// LoadPools re-parses it against the runner's current limits, and whether a
// delete had begun, so LoadPools removes it instead of refilling it.
type poolRecord struct {
	ID        pool.ID      `json:"id"`
	Spec      pool.RawSpec `json:"spec"`
	CreatedAt time.Time    `json:"created_at"`
	Draining  bool         `json:"draining,omitempty"`
}

func validPoolID(id string) bool { return ulid.ValidPrefix(id, ulid.PrefixPool) }

func (s *Server) poolPath(id pool.ID) string {
	return filepath.Join(s.cfg.PoolsDir(), string(id)+".json")
}

// savePool writes rec atomically.
func (s *Server) savePool(rec poolRecord) error {
	if !validPoolID(string(rec.ID)) {
		return fmt.Errorf("pool: invalid id %q", rec.ID)
	}
	dir := s.cfg.PoolsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+string(rec.ID)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.poolPath(rec.ID))
}

func (s *Server) deletePoolRecord(id pool.ID) error {
	if !validPoolID(string(id)) {
		return fmt.Errorf("pool: invalid id %q", id)
	}
	if err := os.Remove(s.poolPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// loadPoolRecords reads every record. An unreadable one is skipped and
// reported, never silently dropped.
func (s *Server) loadPoolRecords() ([]poolRecord, error) {
	entries, err := os.ReadDir(s.cfg.PoolsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []poolRecord
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.cfg.PoolsDir(), name))
		if err != nil {
			s.logger.Warn("pool record unreadable", "file", name, "error", err)
			continue
		}
		var rec poolRecord
		if err := json.Unmarshal(data, &rec); err != nil || string(rec.ID)+".json" != name || rec.Spec.ID != rec.ID {
			s.logger.Warn("pool record malformed", "file", name, "error", err)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// poolEvents writes pool lifecycle events (reset, retire, alive, destroy) off
// the pool's own path. A reset's check-in holds the run's trailer, and with a
// Postgres audit store an append is a network round trip, so events queue to one
// writer that keeps their order. A stalled store must not stall the pools: when
// the queue is full the newest event is dropped, counted and logged. What is lost
// is a line of the trail, since a member's reset outcome is in Stats and its
// RunDone. Dropping the newest keeps what is queued in order and needs nothing
// from the writer.
type poolEvents struct {
	s  *Server
	mu sync.Mutex // guards closed, started and dropped, and is never held across a write
	// closed is set by flush; later events (a run checking in after ClosePools)
	// are written inline.
	closed  bool
	started bool
	// dropped counts the events lost since the queue last had room.
	dropped uint64
	ch      chan sandbox.Event
	done    chan struct{} // closed when the writer has ended
	// abandon ends every write when flush gives up on the store.
	abandon      context.Context
	abandonWrite context.CancelFunc

	writeTimeout time.Duration
	flushTimeout time.Duration
}

const (
	poolEventQueue = 1024
	// poolEventWriteTimeout is also what a stalled store costs a check-in after
	// ClosePools, when events are written inline.
	poolEventWriteTimeout = 5 * time.Second
	poolEventFlushTimeout = 10 * time.Second
)

func (e *poolEvents) init(s *Server) {
	e.s = s
	e.ch = make(chan sandbox.Event, poolEventQueue)
	e.done = make(chan struct{})
	e.abandon, e.abandonWrite = context.WithCancel(context.Background())
	e.writeTimeout, e.flushTimeout = poolEventWriteTimeout, poolEventFlushTimeout
}

func (e *poolEvents) append(ev sandbox.Event) {
	ev.Timestamp = time.Now().UTC()
	if !e.enqueue(ev) {
		e.write(ev)
	}
}

// enqueue queues ev, or drops it if the queue is full. It reports false once
// the writer is gone. It never blocks, so a check-in never waits on the store.
func (e *poolEvents) enqueue(ev sandbox.Event) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	if !e.started {
		e.started = true
		go e.drain()
	}
	select {
	case e.ch <- ev:
		if e.dropped > 0 {
			e.s.logger.Warn("pool event queue has room again", "dropped", e.dropped)
			e.dropped = 0
		}
	default:
		e.s.metrics.PoolEventsDroppedTotal.Inc()
		// Once per stall: a stalled store would otherwise add a log line per run.
		if e.dropped == 0 {
			e.s.logger.Warn("pool event queue is full; dropping the newest events", "sandbox_id", ev.SandboxID, "kind", ev.Kind)
		}
		e.dropped++
	}
	return true
}

func (e *poolEvents) drain() {
	defer close(e.done)
	for ev := range e.ch {
		e.write(ev)
	}
}

func (e *poolEvents) write(ev sandbox.Event) {
	ctx, cancel := context.WithTimeout(e.abandon, e.writeTimeout)
	defer cancel()
	if err := e.s.audit.AppendEvent(ctx, ev.SandboxID, ev); err != nil {
		e.s.metrics.AuditWriteFailedTotal.Inc()
		e.s.logger.Warn("append pool event failed", "sandbox_id", ev.SandboxID, "kind", ev.Kind, "error", err)
	}
}

// flush stops the writer and waits for what is queued, up to flushTimeout. Past
// that the rest is abandoned: shutdown does not wait on a stalled store.
func (e *poolEvents) flush() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.ch)
	started := e.started
	e.mu.Unlock()
	if !started {
		return
	}
	timer := time.NewTimer(e.flushTimeout)
	defer timer.Stop()
	select {
	case <-e.done:
	case <-timer.C:
		e.s.logger.Warn("pool events not written before shutdown; abandoning them", "queued", len(e.ch))
		e.abandonWrite()
	}
}
