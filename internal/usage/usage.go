package usage

import (
	"sync"
	"time"
)

// Entry aggregates requests, token counts and an estimated USD cost for one
// scope (a provider deployment prefix or a client key name).
type Entry struct {
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	EstCostUSD   float64 `json:"est_cost_usd"`
}

func (e *Entry) add(other Entry) {
	e.Requests += other.Requests
	e.InputTokens += other.InputTokens
	e.OutputTokens += other.OutputTokens
	e.EstCostUSD += other.EstCostUSD
}

// Snapshot is a point-in-time copy of all tracked usage.
type Snapshot struct {
	Totals     Entry            `json:"totals"`
	ByProvider map[string]Entry `json:"by_provider"`
	ByKey      map[string]Entry `json:"by_key"`
}

// Tracker keeps bounded in-memory usage counters. State is intentionally
// process-local: restarts reset it and multi-process deployments track
// independently.
type Tracker struct {
	mu         sync.RWMutex
	total      Entry
	byProvider map[string]Entry
	byKey      map[string]Entry
	recent     []RequestRecord
}

func New() *Tracker {
	return &Tracker{byProvider: map[string]Entry{}, byKey: map[string]Entry{}}
}

// RecordRequest attributes one completed data-plane request to a provider
// deployment id and a client key name.
func (t *Tracker) RecordRequest(providerID, keyName string, inputTokens, outputTokens int64, estCostUSD float64) {
	delta := Entry{Requests: 1, InputTokens: inputTokens, OutputTokens: outputTokens, EstCostUSD: estCostUSD}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total.add(delta)
	if providerID != "" {
		e := t.byProvider[providerID]
		e.add(delta)
		t.byProvider[providerID] = e
	}
	if keyName != "" {
		e := t.byKey[keyName]
		e.add(delta)
		t.byKey[keyName] = e
	}
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := Snapshot{Totals: t.total, ByProvider: make(map[string]Entry, len(t.byProvider)), ByKey: make(map[string]Entry, len(t.byKey))}
	for k, v := range t.byProvider {
		out.ByProvider[k] = v
	}
	for k, v := range t.byKey {
		out.ByKey[k] = v
	}
	return out
}

func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = Entry{}
	t.byProvider = map[string]Entry{}
	t.byKey = map[string]Entry{}
	t.recent = nil
}

// RequestRecord is one completed data-plane request with its resolved route,
// latency, token usage and estimated cost — the "what actually happened" log.
type RequestRecord struct {
	Time         time.Time `json:"time"`
	RequestID    string    `json:"request_id"`
	Path         string    `json:"path"`
	Model        string    `json:"model"`
	Deployment   string    `json:"deployment"`
	ProviderName string    `json:"provider_name,omitempty"`
	KeyName      string    `json:"key_name"`
	Status       int       `json:"status"`
	LatencyMS    int64     `json:"latency_ms"`
	Stream       bool      `json:"stream"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	EstCostUSD   float64   `json:"est_cost_usd"`
	Error        string    `json:"error,omitempty"`
}

const maxRecentRequests = 500

// Recent stores the bounded ring of recent requests (newest last).
func (t *Tracker) Recent(r RequestRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.recent) == maxRecentRequests {
		copy(t.recent, t.recent[1:])
		t.recent[len(t.recent)-1] = r
		return
	}
	t.recent = append(t.recent, r)
}

// RecentRequests returns a copy of the ring, newest first, up to limit
// entries (limit <= 0 means all).
func (t *Tracker) RecentRequests(limit int) []RequestRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]RequestRecord, 0, len(t.recent))
	for i := len(t.recent) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, t.recent[i])
	}
	return out
}
