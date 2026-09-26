package decision

import (
	"fmt"
	"sync"
)

// Registry holds DecisionProviders by ID.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]DecisionProvider
}

// NewRegistry creates an empty registry with local provider pre-registered.
func NewRegistry() *Registry {
	r := &Registry{providers: map[string]DecisionProvider{}}
	r.Register(&LocalProvider{})
	return r
}

// Register adds a provider; last wins for same ID (but logs should warn).
func (r *Registry) Register(p DecisionProvider) {
	if p == nil {
		return
	}
	id := p.ID()
	if id == "" {
		return
	}
	r.mu.Lock()
	r.providers[id] = p
	r.mu.Unlock()
}

// Get returns provider by ID.
func (r *Registry) Get(id string) (DecisionProvider, bool) {
	r.mu.RLock()
	p, ok := r.providers[id]
	r.mu.RUnlock()
	return p, ok
}

// List returns all provider IDs.
func (r *Registry) List() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.providers))
	for id := range r.providers {
		out = append(out, id)
	}
	r.mu.RUnlock()
	return out
}

// Snapshot returns provider health snapshot for admin.
func (r *Registry) Snapshot() map[string]ProviderHealth {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]ProviderHealth, len(r.providers))
	for id, p := range r.providers {
		out[id] = p.Health()
	}
	return out
}

// Resolve returns provider for given name, or error if unknown.
// Empty name defaults to "local".
func (r *Registry) Resolve(name string) (DecisionProvider, error) {
	if name == "" {
		name = "local"
	}
	r.mu.RLock()
	p, ok := r.providers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("decision provider %q not found", name)
	}
	return p, nil
}
