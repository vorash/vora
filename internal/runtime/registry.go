package runtime

import (
	"fmt"
	"sort"
	"sync"
)

// Registry maps backend names to implementations.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
	def      string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Backend)}
}

// Register adds a backend. Registering a name twice is an error. The first
// backend registered becomes the default.
func (r *Registry) Register(b Backend) error {
	if b == nil {
		return fmt.Errorf("runtime: cannot register nil backend")
	}
	name := b.Name()
	if name == "" {
		return fmt.Errorf("runtime: backend name is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.backends[name]; exists {
		return fmt.Errorf("runtime: backend %q already registered", name)
	}
	r.backends[name] = b
	if r.def == "" {
		r.def = name
	}
	return nil
}

// Get returns the backend registered under name.
func (r *Registry) Get(name string) (Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.backends[name]
	return b, ok
}

// Default returns the default backend (the first registered).
func (r *Registry) Default() (Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.def == "" {
		return nil, false
	}
	b, ok := r.backends[r.def]
	return b, ok
}

// Names returns the registered backend names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
