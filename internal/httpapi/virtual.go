package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// generateGatewayKey creates a gateway client key like PR #13: nx_ + 32 random bytes hex (67 chars).
func generateGatewayKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "nx_" + hex.EncodeToString(b), nil
}

// --- Virtual Endpoints ---

func (s *Server) adminVirtualEndpoints(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.runtimeMu.RLock()
		cfg := s.cfg
		resolver := s.routeResolver
		s.runtimeMu.RUnlock()
		// Include eligibility counts
		deployments := []map[string]any{}
		if resolver != nil {
			for _, ve := range resolver.ListVirtualEndpoints() {
				// Count eligible deployments for this VE
				// We need to compute eligible count by filtering router candidates for empty requirement? Use resolver expanded set.
				allowed, _ := resolver.GetExpanded(func() string {
					// Need profile to get pool
					// Find profile
					for _, rp := range cfg.RouteProfiles {
						if rp.ID == ve.RouteProfile {
							return rp.CandidatePool
						}
					}
					return ""
				}())
				eligible := len(allowed)
				deployments = append(deployments, map[string]any{
					"id": ve.ID, "name": ve.Name, "enabled": ve.IsEnabled(), "public_model": ve.PublicModel,
					"route_profile": ve.RouteProfile, "protocols": ve.Protocols,
					"eligible_deployments": eligible,
				})
			}
		} else {
			for _, ve := range cfg.VirtualEndpoints {
				deployments = append(deployments, map[string]any{
					"id": ve.ID, "name": ve.Name, "enabled": ve.IsEnabled(), "public_model": ve.PublicModel,
					"route_profile": ve.RouteProfile, "protocols": ve.Protocols,
				})
			}
		}
		writeJSON(w, 200, map[string]any{"virtual_endpoints": deployments})
	case http.MethodPost:
		var in config.VirtualEndpointConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		// Trim already done in ApplyDefaults but validate early
		if strings.TrimSpace(in.ID) == "" {
			errorJSON(w, 400, "id is required")
			return
		}
		if strings.TrimSpace(in.PublicModel) == "" {
			errorJSON(w, 400, "public_model is required")
			return
		}
		if strings.TrimSpace(in.RouteProfile) == "" {
			errorJSON(w, 400, "route_profile is required")
			return
		}
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			// Check duplicate ID
			for _, existing := range cfg.VirtualEndpoints {
				if existing.ID == strings.TrimSpace(in.ID) {
					return fmt.Errorf("duplicate virtual endpoint id %q", in.ID)
				}
			}
			cfg.VirtualEndpoints = append(cfg.VirtualEndpoints, in)
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"saved": true, "id": in.ID})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

func (s *Server) adminVirtualEndpointByID(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/admin/api/virtual-endpoints/"))
	if err != nil || strings.TrimSpace(id) == "" {
		errorJSON(w, 400, "virtual endpoint id required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.runtimeMu.RLock()
		cfg := s.cfg
		s.runtimeMu.RUnlock()
		for _, ve := range cfg.VirtualEndpoints {
			if ve.ID == id {
				writeJSON(w, 200, ve)
				return
			}
		}
		errorJSON(w, 404, "virtual endpoint not found")
	case http.MethodPut:
		var in config.VirtualEndpointConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if in.ID != "" && in.ID != id {
			errorJSON(w, 400, "id in body must match URL or be empty")
			return
		}
		in.ID = id
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			found := false
			for i, existing := range cfg.VirtualEndpoints {
				if existing.ID == id {
					cfg.VirtualEndpoints[i] = in
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("virtual endpoint %q not found", id)
			}
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "id": id})
	case http.MethodDelete:
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			newList := make([]config.VirtualEndpointConfig, 0, len(cfg.VirtualEndpoints))
			found := false
			for _, ve := range cfg.VirtualEndpoints {
				if ve.ID == id {
					found = true
					continue
				}
				newList = append(newList, ve)
			}
			if !found {
				return fmt.Errorf("virtual endpoint %q not found", id)
			}
			cfg.VirtualEndpoints = newList
			return nil
		}); err != nil {
			errorJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// --- Route Profiles ---

func (s *Server) adminRouteProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		writeJSON(w, 200, map[string]any{"route_profiles": cfg.RouteProfiles})
	case http.MethodPost:
		var in config.RouteProfileConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(in.ID) == "" {
			errorJSON(w, 400, "id is required")
			return
		}
		if strings.TrimSpace(in.CandidatePool) == "" {
			errorJSON(w, 400, "candidate_pool is required")
			return
		}
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			for _, existing := range cfg.RouteProfiles {
				if existing.ID == strings.TrimSpace(in.ID) {
					return fmt.Errorf("duplicate route profile id %q", in.ID)
				}
			}
			cfg.RouteProfiles = append(cfg.RouteProfiles, in)
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"saved": true, "id": in.ID})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

