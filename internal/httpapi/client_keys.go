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
	// Budget webhook alert overrides for the new key's budget (all
	// optional; empty inherits the global budget_alerts policy).
	// BudgetUSD itself is set through the config file.
	BudgetWebhookURL      string    `json:"budget_webhook_url"`
	BudgetAlertThresholds []float64 `json:"budget_alert_thresholds"`
	BudgetPeriod          string    `json:"budget_period"`
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
	// BudgetUSD is emitted ALWAYS (no omitempty): its mere presence
	// tells clients the budget backend exposes per-key limits, while
	// 0 means the key carries no budget cap. It is configuration
	// metadata, never secret material.
	BudgetUSD float64 `json:"budget_usd"`
	// Live budget state from this gateway's in-memory ledger,
	// emitted ONLY for keys with a configured limit — remaining
	// budget is shown only when a limit makes it calculable
	// (#435). Committed is settled spend; Reserved is in-flight
	// reservations not yet settled. Per-instance scope: these
	// numbers describe THIS process's ledger, never a global
	// cross-replica budget.
	BudgetCommittedUSD *float64 `json:"budget_committed_usd,omitempty"`
	BudgetReservedUSD  *float64 `json:"budget_reserved_usd,omitempty"`
	BudgetRemainingUSD *float64 `json:"budget_remaining_usd,omitempty"`
	// Budget webhook alert overrides, mirroring the virtual-key
	// config fields. The URL carries no credentials by validation
	// and is operator configuration, safe for admin readers.
	BudgetWebhookURL      string    `json:"budget_webhook_url,omitempty"`
	BudgetAlertThresholds []float64 `json:"budget_alert_thresholds,omitempty"`
	BudgetPeriod          string    `json:"budget_period,omitempty"`
	Key                   string    `json:"key,omitempty"`
	KeyShownOnce          bool      `json:"key_shown_once,omitempty"`
}

func publicClientKey(k config.VirtualKeyConfig, plaintext string) clientKeyResponse {
	return clientKeyResponse{ID: k.ID, Name: k.Name, TenantID: k.TenantID, ProjectID: k.ProjectID, TeamID: k.TeamID, Role: k.Role, AllowedModels: k.AllowedModels, AllowedRoutes: k.AllowedRoutes, ExpiresAt: k.ExpiresAt, Revoked: k.Revoked, RPM: k.RPM, TPM: k.TPM, BudgetUSD: k.BudgetUSD, BudgetWebhookURL: k.BudgetWebhookURL, BudgetAlertThresholds: k.BudgetAlertThresholds, BudgetPeriod: k.BudgetPeriod, Key: plaintext, KeyShownOnce: plaintext != ""}
}

func (s *Server) adminClientKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.currentConfig()
		out := make([]clientKeyResponse, 0, len(cfg.ClientAuth.VirtualKeys))
		for _, key := range cfg.ClientAuth.VirtualKeys {
			resp := publicClientKey(key, "")
			if key.BudgetUSD > 0 && s.budgets != nil {
				committed, reserved := s.budgets.Snapshot(key.ID, time.Now())
				remaining := key.BudgetUSD - committed - reserved
				if remaining < 0 {
					remaining = 0
				}
				resp.BudgetCommittedUSD = &committed
				resp.BudgetReservedUSD = &reserved
				resp.BudgetRemainingUSD = &remaining
			}
			out = append(out, resp)
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
		key := config.VirtualKeyConfig{ID: id, Name: strings.TrimSpace(in.Name), KeyHash: digest, TenantID: strings.TrimSpace(in.TenantID), ProjectID: strings.TrimSpace(in.ProjectID), TeamID: strings.TrimSpace(in.TeamID), Role: in.Role, AllowedModels: in.AllowedModels, AllowedRoutes: in.AllowedRoutes, ExpiresAt: in.ExpiresAt, RPM: in.RPM, TPM: in.TPM, BudgetWebhookURL: strings.TrimSpace(in.BudgetWebhookURL), BudgetAlertThresholds: in.BudgetAlertThresholds, BudgetPeriod: strings.ToLower(strings.TrimSpace(in.BudgetPeriod))}
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
