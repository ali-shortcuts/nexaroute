package httpapi

import (
	"net/http"
)

// GET  /admin/api/usage        → snapshot
// POST /admin/api/usage/reset  → zero the in-memory counters
func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.usage.Snapshot())
	case http.MethodPost:
		if r.URL.Path != "/admin/api/usage/reset" {
			errorJSON(w, 404, "not found")
			return
		}
		s.usage.Reset()
		writeJSON(w, 200, map[string]any{"reset": true})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}
