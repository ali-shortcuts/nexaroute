package httpapi

import (
	"github.com/ali-shortcuts/universal-llm-gateway/internal/config"
	"net/http"
)

type settingsForm struct {
	Routing config.RoutingConfig `json:"routing"`
	Probe   config.ProbeConfig   `json:"probe"`
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, settingsForm{Routing: cfg.Routing, Probe: cfg.Probe})
	case http.MethodPut:
		var in settingsForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		cfg.Routing = in.Routing
		cfg.Probe = in.Probe
		cfg.ApplyDefaults()
		if err := s.applyConfig(cfg); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "routing": cfg.Routing, "probe": cfg.Probe})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}
