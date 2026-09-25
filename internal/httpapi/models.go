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
	if !s.clientAuthAllowed(w, r, false) {
		return
	}
	s.runtimeMu.RLock()
	deployments := s.rt.All()
	resolver := s.routeResolver
	cfg := s.cfg
	s.runtimeMu.RUnlock()
	created := time.Now().Unix()
	data := make([]map[string]any, 0, 2+len(deployments)*2+len(cfg.VirtualEndpoints))
	seen := make(map[string]bool, 2+len(deployments)*2+len(cfg.VirtualEndpoints))
	// Virtual endpoints first (stable client-facing identities)
	if resolver != nil {
		for _, ve := range resolver.ListVirtualEndpoints() {
			if !ve.IsEnabled() {
				continue
			}
			if ve.PublicModel == "" || seen[ve.PublicModel] {
				continue
			}
			seen[ve.PublicModel] = true
			data = append(data, map[string]any{"id": ve.PublicModel, "object": "model", "created": created, "owned_by": "gateway", "virtual_endpoint": ve.ID})
		}
	} else if cfg.Routing.PublicModel != "" {
		// Legacy single endpoint compatibility
		id := cfg.Routing.PublicModel
		if !seen[id] {
			seen[id] = true
			data = append(data, map[string]any{"id": id, "object": "model", "created": created, "owned_by": "gateway"})
		}
	}
	if len(deployments) > 0 || (resolver != nil && len(resolver.ListVirtualEndpoints()) > 0) {
		for _, id := range []string{"auto", "claude-auto"} {
			if seen[id] {
				continue
			}
			seen[id] = true
			data = append(data, map[string]any{"id": id, "object": "model", "created": created, "owned_by": "gateway"})
		}
	}
	for _, d := range deployments {
		ids := append([]string{d.ID, d.Model}, d.Aliases...)
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			data = append(data, map[string]any{"id": id, "object": "model", "created": created, "owned_by": d.ProviderID})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}
