package usage

import (
	"math"
	"sort"
	"sync"
)

type Sample struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	ReasoningTokens          int64 `json:"reasoning_tokens,omitempty"`
}

const maxTokensPerObservation int64 = 1_000_000_000_000

func (s Sample) Valid() bool {
	for _, v := range []int64{s.InputTokens, s.OutputTokens, s.CacheReadInputTokens, s.CacheCreationInputTokens, s.ReasoningTokens} {
		if v < 0 || v > maxTokensPerObservation {
			return false
		}
	}
	return true
}

type Pricing struct {
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
}

func (p Pricing) Configured() bool {
	return isFiniteNonNegative(p.InputUSDPerMillion) &&
		isFiniteNonNegative(p.OutputUSDPerMillion)
}

func isFiniteNonNegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

type Stats struct {
	Deployment               string  `json:"deployment,omitempty"`
	Provider                 string  `json:"provider,omitempty"`
	ExactRequests            uint64  `json:"exact_requests"`
	UnknownRequests          uint64  `json:"unknown_requests"`
	PricedRequests           uint64  `json:"priced_requests"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	ReasoningTokens          int64   `json:"reasoning_tokens"`
	EstimatedCostNanoUSD     int64   `json:"estimated_cost_nano_usd"`
	EstimatedCostUSD         float64 `json:"estimated_cost_usd"`
}

func saturatingAddInt64(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func (s *Stats) addSample(sample Sample, pricing *Pricing) {
	s.ExactRequests++
	s.InputTokens = saturatingAddInt64(s.InputTokens, sample.InputTokens)
	s.OutputTokens = saturatingAddInt64(s.OutputTokens, sample.OutputTokens)
	s.CacheReadInputTokens = saturatingAddInt64(s.CacheReadInputTokens, sample.CacheReadInputTokens)
	s.CacheCreationInputTokens = saturatingAddInt64(s.CacheCreationInputTokens, sample.CacheCreationInputTokens)
	s.ReasoningTokens = saturatingAddInt64(s.ReasoningTokens, sample.ReasoningTokens)
	if pricing != nil && pricing.Configured() {
		// USD-per-million * tokens * 1e9 nanos/USD = tokens * price * 1000.
		cost := float64(sample.InputTokens)*pricing.InputUSDPerMillion*1000 +
			float64(sample.OutputTokens)*pricing.OutputUSDPerMillion*1000
		if cost >= 0 && cost <= math.MaxInt64 {
			n := int64(math.Round(cost))
			s.EstimatedCostNanoUSD = saturatingAddInt64(s.EstimatedCostNanoUSD, n)
			s.EstimatedCostUSD = float64(s.EstimatedCostNanoUSD) / 1e9
			s.PricedRequests++
		}
	}
}

type Manager struct {
	mu          sync.RWMutex
	deployments map[string]Stats
	providers   map[string]Stats
	total       Stats
}

func New() *Manager {
	return &Manager{
		deployments: map[string]Stats{},
		providers:   map[string]Stats{},
	}
}

func (m *Manager) Record(deployment, provider string, sample Sample, pricing *Pricing) {
	if m == nil || deployment == "" || provider == "" || !sample.Valid() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ds := m.deployments[deployment]
	ds.Deployment = deployment
	ds.Provider = provider
	ds.addSample(sample, pricing)
	m.deployments[deployment] = ds

	ps := m.providers[provider]
	ps.Provider = provider
	ps.addSample(sample, pricing)
	m.providers[provider] = ps

	m.total.addSample(sample, pricing)
}

func (m *Manager) RecordUnknown(deployment, provider string) {
	if m == nil || deployment == "" || provider == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ds := m.deployments[deployment]
	ds.Deployment = deployment
	ds.Provider = provider
	ds.UnknownRequests++
	m.deployments[deployment] = ds
	ps := m.providers[provider]
	ps.Provider = provider
	ps.UnknownRequests++
	m.providers[provider] = ps
	m.total.UnknownRequests++
}

func (m *Manager) Snapshot() []Stats {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Stats, 0, len(m.deployments))
	for _, st := range m.deployments {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deployment < out[j].Deployment })
	return out
}

func (m *Manager) ProviderSnapshot() []Stats {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Stats, 0, len(m.providers))
	for _, st := range m.providers {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func (m *Manager) Total() Stats {
	if m == nil {
		return Stats{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.total
}

func (m *Manager) Retain(validDeployments, validProviders map[string]struct{}) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.deployments {
		if _, ok := validDeployments[id]; !ok {
			delete(m.deployments, id)
		}
	}
	for id := range m.providers {
		if _, ok := validProviders[id]; !ok {
			delete(m.providers, id)
		}
	}
}
