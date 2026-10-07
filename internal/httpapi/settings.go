package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"net/http"
	"strconv"
	"strings"
)

type settingsForm struct {
	Revision      uint64               `json:"revision,omitempty"`
	ClientBaseURL string               `json:"client_base_url,omitempty"`
	Routing       config.RoutingConfig `json:"routing"`
	Probe         config.ProbeConfig   `json:"probe"`
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routingCfg, probeCfg := s.runtimeSettingsSnapshot()
		cfg := s.currentConfig()
		rev := s.currentConfigRevision()
		w.Header().Set("ETag", strconv.FormatUint(rev, 10))
		writeJSON(w, 200, settingsForm{Revision: rev, ClientBaseURL: cfg.ClientBaseURL, Routing: routingCfg, Probe: probeCfg})
	case http.MethodPut:
		if raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), "\""); raw != "" {
			want, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || want != s.currentConfigRevision() {
				errorJSON(w, http.StatusPreconditionFailed, "config revision changed; re-read settings")
				return
			}
		}
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
		rev := s.currentConfigRevision()
		w.Header().Set("ETag", strconv.FormatUint(rev, 10))
		writeJSON(w, 200, map[string]any{"saved": true, "revision": rev, "client_base_url": cfg.ClientBaseURL, "routing": cfg.Routing, "probe": cfg.Probe})
	default:
		errorJSON(w, 405, "method not allowed")
	}
}