func (s *Server) adminRouteProfileByID(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/admin/api/route-profiles/"))
	if err != nil || strings.TrimSpace(id) == "" {
		errorJSON(w, 400, "route profile id required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		for _, rp := range cfg.RouteProfiles {
			if rp.ID == id {
				writeJSON(w, 200, rp)
				return
			}
		}
		errorJSON(w, 404, "route profile not found")
	case http.MethodPut:
		var in config.RouteProfileConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if in.ID != "" && in.ID != id {
			errorJSON(w, 400, "id in body must match URL or be empty")
			return
		}
		in.ID = id
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			found := false
			for i, existing := range cfg.RouteProfiles {
				if existing.ID == id {
					cfg.RouteProfiles[i] = in
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("route profile %q not found", id)
			}
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "id": id})
	case http.MethodDelete:
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			newList := make([]config.RouteProfileConfig, 0, len(cfg.RouteProfiles))
			found := false
			for _, rp := range cfg.RouteProfiles {
				if rp.ID == id {
					found = true
					continue
				}
				newList = append(newList, rp)
			}
			if !found {
				return fmt.Errorf("route profile %q not found", id)
			}
			cfg.RouteProfiles = newList
			return nil
		}); err != nil {
			errorJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// --- Candidate Pools ---

func (s *Server) adminCandidatePools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		// Include expanded counts if resolver available
		s.runtimeMu.RLock()
		resolver := s.routeResolver
		s.runtimeMu.RUnlock()
		out := []map[string]any{}
		for _, cp := range cfg.CandidatePools {
			item := map[string]any{"id": cp.ID, "name": cp.Name, "mode": cp.Mode, "deployments": cp.Deployments}
			if resolver != nil {
				if set, ok := resolver.GetExpanded(cp.ID); ok {
					item["expanded_count"] = len(set)
					// include expanded list for UI debugging
					ids := make([]string, 0, len(set))
					for k := range set {
						ids = append(ids, k)
					}
					item["expanded"] = ids
				}
			}
			out = append(out, item)
		}
		writeJSON(w, 200, map[string]any{"candidate_pools": out})
	case http.MethodPost:
		var in config.CandidatePoolConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(in.ID) == "" {
			errorJSON(w, 400, "id is required")
			return
		}
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			for _, existing := range cfg.CandidatePools {
				if existing.ID == strings.TrimSpace(in.ID) {
					return fmt.Errorf("duplicate candidate pool id %q", in.ID)
				}
			}
			cfg.CandidatePools = append(cfg.CandidatePools, in)
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"saved": true, "id": in.ID})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

func (s *Server) adminCandidatePoolByID(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/admin/api/candidate-pools/"))
	if err != nil || strings.TrimSpace(id) == "" {
		errorJSON(w, 400, "candidate pool id required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		for _, cp := range cfg.CandidatePools {
			if cp.ID == id {
				s.runtimeMu.RLock()
				resolver := s.routeResolver
				s.runtimeMu.RUnlock()
				out := map[string]any{"id": cp.ID, "name": cp.Name, "mode": cp.Mode, "deployments": cp.Deployments}
				if resolver != nil {
					if set, ok := resolver.GetExpanded(cp.ID); ok {
						out["expanded_count"] = len(set)
						ids := make([]string, 0, len(set))
						for k := range set {
							ids = append(ids, k)
						}
						out["expanded"] = ids
					}
				}
				writeJSON(w, 200, out)
				return
			}
		}
		errorJSON(w, 404, "candidate pool not found")
	case http.MethodPut:
		var in config.CandidatePoolConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if in.ID != "" && in.ID != id {
			errorJSON(w, 400, "id in body must match URL or be empty")
			return
		}
		in.ID = id
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			found := false
			for i, existing := range cfg.CandidatePools {
				if existing.ID == id {
					cfg.CandidatePools[i] = in
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("candidate pool %q not found", id)
			}
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "id": id})
	case http.MethodDelete:
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			newList := make([]config.CandidatePoolConfig, 0, len(cfg.CandidatePools))
			found := false
			for _, cp := range cfg.CandidatePools {
				if cp.ID == id {
					found = true
					continue
				}
				newList = append(newList, cp)
			}
			if !found {
				return fmt.Errorf("candidate pool %q not found", id)
			}
			cfg.CandidatePools = newList
			return nil
		}); err != nil {
			errorJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// --- Fallback Chains ---

func (s *Server) adminFallbackChains(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		writeJSON(w, 200, map[string]any{"fallback_chains": cfg.FallbackChains})
	case http.MethodPost:
		var in config.FallbackChainConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(in.ID) == "" {
			errorJSON(w, 400, "id is required")
			return
		}
		if len(in.Pools) == 0 {
			errorJSON(w, 400, "pools is required")
			return
		}
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			for _, existing := range cfg.FallbackChains {
				if existing.ID == strings.TrimSpace(in.ID) {
					return fmt.Errorf("duplicate fallback chain id %q", in.ID)
				}
			}
			cfg.FallbackChains = append(cfg.FallbackChains, in)
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"saved": true, "id": in.ID})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

