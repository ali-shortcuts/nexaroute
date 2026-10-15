package httpapi

import (
	"net/http"
)

func (s *Server) recordUsage(r *http.Request, deployment string, input, output int64) {
	noteSpanUsage(r.Context(), deployment, input, output)
	s.usage.Record(deployment, input, output)
	// Reconcile the request's TPM reservation with the actual usage the
	// provider reported (M3-8). Requests that never reach a usage
	// report keep the admission estimate as their charge.
	if res := tpmReservationFromContext(r.Context()); res != nil {
		s.clientTPMSettle(res, input+output)
	}
	if s.identityUsage == nil {
		return
	}
	id, ok := clientIdentityFromRequest(r)
	if !ok {
		s.identityUsage.Record("anonymous", "", "", "", input, output)
		return
	}
	s.identityUsage.Record(id.ID, id.TenantID, id.ProjectID, id.TeamID, input, output)
}
