// Package jsonl implements audit.Store using per-sandbox JSONL/JSON files on
// the local filesystem (events.jsonl + exec/<execution_id>.json).
package jsonl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/sandbox"
)

// Store is a filesystem-backed audit store.
type Store struct {
	mu sync.Mutex
	// logsDir maps a sandbox id to its logs directory on the host.
	logsDir func(sandboxID string) string
}

var _ audit.Store = (*Store)(nil)

// New returns a JSONL store that resolves a sandbox's logs directory with
// logsDir.
func New(logsDir func(sandboxID string) string) *Store {
	return &Store{logsDir: logsDir}
}

func (s *Store) execDir(sandboxID string) string {
	return filepath.Join(s.logsDir(sandboxID), "exec")
}

// AppendEvent implements audit.Store.
func (s *Store) AppendEvent(_ context.Context, sandboxID string, event sandbox.Event) error {
	return sandbox.AppendEvent(s.logsDir(sandboxID), event)
}

// AppendExec implements audit.Store.
func (s *Store) AppendExec(_ context.Context, sandboxID string, exec sandbox.ExecRecord) error {
	return sandbox.WriteExecRecord(s.execDir(sandboxID), exec)
}

// Events implements audit.Store.
func (s *Store) Events(_ context.Context, sandboxID string) ([]sandbox.Event, error) {
	return sandbox.ReadEvents(s.logsDir(sandboxID))
}

// Execs implements audit.Store.
func (s *Store) Execs(_ context.Context, sandboxID string) ([]sandbox.ExecRecord, error) {
	return sandbox.ReadExecRecords(s.execDir(sandboxID))
}

// AppendEgress implements audit.Store.
const maxEgressLogBytes int64 = 1 << 20

func (s *Store) AppendEgress(_ context.Context, sandboxID string, record sandbox.EgressRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.logsDir(sandboxID)
	marker := filepath.Join(dir, "egress.truncated")
	if _, err := os.Stat(marker); err == nil {
		if record.Allowed {
			return fmt.Errorf("audit: egress log truncated; refusing unaudited allowed traffic")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	path := filepath.Join(dir, sandbox.EgressFile)
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// Reserve room for a terminal marker even when the next record is large.
	if size+int64(len(data))+1024 > maxEgressLogBytes {
		if record.Allowed {
			return fmt.Errorf("audit: egress log full; refusing unaudited allowed traffic")
		}
		if err := sandbox.AppendEgress(dir, sandbox.EgressRecord{SandboxID: sandboxID, Reason: "audit_truncated"}); err != nil {
			return err
		}
		return os.WriteFile(marker, []byte("audit_truncated\n"), 0o600)
	}
	return sandbox.AppendEgress(dir, record)
}

// Egress implements audit.Store.
func (s *Store) Egress(_ context.Context, sandboxID string) ([]sandbox.EgressRecord, error) {
	return sandbox.ReadEgress(s.logsDir(sandboxID))
}

// Close implements audit.Store.
func (s *Store) Close() error { return nil }
