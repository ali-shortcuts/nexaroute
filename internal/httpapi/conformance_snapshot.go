package httpapi

import (
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// Conformance surfacing (#436 slice 2): the admin snapshot
// carries the conformance manifest so the provider UI displays
// capability status from the SAME evidence source as
// docs/COMPATIBILITY.md — never from a model list or a preset
// name. Effective states apply the manifest's staleness policy:
// verified evidence past its freshness window reads STALE.

func conformanceSnapshot(now time.Time) map[string]any {
	entries := providers.Manifest()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		effective := providers.EffectiveState(e, now, providers.DefaultStaleAfterDays)
		dims := make([]map[string]any, 0, len(e.Dimensions))
		for _, d := range providers.ConformanceDimensions() {
			claim, ok := e.Dimensions[d]
			if !ok {
				continue
			}
			dimEffective := claim.State
			if effective == providers.StateStale && freshnessBearing(claim.State) {
				dimEffective = providers.StateStale
			}
			dims = append(dims, map[string]any{
				"dimension":       string(d),
				"state":           string(claim.State),
				"effective_state": string(dimEffective),
				"note":            claim.Note,
			})
		}
		out = append(out, map[string]any{
			"dialect":            e.Dialect,
			"contract_version":   e.ContractVersion,
			"native_passthrough": e.Native,
			"status":             string(e.Status),
			"effective_status":   string(effective),
			"evidence_source":    e.EvidenceSource,
			"evidence_date":      e.EvidenceDate,
			"limitations":        e.Limitations,
			"dimensions":         dims,
		})
	}
	return map[string]any{
		"version":          providers.ManifestVersion,
		"stale_after_days": providers.DefaultStaleAfterDays,
		"entries":          out,
	}
}

// freshnessBearing mirrors EffectiveState's policy: only claims
// that assert verified freshness can go stale; UNSUPPORTED and
// UNTESTED assert absence and do not age.
func freshnessBearing(s providers.ConformanceState) bool {
	return s == providers.StateFixtureVerified || s == providers.StateLiveVerified || s == providers.StatePartial
}
