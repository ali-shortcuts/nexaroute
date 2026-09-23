package httpapi

import (
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type settingsForm struct {
	Routing    config.RoutingConfig          `json:"routing"`
	Probe      config.ProbeConfig            `json:"probe"`
	Logging    config.LoggingConfig          `json:"logging"`
	Admin      config.AdminConfig            `json:"admin"`
	Pricing    map[string]config.PriceConfig `json:"pricing"`
	Guardrails config.GuardrailsConfig       `json:"guardrails"`
	Listen     string                        `json:"listen"`
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routingCfg, probeCfg := s.runtimeSettingsSnapshot()
		cfg := s.currentConfig()
		writeJSON(w, 200, settingsForm{
			Routing:    routingCfg,
			Probe:      probeCfg,
			Logging:    cfg.Logging,
			Admin:      cfg.Admin,
			Pricing:    cfg.Pricing,
			Guardrails: cfg.Guardrails,
			Listen:     cfg.Listen,
		})
	case http.MethodPut:
		var in settingsForm
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if !in.Admin.BindLocalOnly && strings.TrimSpace(in.Admin.APIKey) == "" {
			errorJSON(w, 400, "admin.api_key is required when admin.bind_local_only is disabled")
			return
		}
		cfg, err := s.mutateConfig(func(cfg *config.Config) error {
			cfg.Routing = in.Routing
			cfg.Probe = in.Probe
			cfg.Logging = in.Logging
			cfg.Admin = in.Admin
			cfg.Pricing = in.Pricing
			cfg.Guardrails = in.Guardrails
			cfg.ApplyDefaults()
			return nil
		})
		if err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		// Never echo the stored admin key back in the response body.
		writeJSON(w, 200, map[string]any{
			"saved":   true,
			"routing": cfg.Routing,
			"probe":   cfg.Probe,
			"logging": cfg.Logging,
			"admin":   config.AdminConfig{BindLocalOnly: cfg.Admin.BindLocalOnly},
			"pricing": cfg.Pricing,
			"listen":  cfg.Listen,
		})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}
