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

func (r *Registry) Reload(cfg config.Config) error {
	next := map[string]Adapter{}
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		a, err := NewAdapter(p, cfg.RequestTimeout())
		if err != nil {
			return err
		}
		next[p.ID] = a
	}
	r.mu.Lock()
	r.m = next
	r.mu.Unlock()
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
	return newHTTPAdapter(p, timeout)
}
