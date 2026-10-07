package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type clientKeyCreateRequest struct {
	Name          string   `json:"name"`
	TenantID      string   `json:"tenant_id"`
	ProjectID     string   `json:"project_id"`
	TeamID        string   `json:"team_id"`
	Role          string   `json:"role"`
	AllowedModels []string `json:"allowed_models"`
	AllowedRoutes []string `json:"allowed_routes"`
	ExpiresAt     string   `json:"expires_at"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
}

type clientKeyResponse struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	TenantID      string   `json:"tenant_id,omitempty"`
	ProjectID     string   `json:"project_id,omitempty"`
	TeamID        string   `json:"team_id,omitempty"`
	Role          string   `json:"role,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	AllowedRoutes []string `json:"allowed_routes,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
	Revoked       bool     `json:"revoked,omitempty"`
	RPM           int      `json:"rpm,omitempty"`
	TPM           int      `json:"tpm,omitempty"`
	Key           string   `json:"key,omitempty"`
	KeyShownOnce  bool     `json:"key_shown_once,omitempty"`
}

func publicClientKey(k config.VirtualKeyConfig, plaintext string) clientKeyResponse {
	return clientKeyResponse{ID: k.ID, Name: k.Name, TenantID: k.TenantID, ProjectID: k.ProjectID, TeamID: k.TeamID, Role: k.Role, AllowedModels: k.AllowedModels, AllowedRoutes: k.AllowedRoutes, ExpiresAt: k.ExpiresAt, Revoked: k.Revoked, RPM: k.RPM, TPM: k.TPM, Key: plaintext, KeyShownOnce: plaintext != ""}
}

func (s *Server) adminClientKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		out := make([]clientKeyResponse, 0, len(cfg.ClientAuth.VirtualKeys))
		for _, key := range cfg.ClientAuth.VirtualKeys {
			out = append(out, publicClientKey(key, ""))
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": out})
	case http.MethodPost:
		var in clientKeyCreateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		plain, digest, err := newVirtualClientKey()
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "could not generate client key")
			return
		}
		if in.Role == "" {
			in.Role = "developer"
		}
		if in.ExpiresAt != "" {
			if expiry, err := time.Parse(time.RFC3339, in.ExpiresAt); err != nil || !expiry.After(time.Now().UTC()) {
				errorJSON(w, http.StatusBadRequest, "expires_at must be a future RFC3339 timestamp")
				return
			}
		}
		id := fmt.Sprintf("vk_%d", time.Now().UnixNano())
		key := config.VirtualKeyConfig{ID: id, Name: strings.TrimSpace(in.Name), KeyHash: digest, TenantID: strings.TrimSpace(in.TenantID), ProjectID: strings.TrimSpace(in.ProjectID), TeamID: strings.TrimSpace(in.TeamID), Role: in.Role, AllowedModels: in.AllowedModels, AllowedRoutes: in.AllowedRoutes, ExpiresAt: in.ExpiresAt, RPM: in.RPM, TPM: in.TPM}
		if _, err := s.mutateConfig(func(cfg *config.Config) error {
			cfg.ClientAuth.VirtualKeys = append(cfg.ClientAuth.VirtualKeys, key)
			cfg.ClientAuth.Enabled = true
			return nil
		}); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, publicClientKey(key, plain))
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) adminClientKeyByID(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/client-keys/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		errorJSON(w, http.StatusBadRequest, "client key id is required")
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "rotate" {
		if r.Method != http.MethodPost {
			errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		plain, digest, err := newVirtualClientKey()
		if err != nil {
			errorJSON(w, 500, "could not generate client key")
			return
		}
		var rotated config.VirtualKeyConfig
		_, err = s.mutateConfig(func(cfg *config.Config) error {
			for i := range cfg.ClientAuth.VirtualKeys {
				if cfg.ClientAuth.VirtualKeys[i].ID == id {
					cfg.ClientAuth.VirtualKeys[i].Revoked = true
					rotated = cfg.ClientAuth.VirtualKeys[i]
					rotated.ID = fmt.Sprintf("vk_%d", time.Now().UnixNano())
					rotated.KeyHash = digest
					rotated.Revoked = false
					cfg.ClientAuth.VirtualKeys = append(cfg.ClientAuth.VirtualKeys, rotated)
					return nil
				}
			}
			return fmt.Errorf("client key %q not found", id)
		})
		if err != nil {
			errorJSON(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, publicClientKey(rotated, plain))
		return
	}
	if r.Method != http.MethodDelete {
		errorJSON(w, http.StatusMethodNotAllowed, "only DELETE or POST /rotate is supported")
		return
	}
	_, err := s.mutateConfig(func(cfg *config.Config) error {
		for i := range cfg.ClientAuth.VirtualKeys {
			if cfg.ClientAuth.VirtualKeys[i].ID == id {
				cfg.ClientAuth.VirtualKeys[i].Revoked = true
				return nil
			}
		}
		return fmt.Errorf("client key %q not found", id)
	})
	if err != nil {
		errorJSON(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
