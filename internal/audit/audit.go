// Package audit defines the storage interface for sandbox lifecycle events and
// execution records. Implementations include a local JSONL store and a POSIX/
// Supabase PostgreSQL store.
package audit

import (
	"context"

	"github.com/vorash/vora/internal/sandbox"
)

// Store persists and reads sandbox audit data.
type Store interface {
	// AppendEvent records a lifecycle transition for a sandbox.
	AppendEvent(ctx context.Context, sandboxID string, event sandbox.Event) error
	// AppendExec records an execution for a sandbox.
	AppendExec(ctx context.Context, sandboxID string, exec sandbox.ExecRecord) error
	// Events returns a sandbox's lifecycle events, oldest first.
	Events(ctx context.Context, sandboxID string) ([]sandbox.Event, error)
	// Execs returns a sandbox's execution records, oldest first.
	Execs(ctx context.Context, sandboxID string) ([]sandbox.ExecRecord, error)
	// AppendEgress records one restricted-network policy decision.
	AppendEgress(ctx context.Context, sandboxID string, record sandbox.EgressRecord) error
	// Egress returns policy decisions oldest first.
	Egress(ctx context.Context, sandboxID string) ([]sandbox.EgressRecord, error)
	// Close releases resources.
	Close() error
}
