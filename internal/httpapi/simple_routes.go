package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// simpleRouteForm is the human-friendly control-plane contract. It compiles
// atomically into the existing Candidate Pool -> Route Profile -> Virtual
// Endpoint primitives so the backend remains the only routing authority.
type simpleRouteForm struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	PublicModel string   `json:"public_model"`
	Mode        string   `json:"mode"` // automatic | ordered
	Deployments []string `json:"deployments"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

func (s *Server) adminSimpleRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in simpleRouteForm
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := normalizeSimpleRoute(&in); err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.mutateConfig(func(cfg *config.Config) error {
		if findVirtualEndpoint(cfg, in.ID) >= 0 {
			return fmt.Errorf("simple route %q already exists", in.ID)
		}
		if hasSimpleRoutePrimitiveCollision(cfg, in.ID) {
			return fmt.Errorf("simple route %q conflicts with existing advanced routing objects", in.ID)
		}
		compileSimpleRoute(cfg, in)
		return nil
	}); err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "id": in.ID})
}

func (s *Server) adminSimpleRouteByID(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/admin/api/simple-routes/"))
	if err != nil || strings.TrimSpace(id) == "" {
		errorJSON(w, http.StatusBadRequest, "simple route id required")
		return
	}
	id = strings.TrimSpace(id)

	switch r.Method {
	case http.MethodPut:
		var in simpleRouteForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.ID != "" && strings.TrimSpace(in.ID) != id {
			errorJSON(w, http.StatusBadRequest, "id in body must match URL or be empty")
			return
		}
		in.ID = id
		if err := normalizeSimpleRoute(&in); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			idx := findVirtualEndpoint(cfg, id)
			if idx < 0 {
				return fmt.Errorf("simple route %q not found", id)
			}
			if cfg.VirtualEndpoints[idx].RouteProfile != simpleProfileID(id) {
				return fmt.Errorf("virtual endpoint %q is advanced-managed and cannot be edited as a simple route", id)
			}
			removeSimpleRoutePrimitives(cfg, id, false)
			compileSimpleRoute(cfg, in)
			return nil
		}); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true, "id": id})

	case http.MethodDelete:
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			idx := findVirtualEndpoint(cfg, id)
			if idx < 0 {
				return fmt.Errorf("simple route %q not found", id)
			}
			if cfg.VirtualEndpoints[idx].RouteProfile != simpleProfileID(id) {
				return fmt.Errorf("virtual endpoint %q is advanced-managed and cannot be deleted as a simple route", id)
			}
			removeSimpleRoutePrimitives(cfg, id, true)
			return nil
		}); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})

	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func normalizeSimpleRoute(in *simpleRouteForm) error {
	in.ID = strings.TrimSpace(in.ID)
	in.Name = strings.TrimSpace(in.Name)
	in.PublicModel = strings.TrimSpace(in.PublicModel)
	in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	if in.Mode == "" {
		in.Mode = "automatic"
	}
	if in.ID == "" {
		return fmt.Errorf("id is required")
	}
	if in.PublicModel == "" {
		return fmt.Errorf("public_model is required")
	}
	if in.Name == "" {
		in.Name = in.PublicModel
	}
	if in.Mode != "automatic" && in.Mode != "ordered" {
		return fmt.Errorf("mode must be automatic or ordered")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in.Deployments))
	for _, d := range in.Deployments {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	in.Deployments = out
	if len(in.Deployments) == 0 {
		return fmt.Errorf("at least one deployment is required")
	}
	if in.Enabled == nil {
		t := true
		in.Enabled = &t
	}
	return nil
}

func simpleProfileID(id string) string   { return id + "-profile" }
func simplePoolID(id string) string      { return id + "-pool" }
func simpleFallbackID(id string) string  { return id + "-fallback" }
func simpleStagePrefix(id string) string { return id + "-stage-" }

func findVirtualEndpoint(cfg *config.Config, id string) int {
	for i := range cfg.VirtualEndpoints {
		if cfg.VirtualEndpoints[i].ID == id {
			return i
		}
	}
	return -1
}

func hasSimpleRoutePrimitiveCollision(cfg *config.Config, id string) bool {
	profileID, poolID, fallbackID, stagePrefix := simpleProfileID(id), simplePoolID(id), simpleFallbackID(id), simpleStagePrefix(id)
	for _, x := range cfg.RouteProfiles {
		if x.ID == profileID {
			return true
		}
	}
	for _, x := range cfg.CandidatePools {
		if x.ID == poolID || strings.HasPrefix(x.ID, stagePrefix) {
			return true
		}
	}
	for _, x := range cfg.FallbackChains {
		if x.ID == fallbackID {
			return true
		}
	}
	return false
}

func compileSimpleRoute(cfg *config.Config, in simpleRouteForm) {
	profileID := simpleProfileID(in.ID)
	poolID := simplePoolID(in.ID)
	fallbackID := ""

	if in.Mode == "ordered" {
		stageIDs := make([]string, 0, len(in.Deployments))
		for i, dep := range in.Deployments {
			stageID := fmt.Sprintf("%s%d", simpleStagePrefix(in.ID), i+1)
			cfg.CandidatePools = append(cfg.CandidatePools, config.CandidatePoolConfig{
				ID: stageID, Name: fmt.Sprintf("%s — stage %d", in.Name, i+1), Mode: "explicit", Deployments: []string{dep},
			})
			stageIDs = append(stageIDs, stageID)
		}
		poolID = stageIDs[0]
		fallbackID = simpleFallbackID(in.ID)
		cfg.FallbackChains = append(cfg.FallbackChains, config.FallbackChainConfig{
			ID: fallbackID, Name: in.Name + " fallback", Pools: stageIDs,
		})
	} else {
		cfg.CandidatePools = append(cfg.CandidatePools, config.CandidatePoolConfig{
			ID: poolID, Name: in.Name + " models", Mode: "explicit", Deployments: append([]string(nil), in.Deployments...),
		})
	}

	cfg.RouteProfiles = append(cfg.RouteProfiles, config.RouteProfileConfig{
		ID: profileID, Name: in.Name + " route", CandidatePool: poolID, FallbackChain: fallbackID,
	})
	cfg.VirtualEndpoints = append(cfg.VirtualEndpoints, config.VirtualEndpointConfig{
		ID: in.ID, Name: in.Name, Enabled: in.Enabled, PublicModel: in.PublicModel, RouteProfile: profileID,
	})
}

func removeSimpleRoutePrimitives(cfg *config.Config, id string, removeEndpoint bool) {
	profileID, poolID, fallbackID, stagePrefix := simpleProfileID(id), simplePoolID(id), simpleFallbackID(id), simpleStagePrefix(id)

	if removeEndpoint {
		out := cfg.VirtualEndpoints[:0]
		for _, x := range cfg.VirtualEndpoints {
			if x.ID != id {
				out = append(out, x)
			}
		}
		cfg.VirtualEndpoints = out
	} else {
		// Updating: remove the old endpoint too; compileSimpleRoute re-adds it
		// with the complete normalized body in the same atomic mutation.
		out := cfg.VirtualEndpoints[:0]
		for _, x := range cfg.VirtualEndpoints {
			if x.ID != id {
				out = append(out, x)
			}
		}
		cfg.VirtualEndpoints = out
	}

	rp := cfg.RouteProfiles[:0]
	for _, x := range cfg.RouteProfiles {
		if x.ID != profileID {
			rp = append(rp, x)
		}
	}
	cfg.RouteProfiles = rp

	cp := cfg.CandidatePools[:0]
	for _, x := range cfg.CandidatePools {
		if x.ID != poolID && !strings.HasPrefix(x.ID, stagePrefix) {
			cp = append(cp, x)
		}
	}
	cfg.CandidatePools = cp

	fc := cfg.FallbackChains[:0]
	for _, x := range cfg.FallbackChains {
		if x.ID != fallbackID {
			fc = append(fc, x)
		}
	}
	cfg.FallbackChains = fc
}
