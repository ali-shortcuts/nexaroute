package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"net/http"
)

type settingsForm struct {
	Routing config.RoutingConfig `json:"routing"`
	Probe   config.ProbeConfig   `json:"probe"`
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		writeJSON(w, 200, settingsForm{Routing: cfg.Routing, Probe: cfg.Probe})
	case http.MethodPut:
		var in settingsForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		cfg, err := s.mutateConfig(func(cfg *config.Config) error {
			cfg.Routing = in.Routing
			cfg.Probe = in.Probe
			cfg.ApplyDefaults()
			return nil
		})
		if err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "routing": cfg.Routing, "probe": cfg.Probe})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}
