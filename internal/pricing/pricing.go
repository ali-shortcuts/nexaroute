// Package pricing implements NexaRoute's versioned price book: a
// list of price versions, each effective from a timestamp, mapping
// deployments ("provider/model") to per-million-token rates.
// Costs are computed when usage is RECORDED, under the version
// active at that moment, so a price change never reprices history.
package pricing

import (
	"sort"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Entry is one deployment's rates (USD per million tokens).
// Cache rates are pointers: nil means "not priced separately" and
// the input rate applies (the documented default rule).
type Entry struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  *float64
	CacheWritePerMTok *float64
}

// CacheRead returns the effective cache-read rate.
func (e Entry) CacheRead() float64 {
	if e.CacheReadPerMTok != nil {
		return *e.CacheReadPerMTok
	}
	return e.InputPerMTok
}

// CacheWrite returns the effective cache-write rate.
func (e Entry) CacheWrite() float64 {
	if e.CacheWritePerMTok != nil {
		return *e.CacheWritePerMTok
	}
	return e.InputPerMTok
}

// Version is one effective-dated set of rates.
type Version struct {
	Name          string
	EffectiveFrom time.Time
	Prices        map[string]Entry
}

// Book is the resolved, time-ordered price book.
type Book struct {
	versions []Version // ascending by EffectiveFrom
}

// FromConfig builds the book from config. Invalid entries were
// already rejected by config validation; unparseable timestamps
// are skipped defensively (never serve a wrong price silently —
// config validation is the gate).
func FromConfig(cfg config.Config) *Book {
	versions := make([]Version, 0, len(cfg.Pricing.Versions))
	for _, v := range cfg.Pricing.Versions {
		from, err := time.Parse(time.RFC3339, v.EffectiveFrom)
		if err != nil {
			continue
		}
		prices := make(map[string]Entry, len(v.Prices))
		for dep, p := range v.Prices {
			prices[dep] = Entry{
				InputPerMTok:      p.InputPerMTok,
				OutputPerMTok:     p.OutputPerMTok,
				CacheReadPerMTok:  p.CacheReadPerMTok,
				CacheWritePerMTok: p.CacheWritePerMTok,
			}
		}
		versions = append(versions, Version{Name: v.Version, EffectiveFrom: from, Prices: prices})
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].EffectiveFrom.Before(versions[j].EffectiveFrom)
	})
	return &Book{versions: versions}
}

// Resolve returns the entry for a deployment under the version
// active at `at` (the latest version whose effective_from is not
// after `at` AND that prices this deployment). A later version
// that does not mention the deployment does not shadow an earlier
// version's entry for it.
func (b *Book) Resolve(deploymentID string, at time.Time) (Entry, string, bool) {
	if b == nil {
		return Entry{}, "", false
	}
	for i := len(b.versions) - 1; i >= 0; i-- {
		v := b.versions[i]
		if v.EffectiveFrom.After(at) {
			continue
		}
		if e, ok := v.Prices[deploymentID]; ok {
			return e, v.Name, true
		}
	}
	return Entry{}, "", false
}

// ModelEntry is the fallback entry from a model's config prices,
// labeled version "config" by callers. Config model prices carry
// no cache rates, so the input-rate default applies.
func ModelEntry(inputPerMTok, outputPerMTok float64) Entry {
	return Entry{InputPerMTok: inputPerMTok, OutputPerMTok: outputPerMTok}
}

// ResolveFor is the accounting resolution chain: price book first,
// then the deployment's model-config prices (version "config"),
// then a zero entry (version "unpriced": tokens still counted,
// cost zero — never invent a price).
func (b *Book) ResolveFor(cfg config.Config, deploymentID string, at time.Time) (Entry, string) {
	if e, v, ok := b.Resolve(deploymentID, at); ok {
		return e, v
	}
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			if p.ID+"/"+m.ID == deploymentID {
				return ModelEntry(m.InputCostPerMTok, m.OutputCostPerMTok), "config"
			}
		}
	}
	return Entry{}, "unpriced"
}

// Cost prices one usage report. inputIncludesCache states the
// dialect's convention: when true (OpenAI Chat/Responses, Gemini),
// the input count already contains the cached-read tokens, so the
// cached share is moved from the input rate to the cache-read rate
// (never double-charged); when false (Anthropic), input excludes
// cache tokens and both are priced additively. Cache-write tokens
// are always additive.
func Cost(e Entry, input, output, cacheRead, cacheWrite int64, inputIncludesCache bool) float64 {
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	billableInput := input
	if inputIncludesCache {
		billableInput = input - cacheRead
		if billableInput < 0 {
			billableInput = 0
		}
	}
	cost := (float64(billableInput)*e.InputPerMTok +
		float64(output)*e.OutputPerMTok +
		float64(cacheRead)*e.CacheRead() +
		float64(cacheWrite)*e.CacheWrite()) / 1_000_000
	if cost < 0 {
		return 0
	}
	return cost
}

// Estimate is the conservative admission estimate: the full input
// estimate plus the full output allowance at headline rates (cache
// discounts unknown before the request runs).
func Estimate(e Entry, estInputTokens, maxOutputTokens int64) float64 {
	if estInputTokens < 0 {
		estInputTokens = 0
	}
	if maxOutputTokens < 0 {
		maxOutputTokens = 0
	}
	return (float64(estInputTokens)*e.InputPerMTok + float64(maxOutputTokens)*e.OutputPerMTok) / 1_000_000
}
