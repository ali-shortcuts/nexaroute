package decision

import (
	"fmt"
	"sort"
	"sync"
)

// Registry resolves configured DecisionProvider IDs to providers. The ID is
// operator-chosen (e.g. "jev-main"); the adapter type (e.g. "jev") is
// separate, so future "jev-eu" or "custom-foo" IDs need no architecture
// changes. Registries are built immutably per config generation and swapped
// atomically on hot reload.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]DecisionProvider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]DecisionProvider{}}
}

// Register adds a provider. Empty IDs, nil providers, and duplicate IDs are
// rejected: ambiguous wiring must fail before any request executes.
func (r *Registry) Register(p DecisionProvider) error {
	if p == nil {
		return fmt.Errorf("decision: cannot register nil provider")
	}
	if p.ID() == "" {
		return fmt.Errorf("decision: cannot register provider with empty ID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[p.ID()]; dup {
		return fmt.Errorf("decision: duplicate provider ID %q", p.ID())
	}
	r.providers[p.ID()] = p
	return nil
}

// Get resolves a configured provider ID.
func (r *Registry) Get(id string) (DecisionProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// All returns every registered provider in ID order (for admin snapshots).
func (r *Registry) All() []DecisionProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DecisionProvider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Len returns the number of registered providers.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}
