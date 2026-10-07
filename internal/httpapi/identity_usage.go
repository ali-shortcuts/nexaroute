package httpapi

import (
	"net/http"
)

func (s *Server) recordUsage(r *http.Request, deployment string, input, output int64) {
	s.usage.Record(deployment, input, output)
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
