package providers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type ProviderStats struct {
	ID                 string `json:"id"`
	MaxConcurrency     int    `json:"max_concurrency"`
	ActiveRequests     int64  `json:"active_requests"`
	WaitingRequests    int64  `json:"waiting_requests"`
	Credentials        int    `json:"credentials"`
	CredentialsCooling int    `json:"credentials_cooling"`
}

type Adapter interface {
	ID() string
	Kind() string
	Stats() ProviderStats
	Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error)
	DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error)
	CountTokens(ctx context.Context, payload []byte, forward http.Header) (*http.Response, error)
	Probe(ctx context.Context, model string, maxTokens int) (time.Duration, int, error)
}

type Registry struct {
	mu sync.RWMutex
	m  map[string]Adapter
}

func NewRegistry(cfg config.Config) (*Registry, error) {
	r := &Registry{m: map[string]Adapter{}}
	if err := r.Reload(cfg); err != nil {
		return nil, err
	}
	return r, nil
}

// Prepare builds a complete next registry without mutating the live one.
// When rebuild is non-nil, unchanged provider adapters are reused so their
// connection pools and credential cooldown state survive unrelated reloads.
func (r *Registry) Prepare(cfg config.Config, rebuild map[string]struct{}) (*Registry, error) {
	r.mu.RLock()
	current := make(map[string]Adapter, len(r.m))
	for id, a := range r.m {
		current[id] = a
	}
	r.mu.RUnlock()

	next := &Registry{m: make(map[string]Adapter)}
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		if rebuild != nil {
			if _, changed := rebuild[p.ID]; !changed {
				if a, ok := current[p.ID]; ok {
					next.m[p.ID] = a
					continue
				}
			}
		}
		a, err := NewAdapter(p, cfg.RequestTimeout())
		if err != nil {
			return nil, err
		}
		next.m[p.ID] = a
	}
	return next, nil
}

func (r *Registry) Replace(next *Registry) {
	if next == nil {
		return
	}
	next.mu.RLock()
	m := next.m
	next.mu.RUnlock()

	r.mu.Lock()
	r.m = m
	r.mu.Unlock()
}

func (r *Registry) Reload(cfg config.Config) error {
	next, err := r.Prepare(cfg, nil)
	if err != nil {
		return err
	}
	r.Replace(next)
	return nil
}
func (r *Registry) Get(id string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.m[id]
	return a, ok
}

func (r *Registry) Stats() []ProviderStats {
	r.mu.RLock()
	adapters := make([]Adapter, 0, len(r.m))
	for _, a := range r.m {
		adapters = append(adapters, a)
	}
	r.mu.RUnlock()
	out := make([]ProviderStats, 0, len(adapters))
	for _, a := range adapters {
		out = append(out, a.Stats())
	}
	return out
}
func NewAdapter(p config.ProviderConfig, timeout time.Duration) (Adapter, error) {
	p.ApplyDefaults()
	if err := config.ValidateProviderConfig(p); err != nil {
		return nil, err
	}
	return newHTTPAdapter(p, timeout)
}
