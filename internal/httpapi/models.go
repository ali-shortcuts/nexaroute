package httpapi

import (
	"net/http"
	"time"
)

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	s.runtimeMu.RLock()
	deployments := s.rt.All()
	s.runtimeMu.RUnlock()
	data := []map[string]any{}
	seen := map[string]bool{}
	if len(deployments) > 0 {
		for _, id := range []string{"auto", "claude-auto"} {
			seen[id] = true
			data = append(data, map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "gateway"})
		}
	}
	for _, d := range deployments {
		ids := append([]string{d.ID, d.Model}, d.Aliases...)
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			data = append(data, map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": d.ProviderID})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}
