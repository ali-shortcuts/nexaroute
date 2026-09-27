// Package usage records cumulative token accounting per deployment so the
// gateway can expose real prompt/completion consumption and estimated spend.
//
// Design constraints mirror the rest of NexaRoute: bounded memory, no
// goroutines, one mutex, O(1) Record on the hot path, and a Snapshot that
// never returns more entries than deployments known to the router (the
// router-driven Retain call trims entries for removed deployments).
package usage

import (
	"sort"
	"sync"
)

// Totals is the cumulative token record for one deployment.
type Totals struct {
	Deployment    string  `json:"deployment"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	Requests      int64   `json:"requests"`
	EstimatedCost float64 `json:"estimated_cost_usd"`
}

// Snapshot is the point-in-time view of cumulative usage.
type Snapshot struct {
	TotalPromptTokens     int64    `json:"total_prompt_tokens"`
	TotalCompletionTokens int64    `json:"total_completion_tokens"`
	TotalRequests         int64    `json:"total_requests"`
	TotalEstimatedCostUSD float64  `json:"total_estimated_cost_usd"`
	ByDeployment          []Totals `json:"by_deployment"`
}

// Price is the per-million-token pricing used to estimate cost at snapshot
// time. Pricing is resolved from the live config when a snapshot is built, so
// price edits take effect immediately and are not baked into counters.
type Price struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Tracker is the concurrency-safe cumulative token accountant.
type Tracker struct {
	mu         sync.Mutex
	byDeploy   map[string]*Totals
	prompt     int64
	completion int64
	requests   int64
}

// maxDeployments bounds memory; the real deployment count is already capped
// by config at 20000, so this cannot be exceeded by legitimate traffic.
const maxDeployments = 20000

// maxTokensPerRecord bounds one observed token count.
//
// Upstream usage values are untrusted input: a broken or hostile provider can
// report counts that no real response could produce (MaxInt64, or values whose
// running sum passes MaxInt64). Accumulating them verbatim wraps int64 and
// turns cumulative accounting — and every cost estimate derived from it —
// negative. No single response can exceed this ceiling, so a larger report is
// clamped to it instead of corrupting the counters.
const maxTokensPerRecord = int64(1) << 40

// sanitizeTokenCount clamps one reported token count into [0, maxTokensPerRecord].
// Non-positive values are dropped, which keeps probes and failed attempts out of
// accounting exactly as before.
func sanitizeTokenCount(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v > maxTokensPerRecord {
		return maxTokensPerRecord
	}
	return v
}

// saturatingAdd adds a non-negative delta to a cumulative counter and pins the
// result at the int64 ceiling instead of wrapping to negative.
func saturatingAdd(total, delta int64) int64 {
	const maxInt64 = int64(1<<63 - 1)
	if delta <= 0 {
		return total
	}
	if total > maxInt64-delta {
		return maxInt64
	}
	return total + delta
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{byDeploy: map[string]*Totals{}}
}

// Record accumulates one observed response. Reported counts are clamped first
// (see maxTokensPerRecord) and then added with saturation, so an upstream that
// reports absurd token counts cannot wrap the cumulative counters. Non-positive
// token counts are ignored so synthetic probes and failed attempts never pollute
// accounting.
func (t *Tracker) Record(deploymentID string, promptTokens, completionTokens int64) {
	promptTokens = sanitizeTokenCount(promptTokens)
	completionTokens = sanitizeTokenCount(completionTokens)
	if promptTokens == 0 && completionTokens == 0 {
		return
	}
	if deploymentID == "" {
		deploymentID = "unknown"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tot, ok := t.byDeploy[deploymentID]
	if !ok {
		if len(t.byDeploy) >= maxDeployments {
			return
		}
		tot = &Totals{Deployment: deploymentID}
		t.byDeploy[deploymentID] = tot
	}
	if promptTokens > 0 {
		tot.PromptTokens = saturatingAdd(tot.PromptTokens, promptTokens)
		t.prompt = saturatingAdd(t.prompt, promptTokens)
	}
	if completionTokens > 0 {
		tot.CompletionTok = saturatingAdd(tot.CompletionTok, completionTokens)
		t.completion = saturatingAdd(t.completion, completionTokens)
	}
	tot.Requests++
	t.requests++
}

// Snapshot returns cumulative totals. Cost is estimated per deployment from
// the provided price table (deployments missing from the table have zero
// cost). The result is sorted by deployment ID for stable UI rendering.
func (t *Tracker) Snapshot(prices map[string]Price) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := Snapshot{
		TotalPromptTokens:     t.prompt,
		TotalCompletionTokens: t.completion,
		TotalRequests:         t.requests,
		ByDeployment:          make([]Totals, 0, len(t.byDeploy)),
	}
	var totalCost float64
	for _, tot := range t.byDeploy {
		row := *tot
		if p, ok := prices[row.Deployment]; ok {
			row.EstimatedCost = (float64(row.PromptTokens)/1_000_000)*p.InputPerMTok +
				(float64(row.CompletionTok)/1_000_000)*p.OutputPerMTok
			if row.EstimatedCost < 0 {
				row.EstimatedCost = 0
			}
		}
		totalCost += row.EstimatedCost
		out.ByDeployment = append(out.ByDeployment, row)
	}
	sort.Slice(out.ByDeployment, func(i, j int) bool {
		return out.ByDeployment[i].Deployment < out.ByDeployment[j].Deployment
	})
	out.TotalEstimatedCostUSD = totalCost
	return out
}

// Retain drops entries for deployments that no longer exist. Called after hot
// reload so removed deployments cannot leak accounting state. Global counters
// are rebalanced so totals always equal the sum of the surviving rows.
func (t *Tracker) Retain(valid map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.byDeploy {
		if _, ok := valid[id]; !ok {
			delete(t.byDeploy, id)
		}
	}
	t.prompt, t.completion, t.requests = 0, 0, 0
	for _, tot := range t.byDeploy {
		t.prompt = saturatingAdd(t.prompt, tot.PromptTokens)
		t.completion = saturatingAdd(t.completion, tot.CompletionTok)
		t.requests = saturatingAdd(t.requests, tot.Requests)
	}
}

// Reset clears all accounting.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byDeploy = map[string]*Totals{}
	t.prompt = 0
	t.completion = 0
	t.requests = 0
}
