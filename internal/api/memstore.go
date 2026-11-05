package api

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("api: not found")

// ErrPoolRouteExists is returned when a tenant already has a pool of that name.
var ErrPoolRouteExists = errors.New("api: pool route exists")

// MemStore is an in-memory Store used for development and tests.
type MemStore struct {
	mu         sync.RWMutex
	keys       map[string]APIKey
	runners    map[string]Runner
	routes     map[string]SandboxRoute
	snapRoutes map[string]SnapshotRoute
	poolRoutes map[poolRouteKey]PoolRoute
}

type poolRouteKey struct{ tenant, name string }

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		keys:       map[string]APIKey{},
		runners:    map[string]Runner{},
		routes:     map[string]SandboxRoute{},
		snapRoutes: map[string]SnapshotRoute{},
		poolRoutes: map[poolRouteKey]PoolRoute{},
	}
}

func (m *MemStore) CreateKey(_ context.Context, key APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key.ID] = key
	return nil
}

func (m *MemStore) ListKeys(_ context.Context, tenant string) ([]APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []APIKey
	for _, k := range m.keys {
		if tenant != "" && k.TenantID != tenant {
			continue
		}
		k.KeyHash = ""
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) Key(_ context.Context, id string) (APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.keys[id]
	if !ok {
		return APIKey{}, ErrNotFound
	}
	key.KeyHash = ""
	return key, nil
}

func (m *MemStore) KeyByHash(_ context.Context, hash string) (APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, k := range m.keys {
		if k.KeyHash == hash {
			return k, nil
		}
	}
	return APIKey{}, ErrNotFound
}

func (m *MemStore) RevokeKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return ErrNotFound
	}
	k.Revoked = true
	m.keys[id] = k
	return nil
}

func (m *MemStore) TouchKey(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return ErrNotFound
	}
	k.LastUsed = at
	m.keys[id] = k
	return nil
}

func (m *MemStore) UpsertRunner(_ context.Context, r Runner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.runners[r.ID]; ok {
		if r.RegisteredAt.IsZero() {
			r.RegisteredAt = prev.RegisteredAt
		}
	}
	m.runners[r.ID] = r
	return nil
}

func (m *MemStore) ListRunners(_ context.Context) ([]Runner, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Runner, 0, len(m.runners))
	for _, r := range m.runners {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) Runner(_ context.Context, id string) (Runner, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runners[id]
	if !ok {
		return Runner{}, ErrNotFound
	}
	return r, nil
}

func (m *MemStore) SetRoute(_ context.Context, route SandboxRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[route.SandboxID] = route
	return nil
}

func (m *MemStore) Route(_ context.Context, sandboxID string) (SandboxRoute, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	route, ok := m.routes[sandboxID]
	if !ok {
		return SandboxRoute{}, ErrNotFound
	}
	return route, nil
}

func (m *MemStore) DeleteRoute(_ context.Context, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, sandboxID)
	return nil
}

func (m *MemStore) ListRoutes(_ context.Context) ([]SandboxRoute, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SandboxRoute, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	return out, nil
}

func (m *MemStore) SetSnapshotRoute(_ context.Context, route SnapshotRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapRoutes[route.SnapshotID] = route
	return nil
}

func (m *MemStore) SnapshotRoute(_ context.Context, snapshotID string) (SnapshotRoute, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	route, ok := m.snapRoutes[snapshotID]
	if !ok {
		return SnapshotRoute{}, ErrNotFound
	}
	return route, nil
}

func (m *MemStore) DeleteSnapshotRoute(_ context.Context, snapshotID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snapRoutes, snapshotID)
	return nil
}

func (m *MemStore) InsertPoolRoute(_ context.Context, route PoolRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := poolRouteKey{route.TenantID, route.Name}
	if _, taken := m.poolRoutes[key]; taken {
		return ErrPoolRouteExists
	}
	m.poolRoutes[key] = route
	return nil
}

func (m *MemStore) SetPoolRoute(_ context.Context, route PoolRoute) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolRoutes[poolRouteKey{route.TenantID, route.Name}] = route
	return nil
}

func (m *MemStore) PoolRoute(_ context.Context, tenant, name string) (PoolRoute, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	route, ok := m.poolRoutes[poolRouteKey{tenant, name}]
	if !ok {
		return PoolRoute{}, ErrNotFound
	}
	return route, nil
}

func (m *MemStore) ListPoolRoutes(_ context.Context, tenant string) ([]PoolRoute, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PoolRoute, 0, len(m.poolRoutes))
	for _, route := range m.poolRoutes {
		if tenant == "" || route.TenantID == tenant {
			out = append(out, route)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *MemStore) DeletePoolRoute(_ context.Context, tenant, name, poolID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := poolRouteKey{tenant, name}
	if route, ok := m.poolRoutes[key]; ok && route.PoolID == poolID {
		delete(m.poolRoutes, key)
	}
	return nil
}

// Close implements Store.
func (m *MemStore) Close() error { return nil }
