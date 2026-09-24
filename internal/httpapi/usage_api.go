package httpapi

import (
	"net/http"
	"strconv"
)

// GET /admin/api/requests?limit=100 → recent completed data-plane requests
// (newest first) with their resolved route, latency, tokens and cost.
func (s *Server) adminRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 500 {
				limit = 500
			}
		}
	}
	writeJSON(w, 200, map[string]any{"requests": s.usage.RecentRequests(limit)})
}

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
