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

// APIKey is a hashed caller credential. An empty AgentID is a plain tenant
// key; a bound key may only act as its agent.
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TenantID  string    `json:"tenant_id"`
	AgentID   string    `json:"agent_id,omitempty"`
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
	Tunneled      bool      `json:"tunneled,omitempty"`
	Backends      []string  `json:"backends"`
	NetworkModes  []string  `json:"network_modes,omitempty"`
	CPUMillis     int64     `json:"cpu_millis"`
	MemoryBytes   int64     `json:"memory_bytes"`
	MaxSandboxes  int32     `json:"max_sandboxes"`
	Running       int32     `json:"sandboxes_running"`
	Version       string    `json:"version"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// SupportsBackend reports whether the runner can host the given backend. An
// empty backend means auto-select, so any runner with a backend qualifies.
func (r Runner) SupportsBackend(backend string) bool {
	if backend == "" {
		return len(r.Backends) > 0
	}
	for _, b := range r.Backends {
		if b == backend {
			return true
		}
	}
	return false
}

// SupportsNetwork reports whether the runner can enforce a network mode. A
// runner that did not report modes is assumed to support only "none".
func (r Runner) SupportsNetwork(mode string) bool {
	if mode == "" || mode == "none" {
		return true
	}
	for _, m := range r.NetworkModes {
		if m == mode {
			return true
		}
	}
	return false
}

// Healthy reports whether the runner heartbeat is within ttl.
func (r Runner) Healthy(now time.Time, ttl time.Duration) bool {
	return !r.LastHeartbeat.IsZero() && now.Sub(r.LastHeartbeat) <= ttl
}

// SandboxRoute records which runner owns a sandbox, which tenant may access
// it, and the resources it was created with (so placement can account for a
// runner's committed load without trusting heartbeats). An empty TenantID is
// reserved for global-admin-owned sandboxes.
type SandboxRoute struct {
	SandboxID   string `json:"sandbox_id"`
	RunnerID    string `json:"runner_id"`
	TenantID    string `json:"tenant_id"`
	AgentID     string `json:"agent_id,omitempty"`
	CPUMillis   int64  `json:"cpu_millis,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
	// Destroyed marks a route kept after destroy so the sandbox's audit trail
	// stays reachable through the API. It no longer counts toward placement.
	Destroyed bool `json:"destroyed,omitempty"`
}

// SnapshotRoute records which runner holds a snapshot and which tenant may
// access it. The sandbox it was captured from may already be gone.
type SnapshotRoute struct {
	SnapshotID string `json:"snapshot_id"`
	RunnerID   string `json:"runner_id"`
	TenantID   string `json:"tenant_id"`
	AgentID    string `json:"agent_id,omitempty"`
	SandboxID  string `json:"sandbox_id"`
}

// PoolRoute is the API's record of a pool: which runner holds it, who may use
// it, and what it reserves. It is written before the runner is asked for the
// pool and removed only once the runner says the pool is gone, so a pool the
// runner may hold always has a route, and capacity is reserved for as long as
// the route exists: MaxSize members of CPUMillis and MemoryBytes each. Members
// get no SandboxRoute, so a runner replacing them never moves these books.
type PoolRoute struct {
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	PoolID      string `json:"pool_id"`
	RunnerID    string `json:"runner_id"`
	AgentID     string `json:"agent_id,omitempty"`
	MaxSize     int32  `json:"max_size"`
	CPUMillis   int64  `json:"cpu_millis"`
	MemoryBytes int64  `json:"memory_bytes"`
}

// Store persists control-plane state.
type Store interface {
	CreateKey(ctx context.Context, key APIKey) error
	ListKeys(ctx context.Context, tenant string) ([]APIKey, error)
	Key(ctx context.Context, id string) (APIKey, error)
	KeyByHash(ctx context.Context, hash string) (APIKey, error)
	RevokeKey(ctx context.Context, id string) error
	TouchKey(ctx context.Context, id string, at time.Time) error

	UpsertRunner(ctx context.Context, r Runner) error
	ListRunners(ctx context.Context) ([]Runner, error)
	Runner(ctx context.Context, id string) (Runner, error)

	SetRoute(ctx context.Context, route SandboxRoute) error
	Route(ctx context.Context, sandboxID string) (SandboxRoute, error)
	ListRoutes(ctx context.Context) ([]SandboxRoute, error)
	DeleteRoute(ctx context.Context, sandboxID string) error

	SetSnapshotRoute(ctx context.Context, route SnapshotRoute) error
	SnapshotRoute(ctx context.Context, snapshotID string) (SnapshotRoute, error)
	DeleteSnapshotRoute(ctx context.Context, snapshotID string) error

	// InsertPoolRoute fails with ErrPoolRouteExists when (tenant, name) is taken.
	InsertPoolRoute(ctx context.Context, route PoolRoute) error
	// SetPoolRoute rewrites a route only while it names expectPoolID or already
	// names route.PoolID, and otherwise fails with ErrPoolRouteChanged, including
	// when the route is gone. Two declares answered with one pool both succeed.
	SetPoolRoute(ctx context.Context, route PoolRoute, expectPoolID string) error
	PoolRoute(ctx context.Context, tenant, name string) (PoolRoute, error)
	// ListPoolRoutes lists one tenant's routes, or every route for an empty
	// tenant, sorted by tenant and name.
	ListPoolRoutes(ctx context.Context, tenant string) ([]PoolRoute, error)
	// DeletePoolRoute removes the route only while it still names poolID, so a
	// delete cannot remove the route of a pool declared after it began.
	DeletePoolRoute(ctx context.Context, tenant, name, poolID string) error

	Close() error
}
