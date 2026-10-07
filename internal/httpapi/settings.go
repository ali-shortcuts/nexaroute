package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"net/http"
)

type settingsForm struct {
	ClientBaseURL string               `json:"client_base_url,omitempty"`
	Routing       config.RoutingConfig `json:"routing"`
	Probe         config.ProbeConfig   `json:"probe"`
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routingCfg, probeCfg := s.runtimeSettingsSnapshot()
		cfg := s.currentConfig()
		writeJSON(w, 200, settingsForm{ClientBaseURL: cfg.ClientBaseURL, Routing: routingCfg, Probe: probeCfg})
	case http.MethodPut:
		var in settingsForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		cfg, err := s.mutateConfig(func(cfg *config.Config) error {
			cfg.ClientBaseURL = in.ClientBaseURL
			cfg.Routing = in.Routing
			cfg.Probe = in.Probe
			cfg.ApplyDefaults()
			return nil
		})
		if err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": true, "client_base_url": cfg.ClientBaseURL, "routing": cfg.Routing, "probe": cfg.Probe})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}
