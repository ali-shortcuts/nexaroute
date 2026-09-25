package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"net/http"
	"strings"
)

// adminEndpoint is protected by the same origin/local/admin-key middleware.
// The key is returned only by this explicit, non-cacheable admin endpoint.
func (s *Server) adminEndpoint(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var in struct {
			Model  string `json:"model"`
			Rotate bool   `json:"rotate_key"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON")
			return
		}
		model := strings.TrimSpace(in.Model)
		if model == "" {
			errorJSON(w, 400, "model name is required")
			return
		}
		var err error
		cfg, err = s.mutateConfig(func(c *config.Config) error {
			c.Routing.PublicModel = model
			if len(c.ClientAuth.Keys) == 0 || in.Rotate {
				b := make([]byte, 32)
				if _, e := rand.Read(b); e != nil {
					return e
				}
				key := "nx_" + hex.EncodeToString(b)
				if len(c.ClientAuth.Keys) == 0 {
					c.ClientAuth.Keys = []string{key}
				} else {
					c.ClientAuth.Keys[0] = key
				}
			}
			c.ClientAuth.Enabled = true
			return nil
		})
		if err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
	default:
		errorJSON(w, 405, "method not allowed")
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
	writeJSON(w, 200, map[string]any{"model": cfg.Routing.PublicModel, "api_key": key, "enabled": cfg.ClientAuth.Enabled, "deployments": count})
}
