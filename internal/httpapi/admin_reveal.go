package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// Controlled secret reveal (CCR-parity spec 2.4).
//
// Saved provider keys are never part of list/get/snapshot/SSE/metrics output.
// An operator who explicitly wants to see a saved key can ask for it one entry
// at a time through POST /admin/api/providers/{id}/reveal-secret. The request
// already passes the normal admin gate (authentication, role, CSRF, base rate
// limit, pre/post audit records). On top of that this handler:
//
//   - requires admin.allow_secret_reveal (default: loopback-only listener),
//   - applies a much stricter per-subject rate limit,
//   - writes a dedicated audit event BEFORE the secret leaves the process and
//     fails closed (no secret) when that audit write fails,
//   - never reveals a key that is sourced from an environment variable, because
//     that value does not live in the gateway configuration at all.
//
// The secret appears only in this one response body. It is never logged.

const (
	revealRateWindow    = time.Minute
	revealRateMax       = 10
	maxRevealRateSubjs  = 256
	maxRevealRequestLen = 4 << 10
)

type revealWindow struct {
	start time.Time
	count int
}

// revealLimiter is a small fixed-window limiter keyed by admin subject. The
// zero value is ready to use.
type revealLimiter struct {
	mu      sync.Mutex
	windows map[string]*revealWindow
}

func (l *revealLimiter) allow(subject string, now time.Time) bool {
	if strings.TrimSpace(subject) == "" {
		subject = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = map[string]*revealWindow{}
	}
	if len(l.windows) >= maxRevealRateSubjs {
		for k, w := range l.windows {
			if now.Sub(w.start) >= revealRateWindow {
				delete(l.windows, k)
			}
		}
		if _, known := l.windows[subject]; !known && len(l.windows) >= maxRevealRateSubjs {
			return false
		}
	}
	w, ok := l.windows[subject]
	if !ok || now.Sub(w.start) >= revealRateWindow {
		l.windows[subject] = &revealWindow{start: now, count: 1}
		return true
	}
	if w.count >= revealRateMax {
		return false
	}
	w.count++
	return true
}

type revealSecretRequest struct {
	// CredentialIndex selects one entry of a credential pool. Omit it to reveal
	// the provider's single API key.
	CredentialIndex *int `json:"credential_index,omitempty"`
}

// secretPreview preserves the legacy response field without disclosing any
// characters from the credential. Full values are available only through the
// separately authorized, audited reveal operation.
func secretPreview(secret string) string {
	if secret == "" {
		return ""
	}
	return maskedPlaceholder
}

// credentialMeta describes pool entries without any secret value.
func credentialMeta(creds []config.CredentialConfig) []map[string]any {
	out := make([]map[string]any, 0, len(creds))
	for i, c := range creds {
		source, preview := "literal", ""
		if c.APIKeyEnv != "" && os.Getenv(c.APIKeyEnv) != "" {
			source = "env"
		} else {
			preview = secretPreview(c.APIKey)
		}
		out = append(out, map[string]any{
			"index": i, "name": c.Name, "enabled": c.Enabled,
			"source": source, "preview": preview,
			"email_masked": providers.MaskEmail(c.Email),
			"priority":     c.Priority, "weight": c.Weight,
		})
	}
	return out
}

// revealable picks the stored literal key a reveal request refers to. status is
// 0 on success. For an env-sourced key the returned envName is set so the
// caller can tell the operator where the value actually lives.
func revealable(p config.ProviderConfig, index *int) (secret string, envName string, status int, msg string) {
	if index != nil {
		if *index < 0 || *index >= len(p.Credentials) {
			return "", "", http.StatusNotFound, "credential index out of range"
		}
		c := p.Credentials[*index]
		if c.APIKeyEnv != "" && os.Getenv(c.APIKeyEnv) != "" {
			return "", c.APIKeyEnv, http.StatusConflict, "this key comes from an environment variable and is never revealed"
		}
		if c.APIKey == "" {
			return "", "", http.StatusNotFound, "no stored key for this credential"
		}
		return c.APIKey, "", 0, ""
	}
	if len(p.Credentials) > 0 && p.APIKey == "" && p.APIKeyEnv == "" {
		return "", "", http.StatusBadRequest, "provider uses a credential pool; credential_index is required"
	}
	if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "" {
		return "", p.APIKeyEnv, http.StatusConflict, "this key comes from an environment variable and is never revealed"
	}
	if p.APIKey == "" {
		return "", "", http.StatusNotFound, "no stored key for this provider"
	}
	return p.APIKey, "", 0, ""
}

func (s *Server) adminProviderRevealSecret(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") {
		errorJSON(w, http.StatusBadRequest, "provider id required")
		return
	}
	cfg := s.currentConfig()
	if !cfg.SecretRevealAllowed() {
		errorJSON(w, http.StatusForbidden, "secret reveal is disabled (admin.allow_secret_reveal)")
		return
	}
	identity := adminIdentityFromRequest(r)
	if !s.revealRL.allow(identity.Subject, time.Now()) {
		w.Header().Set("Retry-After", "60")
		errorJSON(w, http.StatusTooManyRequests, "secret reveal rate limit exceeded")
		return
	}
	var in revealSecretRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRevealRequestLen+1))
	if err != nil || len(raw) > maxRevealRequestLen {
		errorJSON(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	idx := cfg.ProviderIndex(id)
	if idx < 0 {
		errorJSON(w, http.StatusNotFound, "provider not found")
		return
	}
	secret, envName, status, msg := revealable(cfg.Providers[idx], in.CredentialIndex)
	if status != 0 {
		if envName != "" {
			writeJSON(w, status, map[string]any{
				"error":  map[string]any{"type": "gateway_error", "message": msg},
				"source": "env", "env_name": envName,
			})
			return
		}
		errorJSON(w, status, msg)
		return
	}
	// Audit first: no durable record means no secret. The record names the
	// provider and entry only; it never contains key material.
	target := "provider:" + id
	if in.CredentialIndex != nil {
		target = fmt.Sprintf("%s credential:%d", target, *in.CredentialIndex)
	}
	event := auditEvent(r, identity.Subject, "provider.secret.revealed", "success", http.StatusOK, "admin", authz.ManageKeys)
	event.Target = target
	if err := s.recordAudit(event); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin audit store unavailable")
		return
	}
	resp := map[string]any{"secret": secret, "source": "literal"}
	if in.CredentialIndex != nil {
		resp["credential_index"] = *in.CredentialIndex
	}
	writeJSON(w, http.StatusOK, resp)
}
