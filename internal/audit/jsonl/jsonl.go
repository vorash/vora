// Package jsonl implements audit.Store using per-sandbox JSONL/JSON files on
// the local filesystem (events.jsonl + exec/<execution_id>.json).
package jsonl

import (
	"context"
	"path/filepath"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/sandbox"
)

// Store is a filesystem-backed audit store.
type Store struct {
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
func (s *Store) AppendEgress(_ context.Context, sandboxID string, record sandbox.EgressRecord) error {
	return sandbox.AppendEgress(s.logsDir(sandboxID), record)
}

// Egress implements audit.Store.
func (s *Store) Egress(_ context.Context, sandboxID string) ([]sandbox.EgressRecord, error) {
	return sandbox.ReadEgress(s.logsDir(sandboxID))
}

// Close implements audit.Store.
func (s *Store) Close() error { return nil }
