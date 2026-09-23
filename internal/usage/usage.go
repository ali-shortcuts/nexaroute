package usage

import (
	"sync"
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

// RecordKeyRequest counts an authorized data-plane admission even when token
// usage cannot be parsed from the response.
func (t *Tracker) RecordKeyRequest(keyName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total.Requests++
	if keyName != "" {
		e := t.byKey[keyName]
		e.Requests++
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
}
