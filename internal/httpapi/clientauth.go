package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// authorizedClientKey returns the matching enabled ClientKey for the presented
// credential, using constant-time comparisons against every enabled key.
func authorizedClientKey(cfg config.Config, presented string) (config.ClientKey, bool) {
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return config.ClientKey{}, false
	}
	for _, k := range cfg.ClientAuth.Keys {
		if !k.Enabled {
			continue
		}
		if len(presented) == len(k.Key) && subtle.ConstantTimeCompare([]byte(presented), []byte(k.Key)) == 1 {
			return k, true
		}
	}
	return config.ClientKey{}, false
}

func extractClientCredential(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (s *Server) clientAuthRequired() bool {
	cfg := s.currentConfig()
	return cfg.ClientAuth.Required
}

func (s *Server) authorizeClientKey(r *http.Request) (string, bool) {
	k, ok := authorizedClientKey(s.currentConfig(), extractClientCredential(r))
	if !ok {
		return "", false
	}
	name := k.Name
	if name == "" {
		name = k.ID
	}
	return name, true
}

// rejectUnauthorized answers with the error shape of the ingress the caller
// used, so Claude Code and OpenAI SDKs both render a native error.
func (s *Server) rejectUnauthorized(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/messages", "/v1/messages/count_tokens":
		anthropicErrorJSON(w, http.StatusUnauthorized, "missing or invalid client key")
	default:
		errorJSON(w, http.StatusUnauthorized, "missing or invalid client key")
	}
}

type clientKeyOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Key      string `json:"key"`
	Enabled  bool   `json:"enabled"`
	Requests int64  `json:"requests"`
}

func generateClientKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "nr-" + hex.EncodeToString(b), nil
}

func generateClientKeyID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "key-" + hex.EncodeToString(b), nil
}

// GET  /admin/api/client-keys            → list (+ required flag)
// POST /admin/api/client-keys            → generate {name}
// PUT  /admin/api/client-keys/{id}       → update {name?, enabled?}
// DELETE /admin/api/client-keys/{id}
func (s *Server) adminClientKeys(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/client-keys")
	rest = strings.Trim(rest, "/")

	switch {
	case r.Method == http.MethodGet && rest == "":
		cfg := s.currentConfig()
		snap := s.usage.Snapshot()
		out := make([]clientKeyOut, 0, len(cfg.ClientAuth.Keys))
		for _, k := range cfg.ClientAuth.Keys {
			name := k.Name
			if name == "" {
				name = k.ID
			}
			out = append(out, clientKeyOut{ID: k.ID, Name: name, Key: k.Key, Enabled: k.Enabled, Requests: snap.ByKey[name].Requests})
		}
		writeJSON(w, 200, map[string]any{"required": cfg.ClientAuth.Required, "keys": out})
		return

	case r.Method == http.MethodPost && rest == "":
		var in struct {
			Name string `json:"name"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		key, err := generateClientKey()
		if err != nil {
			errorJSON(w, 500, "key generation failed")
			return
		}
		id, err := generateClientKeyID()
		if err != nil {
			errorJSON(w, 500, "key generation failed")
			return
		}
		var saved config.ClientKey
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			if len(cfg.ClientAuth.Keys) >= 256 {
				return errors.New("client key limit (256) reached")
			}
			saved = config.ClientKey{ID: id, Name: strings.TrimSpace(in.Name), Key: key, Enabled: true}
			cfg.ClientAuth.Keys = append(cfg.ClientAuth.Keys, saved)
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		name := saved.Name
		if name == "" {
			name = saved.ID
		}
		writeJSON(w, 201, map[string]any{"created": true, "key": clientKeyOut{ID: saved.ID, Name: name, Key: saved.Key, Enabled: saved.Enabled}})
		return

	case r.Method == http.MethodPut && rest != "":
		var in struct {
			Name    *string `json:"name"`
			Enabled *bool   `json:"enabled"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		var updated config.ClientKey
		found := false
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			for i := range cfg.ClientAuth.Keys {
				if cfg.ClientAuth.Keys[i].ID != rest {
					continue
				}
				found = true
				if in.Name != nil {
					cfg.ClientAuth.Keys[i].Name = strings.TrimSpace(*in.Name)
				}
				if in.Enabled != nil {
					cfg.ClientAuth.Keys[i].Enabled = *in.Enabled
				}
				updated = cfg.ClientAuth.Keys[i]
				return nil
			}
			return errAdminProviderNotFound // reuse typed 404 mapping below
		}); err != nil {
			if errors.Is(err, errAdminProviderNotFound) {
				errorJSON(w, 404, "client key not found")
				return
			}
			errorJSON(w, 400, err.Error())
			return
		}
		if !found {
			errorJSON(w, 404, "client key not found")
			return
		}
		writeJSON(w, 200, map[string]any{"updated": true, "id": updated.ID, "name": updated.Name, "enabled": updated.Enabled})
		return

	case r.Method == http.MethodDelete && rest != "":
		removed := false
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			keys := cfg.ClientAuth.Keys[:0]
			for _, k := range cfg.ClientAuth.Keys {
				if k.ID == rest {
					removed = true
					continue
				}
				keys = append(keys, k)
			}
			cfg.ClientAuth.Keys = keys
			return nil
		}); err != nil {
			errorJSON(w, 400, err.Error())
			return
		}
		if !removed {
			errorJSON(w, 404, "client key not found")
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "id": rest})
		return

	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// PUT /admin/api/client-keys  (empty path) is not valid, but toggling the
// required flag has its own endpoint to keep validation errors legible.
func (s *Server) adminClientAuthRequired(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		errorJSON(w, 405, "method not allowed")
		return
	}
	var in struct {
		Required *bool `json:"required"`
	}
	if _, err := readJSON(r, &in); err != nil || in.Required == nil {
		errorJSON(w, 400, "body must be {\"required\": true|false}")
		return
	}
	var saved bool
	if _, err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.ClientAuth.Required = *in.Required
		saved = *in.Required
		return nil
	}); err != nil {
		errorJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"required": saved})
}
