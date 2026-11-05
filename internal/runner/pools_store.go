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
		if err := json.Unmarshal(data, &rec); err != nil || string(rec.ID)+".json" != name {
			s.logger.Warn("pool record malformed", "file", name, "error", err)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// poolEvents writes pool lifecycle events (reset, retire, alive, destroy)
// off the pool's own path. A reset's check-in holds the run's trailer, and
// with a Postgres audit store an append is a network round trip, so events
// queue to one writer that keeps their order. The queue is bounded: once it
// is full, Event blocks, because a dropped audit record is worse than a slow
// trailer.
type poolEvents struct {
	s  *Server
	mu sync.Mutex // held across a send, so flush never closes under one
	// closed is set by flush; later events (a run checking in after
	// ClosePools) are written inline.
	closed  bool
	started bool
	ch      chan sandbox.Event
	wg      sync.WaitGroup
}

const poolEventQueue = 1024

func (e *poolEvents) init(s *Server) {
	e.s = s
	e.ch = make(chan sandbox.Event, poolEventQueue)
}

func (e *poolEvents) append(ev sandbox.Event) {
	ev.Timestamp = time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		e.write(ev)
		return
	}
	if !e.started {
		e.started = true
		e.wg.Add(1)
		go e.drain()
	}
	e.ch <- ev
}

func (e *poolEvents) drain() {
	defer e.wg.Done()
	for ev := range e.ch {
		e.write(ev)
	}
}

func (e *poolEvents) write(ev sandbox.Event) {
	if err := e.s.audit.AppendEvent(context.Background(), ev.SandboxID, ev); err != nil {
		e.s.metrics.AuditWriteFailedTotal.Inc()
		e.s.logger.Warn("append pool event failed", "sandbox_id", ev.SandboxID, "kind", ev.Kind, "error", err)
	}
}

// flush writes what is queued and stops the writer.
func (e *poolEvents) flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.ch)
	e.wg.Wait()
}
