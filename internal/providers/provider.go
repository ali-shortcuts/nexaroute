package providers

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type ProviderStats struct {
	ID                         string `json:"id"`
	MaxConcurrency             int    `json:"max_concurrency"`
	ActiveRequests             int64  `json:"active_requests"`
	WaitingRequests            int64  `json:"waiting_requests"`
	Credentials                int    `json:"credentials"`
	CredentialsCooling         int    `json:"credentials_cooling"`
	RequestLimit               int64  `json:"request_limit"`
	RemainingRequests          int64  `json:"remaining_requests"`
	ReservedRequests           int64  `json:"reserved_requests"`
	EffectiveRemainingRequests int64  `json:"effective_remaining_requests"`
	TokenLimit                 int64  `json:"token_limit"`
	RemainingTokens            int64  `json:"remaining_tokens"`
	ReservedTokens             int64  `json:"reserved_tokens"`
	EffectiveRemainingTokens   int64  `json:"effective_remaining_tokens"`
	RequestResetUnix           int64  `json:"request_reset_unix,omitempty"`
	TokenResetUnix             int64  `json:"token_reset_unix,omitempty"`
	RateLimitResetUnix         int64  `json:"rate_limit_reset_unix,omitempty"`
}

type quotaEstimateContextKey struct{}

const maxQuotaEstimateTokens int64 = 1_000_000_000_000

// WithQuotaEstimate marks a data-plane upstream attempt with a conservative
// token bound. Adapters use it only as an in-flight reservation overlay on
// provider-reported quota; it never hard-blocks a request by itself.
func WithQuotaEstimate(ctx context.Context, estimatedInputTokens, maxOutputTokens int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	var total int64
	for _, v := range []int{estimatedInputTokens, maxOutputTokens} {
		if v <= 0 {
			continue
		}
		x := int64(v)
		if x > maxQuotaEstimateTokens-total {
			total = maxQuotaEstimateTokens
			break
		}
		total += x
	}
	return context.WithValue(ctx, quotaEstimateContextKey{}, total)
}

func quotaEstimateFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	v, ok := ctx.Value(quotaEstimateContextKey{}).(int64)
	if !ok {
		return 0, false
	}
	if v < 0 {
		v = 0
	}
	if v > maxQuotaEstimateTokens {
		v = maxQuotaEstimateTokens
	}
	return v, true
}

type Adapter interface {
	ID() string
	Kind() string
	Stats() ProviderStats
	CredentialsMatch([]string) bool
	RedactBody([]byte) []byte
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
		a, err := NewAdapterWithRetryAfterCap(p, cfg.RequestTimeout(), time.Duration(cfg.Routing.MaxRetryAfterSeconds)*time.Second)
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

func (r *Registry) Stat(id string) (ProviderStats, bool) {
	r.mu.RLock()
	a, ok := r.m[id]
	r.mu.RUnlock()
	if !ok {
		return ProviderStats{}, false
	}
	return a.Stats(), true
}

func (r *Registry) CredentialsMatchProvider(p config.ProviderConfig) bool {
	r.mu.RLock()
	a, ok := r.m[p.ID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return a.CredentialsMatch(p.ResolvedCredentials())
}

func CloseIdleConnections(a Adapter) {
	if closer, ok := a.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
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
	return NewAdapterWithRetryAfterCap(p, timeout, 60*time.Second)
}

func NewAdapterWithRetryAfterCap(p config.ProviderConfig, timeout, retryAfterCap time.Duration) (Adapter, error) {
	p.ApplyDefaults()
	if err := config.ValidateProviderConfig(p); err != nil {
		return nil, err
	}
	return newHTTPAdapterWithRetryCap(p, timeout, retryAfterCap)
}
