package api

import (
	"context"
	"time"
)

// Scopes gate API access.
const (
	ScopeAdmin   = "admin"
	ScopeSandbox = "sandbox"
)

// APIKey is a hashed caller credential.
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TenantID  string    `json:"tenant_id"`
	KeyHash   string    `json:"-"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	Revoked   bool      `json:"revoked"`
}

// HasScope reports whether the key carries a scope.
func (k APIKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Runner is a registered execution host.
type Runner struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	Backends      []string  `json:"backends"`
	CPUMillis     int64     `json:"cpu_millis"`
	MemoryBytes   int64     `json:"memory_bytes"`
	MaxSandboxes  int32     `json:"max_sandboxes"`
	Running       int32     `json:"sandboxes_running"`
	Version       string    `json:"version"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// Healthy reports whether the runner heartbeat is within ttl.
func (r Runner) Healthy(now time.Time, ttl time.Duration) bool {
	return !r.LastHeartbeat.IsZero() && now.Sub(r.LastHeartbeat) <= ttl
}

// Store persists control-plane state.
type Store interface {
	CreateKey(ctx context.Context, key APIKey) error
	ListKeys(ctx context.Context, tenant string) ([]APIKey, error)
	KeyByHash(ctx context.Context, hash string) (APIKey, error)
	RevokeKey(ctx context.Context, id string) error
	TouchKey(ctx context.Context, id string, at time.Time) error

	UpsertRunner(ctx context.Context, r Runner) error
	ListRunners(ctx context.Context) ([]Runner, error)
	Runner(ctx context.Context, id string) (Runner, error)

	SetRoute(ctx context.Context, sandboxID, runnerID string) error
	Route(ctx context.Context, sandboxID string) (string, error)
	DeleteRoute(ctx context.Context, sandboxID string) error

	Close() error
}
