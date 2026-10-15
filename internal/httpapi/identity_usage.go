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
	noteSpanUsage(r.Context(), deployment, input, output)
	s.usage.Record(deployment, input, output)
	// Reconcile the request's TPM reservation with the actual usage the
	// provider reported (M3-8). Requests that never reach a usage
	// report keep the admission estimate as their charge.

	if res := tpmReservationFromContext(r.Context()); res != nil {
		s.clientTPMSettle(res, counts.Input+counts.Output)
	}
	if bres := budgetReservationFromContext(r.Context()); bres != nil && s.budgets != nil {
		s.budgets.Settle(bres.keyID, bres.id, cost)
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