func (s *Server) adminFallbackChainByID(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/admin/api/fallback-chains/"))
	if err != nil || strings.TrimSpace(id) == "" {
		errorJSON(w, 400, "fallback chain id required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		for _, fc := range cfg.FallbackChains {
			if fc.ID == id {
				writeJSON(w, 200, fc)
				return
			}
		}
		errorJSON(w, 404, "fallback chain not found")
	case http.MethodPut:
		var in config.FallbackChainConfig
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if in.ID != "" && in.ID != id {
			errorJSON(w, 400, "id in body must match URL or be empty")
			return
		}
		in.ID = id
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			found := false
			for i, existing := range cfg.FallbackChains {
				if existing.ID == id {
					cfg.FallbackChains[i] = in
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("fallback chain %q not found", id)
			}
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "id": id})
	case http.MethodDelete:
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			newList := make([]config.FallbackChainConfig, 0, len(cfg.FallbackChains))
			found := false
			for _, fc := range cfg.FallbackChains {
				if fc.ID == id {
					found = true
					continue
				}
				newList = append(newList, fc)
			}
			if !found {
				return fmt.Errorf("fallback chain %q not found", id)
			}
			cfg.FallbackChains = newList
			return nil
		}); err != nil {
			errorJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// --- Legacy endpoint (PR #13 compatibility) ---

func (s *Server) adminEndpoint(w http.ResponseWriter, r *http.Request) {
	// This endpoint is protected by admin middleware (loopback + key)
	// Returns no-store to avoid caching secrets.
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		key := ""
		if cfg.ClientAuth.Enabled && len(cfg.ClientAuth.Keys) > 0 {
			key = cfg.ClientAuth.Keys[0]
		}
		publicModel := cfg.Routing.PublicModel
		// If virtual endpoints exist, report the default one
		if len(cfg.VirtualEndpoints) > 0 {
			// Prefer default ID or first
			for _, ve := range cfg.VirtualEndpoints {
				if ve.ID == "default" {
					publicModel = ve.PublicModel
					break
				}
			}
			if publicModel == "" {
				publicModel = cfg.VirtualEndpoints[0].PublicModel
			}
		}
		count := 0
		for _, p := range cfg.Providers {
			if p.Enabled {
				for _, m := range p.Models {
					if m.Enabled {
						count++
					}
				}
			}
		}
		writeJSON(w, 200, map[string]any{"model": publicModel, "api_key": key, "enabled": cfg.ClientAuth.Enabled, "deployments": count})
	case http.MethodPost:
		var in struct {
			Model     string `json:"model"`
			Rotate    bool   `json:"rotate_key"`
			RotateAlt bool   `json:"rotate"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON")
			return
		}
		model := strings.TrimSpace(in.Model)
		if model == "" {
			// Allow empty model if only rotating key? For backward compat, require model.
			// But if rotating and model empty, keep existing.
			cfg := s.currentConfig()
			if len(cfg.VirtualEndpoints) > 0 {
				model = cfg.VirtualEndpoints[0].PublicModel
			} else {
				model = cfg.Routing.PublicModel
				if model == "" {
					model = "nexaroute"
				}
			}
		}
		rotate := in.Rotate || in.RotateAlt
		// Validate model name
		if len(model) > 128 || strings.ContainsAny(model, " \t\r\n\"'`$\\") {
			errorJSON(w, 400, "model name must be simple and <=128 bytes")
			return
		}
		if model == "auto" || model == "claude-auto" {
			errorJSON(w, 400, "model must not be auto or claude-auto")
			return
		}
		var newKey string
		if rotate {
			k, err := generateGatewayKey()
			if err != nil {
				errorJSON(w, 500, "failed to generate key")
				return
			}
			newKey = k
		}
		cfg, err := s.mutateConfig(func(c *config.Config) error {
			// Update legacy field
			c.Routing.PublicModel = model
			// If virtual endpoints exist, update default one
			if len(c.VirtualEndpoints) > 0 {
				updated := false
				for i, ve := range c.VirtualEndpoints {
					if ve.ID == "default" || (!updated && i == 0) {
						c.VirtualEndpoints[i].PublicModel = model
						updated = true
						if ve.ID == "default" {
							break
						}
					}
				}
			}
			// Handle key rotation / creation
			if len(c.ClientAuth.Keys) == 0 || rotate {
				if newKey == "" {
					k, err := generateGatewayKey()
					if err != nil {
						return err
					}
					newKey = k
				}
				if len(c.ClientAuth.Keys) == 0 {
					c.ClientAuth.Keys = []string{newKey}
				} else {
					c.ClientAuth.Keys[0] = newKey
				}
			}
			c.ClientAuth.Enabled = true
			return nil
		})
		if err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		key := ""
		if cfg.ClientAuth.Enabled && len(cfg.ClientAuth.Keys) > 0 {
			key = cfg.ClientAuth.Keys[0]
		}
		count := 0
		for _, p := range cfg.Providers {
			if p.Enabled {
				for _, m := range p.Models {
					if m.Enabled {
						count++
					}
				}
			}
		}
		writeJSON(w, 200, map[string]any{"model": model, "api_key": key, "enabled": cfg.ClientAuth.Enabled, "deployments": count})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// Ensure json import used
var _ = json.Marshal
