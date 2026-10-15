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
	// CacheReadTokens/CacheWriteTokens count prompt-cache tokens
	// reported by upstreams (M5-6); RecordedCostUSD is cost computed
	// AT RECORD TIME under the price version active then, with the
	// per-version split in CostByVersion. Recorded costs are
	// never repriced.
	CacheReadTokens  int64              `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64              `json:"cache_write_tokens,omitempty"`
	RecordedCostUSD  float64            `json:"recorded_cost_usd,omitempty"`
	CostByVersion    map[string]float64 `json:"cost_by_version_usd,omitempty"`
	// unpriced token counters back the legacy snapshot-time
	// fallback for records made without record-time costing.
	unpricedPrompt     int64
	unpricedCompletion int64
}

// Snapshot is the point-in-time view of cumulative usage.
type Snapshot struct {
	TotalPromptTokens     int64    `json:"total_prompt_tokens"`
	TotalCompletionTokens int64    `json:"total_completion_tokens"`
	TotalRequests         int64    `json:"total_requests"`
	TotalEstimatedCostUSD float64  `json:"total_estimated_cost_usd"`
	TotalCacheReadTokens  int64    `json:"total_cache_read_tokens,omitempty"`
	TotalCacheWriteTokens int64    `json:"total_cache_write_tokens,omitempty"`
	TotalRecordedCostUSD  float64  `json:"total_recorded_cost_usd,omitempty"`
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
	mu           sync.Mutex
	byDeploy     map[string]*Totals
	prompt       int64
	completion   int64
	requests     int64
	cacheRead    int64
	cacheWrite   int64
	recordedCost float64
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
	tot.unpricedPrompt = saturatingAdd(tot.unpricedPrompt, promptTokens)
	tot.unpricedCompletion = saturatingAdd(tot.unpricedCompletion, completionTokens)
}

// Counts is one usage report's token accounting.
type Counts struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// RecordEx accumulates one usage report WITH its record-time cost
// (already priced by the caller under the price version active at
// record time). The cost is attributed to `version` in the
// per-version split. Token counts are sanitized exactly like
// Record's.
func (t *Tracker) RecordEx(deploymentID string, c Counts, costUSD float64, version string) {
	c.Input = sanitizeTokenCount(c.Input)
	c.Output = sanitizeTokenCount(c.Output)
	c.CacheRead = sanitizeTokenCount(c.CacheRead)
	c.CacheWrite = sanitizeTokenCount(c.CacheWrite)
	if costUSD < 0 || costUSD != costUSD || costUSD > 1e18 {
		costUSD = 0
	}
	if c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0 && costUSD == 0 {
		return
	}
	if deploymentID == "" {
		deploymentID = "unknown"
	}
	if version == "" {
		version = "unpriced"
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
	tot.PromptTokens = saturatingAdd(tot.PromptTokens, c.Input)
	t.prompt = saturatingAdd(t.prompt, c.Input)
	tot.CompletionTok = saturatingAdd(tot.CompletionTok, c.Output)
	t.completion = saturatingAdd(t.completion, c.Output)
	tot.CacheReadTokens = saturatingAdd(tot.CacheReadTokens, c.CacheRead)
	t.cacheRead = saturatingAdd(t.cacheRead, c.CacheRead)
	tot.CacheWriteTokens = saturatingAdd(tot.CacheWriteTokens, c.CacheWrite)
	t.cacheWrite = saturatingAdd(t.cacheWrite, c.CacheWrite)
	tot.Requests++
	t.requests++
	if costUSD > 0 {
		tot.RecordedCostUSD += costUSD
		t.recordedCost += costUSD
		if tot.CostByVersion == nil {
			tot.CostByVersion = map[string]float64{}
		}
		tot.CostByVersion[version] += costUSD
	}
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
		TotalCacheReadTokens:  t.cacheRead,
		TotalCacheWriteTokens: t.cacheWrite,
		TotalRecordedCostUSD:  t.recordedCost,
		ByDeployment:          make([]Totals, 0, len(t.byDeploy)),
	}
	var totalCost float64
	for _, tot := range t.byDeploy {
		row := *tot
		if tot.CostByVersion != nil {
			cp := make(map[string]float64, len(tot.CostByVersion))
			for k, v := range tot.CostByVersion {
				cp[k] = v
			}
			row.CostByVersion = cp
		}
		// EstimatedCost = record-time cost (versioned, final) plus
		// the snapshot-time fallback for any tokens recorded
		// without record-time costing.
		row.EstimatedCost = row.RecordedCostUSD
		if p, ok := prices[row.Deployment]; ok {
			row.EstimatedCost += (float64(tot.unpricedPrompt)/1_000_000)*p.InputPerMTok +
				(float64(tot.unpricedCompletion)/1_000_000)*p.OutputPerMTok
		}
		if row.EstimatedCost < 0 {
			row.EstimatedCost = 0
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
	// Recompute globals from the surviving rows. Subtraction is incorrect after
	// a counter has saturated because the pre-saturation contribution is no
	// longer recoverable from the saturated aggregate.
	t.prompt, t.completion, t.requests = 0, 0, 0
	t.cacheRead, t.cacheWrite, t.recordedCost = 0, 0, 0
	for _, tot := range t.byDeploy {
		t.prompt = saturatingAdd(t.prompt, tot.PromptTokens)
		t.completion = saturatingAdd(t.completion, tot.CompletionTok)
		t.requests = saturatingAdd(t.requests, tot.Requests)
		t.cacheRead = saturatingAdd(t.cacheRead, tot.CacheReadTokens)
		t.cacheWrite = saturatingAdd(t.cacheWrite, tot.CacheWriteTokens)
		t.recordedCost += tot.RecordedCostUSD
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
	t.cacheRead = 0
	t.cacheWrite = 0
	t.recordedCost = 0
}
