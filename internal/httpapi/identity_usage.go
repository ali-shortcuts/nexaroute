package httpapi

import (
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// recordUsageCanonical funnels a canonical usage report into
// accounting with the dialect's cache convention: Anthropic's
// input count EXCLUDES cache tokens; OpenAI Chat/Responses and
// Gemini counts INCLUDE the cached share in input.
func (s *Server) recordUsageCanonical(r *http.Request, deployment, kind string, u canonical.Usage) {
	s.recordUsageEx(r, deployment, usage.Counts{
		Input:      int64(u.InputTokens),
		Output:     int64(u.OutputTokens),
		CacheRead:  int64(u.CacheReadTokens),
		CacheWrite: int64(u.CacheWriteTokens),
	}, kind != "anthropic" && kind != "bedrock")
}

func (s *Server) recordUsage(r *http.Request, deployment string, input, output int64) {
	s.recordUsageEx(r, deployment, usage.Counts{Input: input, Output: output}, false)
}

// recordUsageEx is the single accounting funnel (M5-6): the usage
// is priced AT RECORD TIME under the price-book version active
// now, accumulated with its cache-token counts, and any budget
// reservation on the request settles to this actual cost (first
// settle wins inside the ledger). The TPM reservation reconciles
// to input+output total tokens exactly as before.
func (s *Server) recordUsageEx(r *http.Request, deployment string, counts usage.Counts, inputIncludesCache bool) {
	noteSpanUsage(r.Context(), deployment, counts.Input, counts.Output)
	snap := s.configSnapshot()
	cost, version := s.recordCost(snap, deployment, counts.Input, counts.Output, counts.CacheRead, counts.CacheWrite, inputIncludesCache)
	s.usage.RecordEx(deployment, counts, cost, version)
	if res := tpmReservationFromContext(r.Context()); res != nil {
		s.clientTPMSettle(res, counts.Input+counts.Output)
	}
	if bres := budgetReservationFromContext(r.Context()); bres != nil && s.budgets != nil {
		s.budgets.Settle(bres.keyID, bres.id, cost)
		// Threshold webhook alerts (best-effort, asynchronous): never
		// affects accounting or the response on this path.
		s.checkBudgetAlerts(bres.keyID)
	}
	if s.identityUsage == nil {
		return
	}
	id, ok := clientIdentityFromRequest(r)
	if !ok {
		s.identityUsage.Record("anonymous", "", "", "", counts.Input, counts.Output)
		return
	}
	s.identityUsage.Record(id.ID, id.TenantID, id.ProjectID, id.TeamID, counts.Input, counts.Output)
}
