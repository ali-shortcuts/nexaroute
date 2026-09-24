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

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{byDeploy: map[string]*Totals{}}
}

// Record accumulates one observed response. Non-positive token counts are
// ignored so synthetic probes and failed attempts never pollute accounting.
func (t *Tracker) Record(deploymentID string, promptTokens, completionTokens int64) {
	if promptTokens <= 0 && completionTokens <= 0 {
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
		tot.PromptTokens += promptTokens
		t.prompt += promptTokens
	}
	if completionTokens > 0 {
		tot.CompletionTok += completionTokens
		t.completion += completionTokens
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
	for id, tot := range t.byDeploy {
		if _, ok := valid[id]; !ok {
			t.prompt -= tot.PromptTokens
			t.completion -= tot.CompletionTok
			t.requests -= tot.Requests
			if t.prompt < 0 {
				t.prompt = 0
			}
			if t.completion < 0 {
				t.completion = 0
			}
			if t.requests < 0 {
				t.requests = 0
			}
			delete(t.byDeploy, id)
		}
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
