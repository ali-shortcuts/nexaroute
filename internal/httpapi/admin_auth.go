package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

const (
	adminSessionCookie  = "nexaroute_admin_session"
	adminOIDCFlowCookie = "nexaroute_admin_oidc_flow"
)

func adminSecurityStorePath(configuredPath, configPath string) (string, error) {
	configuredPath = strings.TrimSpace(configuredPath)
	configPath = strings.TrimSpace(configPath)
	if configuredPath == "" {
		if configPath == "" {
			return "", errors.New("admin security store path is required when no config file path is available")
		}
		configuredPath = configPath + ".security.db"
	}
	if filepath.IsAbs(configuredPath) {
		return filepath.Clean(configuredPath), nil
	}
	base := "."
	if configPath != "" {
		base = filepath.Dir(configPath)
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve admin security store directory: %w", err)
	}
	return filepath.Join(absBase, configuredPath), nil
}

func (s *Server) SecurityStoreError() error { return s.securityErr }

func (s *Server) CloseSecurityStore() error {
	if s.securityDB == nil {
		return nil
	}
	return s.securityDB.Close()
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	_, _, ok, err := s.authenticateAdmin(r)
	return err == nil && ok
}

func (s *Server) authenticateAdmin(r *http.Request) (authz.Identity, string, bool, error) {
	if s.securityErr != nil || s.securityDB == nil {
		return authz.Identity{}, "", false, errors.New("admin security store unavailable")
	}
	cfg := s.adminConfigSnapshot()
	if cfg.BindLocalOnly && !remoteIsLoopback(r.RemoteAddr) {
		return authz.Identity{}, "", false, nil
	}
	identity, _, hasCookie, ok, err := s.sessionIdentity(r)
	if err != nil {
		return authz.Identity{}, "oidc-session", false, err
	}
	if ok {
		return identity, "oidc-session", true, nil
	}
	if cfg.OIDC.Enabled {
		// A broken or unavailable provider never downgrades to a legacy key or
		// loopback trust. Existing sessions are also rejected if discovery failed.
		if s.oidc == nil || s.oidcErr != nil {
			return authz.Identity{}, "oidc-session", false, errors.New("OIDC authentication unavailable")
		}
		if hasCookie {
			return authz.Identity{}, "oidc-session", false, nil
		}
	}
	if !cfg.EmergencyAccessEnabled {
		return authz.Identity{}, "", false, nil
	}
	if !legacyAdminAuthorized(r, cfg) {
		return authz.Identity{}, "emergency", false, nil
	}
	return legacyAdminIdentity(), "emergency", true, nil
}

func legacyAdminAuthorized(r *http.Request, cfg config.AdminConfig) bool {
	remoteLoopback := remoteIsLoopback(r.RemoteAddr)
	if cfg.BindLocalOnly && !remoteLoopback {
		return false
	}
	if cfg.APIKey != "" {
		got := r.Header.Get("x-admin-key")
		if got == "" {
			if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
				got = strings.TrimPrefix(value, "Bearer ")
			}
		}
		return len(got) == len(cfg.APIKey) && subtle.ConstantTimeCompare([]byte(got), []byte(cfg.APIKey)) == 1
	}
	// Keyless emergency mode is restricted to loopback clients and a loopback
	// Host to prevent DNS-rebound browser origins from reading admin data.
	return remoteLoopback && adminHostAllowed(r)
}

func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = strings.Trim(remoteAddr, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cookieValue(r *http.Request, name string) (string, bool, bool) {
	var value string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			count++
			value = cookie.Value
		}
	}
	if count == 0 {
		return "", false, true
	}
	if count != 1 || value == "" {
		return "", true, false
	}
	return value, true, true
}

func opaqueSessionHash(value string) (string, bool) {
	if len(value) != 43 {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return "", false
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:]), true
}

func (s *Server) sessionIdentity(r *http.Request) (authz.Identity, string, bool, bool, error) {
	cfg := s.adminConfigSnapshot()
	value, present, wellFormed := cookieValue(r, adminSessionCookie)
	if !present {
		return authz.Identity{}, "", false, false, nil
	}
	if !wellFormed {
		return authz.Identity{}, "", true, false, nil
	}
	hash, valid := opaqueSessionHash(value)
	if !valid {
		return authz.Identity{}, "", true, false, nil
	}
	if !cfg.OIDC.Enabled || s.oidc == nil || s.oidcErr != nil {
		return authz.Identity{}, hash, true, false, nil
	}
	session, found, err := s.securityDB.TouchSession(r.Context(), hash, time.Now().UTC(), time.Duration(cfg.IdleTimeoutSeconds)*time.Second)
	if err != nil {
		return authz.Identity{}, hash, true, false, err
	}
	if !found {
		return authz.Identity{}, hash, true, false, nil
	}
	if session.PolicyHash != s.oidc.policyHash || session.Issuer != cfg.OIDC.IssuerURL || session.ExpiresAt.After(session.CreatedAt.Add(time.Duration(cfg.SessionTTLSeconds)*time.Second)) {
		event := auditEvent(r, session.Subject, "auth.session.policy_mismatch", "revoked", http.StatusUnauthorized, "oidc-session", "")
		if err := s.securityDB.RevokeSession(context.Background(), hash, event); err != nil {
			return authz.Identity{}, hash, true, false, err
		}
		return authz.Identity{}, hash, true, false, nil
	}
	roles := make([]authz.Role, 0, len(session.Roles))
	for _, role := range session.Roles {
		switch authz.Role(role) {
		case authz.RoleViewer, authz.RoleOperator, authz.RoleAdmin:
			roles = append(roles, authz.Role(role))
		default:
			return authz.Identity{}, hash, true, false, nil
		}
	}
	if len(roles) != 1 || session.Subject == "" {
		return authz.Identity{}, hash, true, false, nil
	}
	return authz.Identity{Subject: session.Subject, Issuer: session.Issuer, Roles: roles}, hash, true, true, nil
}

func adminIdentityFromRequest(r *http.Request) authz.Identity {
	identity, _ := r.Context().Value(adminIdentityContextKey{}).(authz.Identity)
	return identity
}

type adminIdentityContextKey struct{}

func withAdminIdentity(r *http.Request, identity authz.Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), adminIdentityContextKey{}, identity))
}

func auditEvent(r *http.Request, actor, action, outcome string, status int, authentication string, permission authz.Permission) securitystore.AuditEvent {
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = r.Header.Get("X-Trace-ID")
	}
	return securitystore.AuditEvent{
		Timestamp: time.Now().UTC(), RequestID: requestID, Actor: actor, Action: action,
		Target: r.URL.Path, Method: r.Method, Path: r.URL.Path, Outcome: outcome,
		Status: status, Authn: authentication, Permission: string(permission),
	}
}

func (s *Server) recordAudit(event securitystore.AuditEvent) error {
	if s.securityErr != nil || s.securityDB == nil {
		return errors.New("admin security store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.securityDB.AppendAudit(ctx, event)
}

func (s *Server) denyAdminRequest(w http.ResponseWriter, r *http.Request, identity authz.Identity, authentication string, permission authz.Permission, status int, message string) {
	actor := identity.Subject
	if actor == "" {
		actor = "anonymous"
	}
	event := auditEvent(r, actor, "admin.access.denied", "denied", status, authentication, permission)
	if err := s.recordAudit(event); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin audit store unavailable")
		return
	}
	errorJSON(w, status, message)
}

func setAdminSessionCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminSessionCookie, Value: value, Path: "/admin/", HttpOnly: true,
		Secure: requestIsSecure(r), SameSite: http.SameSiteLaxMode,
		Expires: expires.UTC(), MaxAge: maxAge,
	})
}

func clearAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: adminSessionCookie, Value: "", Path: "/admin/", HttpOnly: true,
		Secure: requestIsSecure(r), SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name: adminCSRFCookie, Value: "", Path: "/admin/", HttpOnly: true,
		Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
	})
	clearOIDCFlowCookie(w, r)
}

func setOIDCFlowCookie(w http.ResponseWriter, r *http.Request, value string) {
	expires := time.Now().Add(oidcLoginLifetime).UTC()
	http.SetCookie(w, &http.Cookie{
		Name: adminOIDCFlowCookie, Value: value, Path: "/admin/", HttpOnly: true,
		Secure: requestIsSecure(r), SameSite: http.SameSiteLaxMode,
		Expires: expires, MaxAge: int(oidcLoginLifetime.Seconds()),
	})
}

func clearOIDCFlowCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: adminOIDCFlowCookie, Value: "", Path: "/admin/", HttpOnly: true,
		Secure: requestIsSecure(r), SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
	})
}

func strictSessionCSRFAllowed(r *http.Request) bool {
	if !sameOrigin(r) {
		return false
	}
	cookie, err := r.Cookie(adminCSRFCookie)
	if err != nil || len(cookie.Value) != 43 {
		return false
	}
	got := r.Header.Get("X-NexaRoute-CSRF")
	return len(got) == len(cookie.Value) && subtle.ConstantTimeCompare([]byte(got), []byte(cookie.Value)) == 1
}

func (s *Server) adminAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.securityErr != nil || s.securityDB == nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin security store unavailable")
		return
	}
	cfg := s.adminConfigSnapshot()
	result := map[string]any{
		"oidc_enabled": cfg.OIDC.Enabled, "oidc_available": !cfg.OIDC.Enabled || (s.oidc != nil && s.oidcErr == nil),
		"emergency_access_enabled": cfg.EmergencyAccessEnabled, "api_key_configured": cfg.APIKey != "",
		"authenticated": false,
	}
	if cfg.OIDC.Enabled && (s.oidc == nil || s.oidcErr != nil) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	identity, _, _, ok, err := s.sessionIdentity(r)
	if err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin session store unavailable")
		return
	}
	if ok {
		result["authenticated"] = true
		result["subject"] = identity.Subject
		roles := make([]string, 0, len(identity.Roles))
		for _, role := range identity.Roles {
			roles = append(roles, string(role))
		}
		result["roles"] = roles
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.securityErr != nil || s.securityDB == nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin security store unavailable")
		return
	}
	if !s.adminConfigSnapshot().OIDC.Enabled {
		errorJSON(w, http.StatusNotFound, "OIDC login is not enabled")
		return
	}
	if s.oidc == nil || s.oidcErr != nil {
		errorJSON(w, http.StatusServiceUnavailable, "OIDC authentication is unavailable")
		return
	}
	oldCookie, oldCookiePresent, oldCookieOK := cookieValue(r, adminSessionCookie)
	if oldCookiePresent && !oldCookieOK {
		errorJSON(w, http.StatusBadRequest, "invalid admin session cookie")
		return
	}
	oldHash := ""
	if oldCookieOK && oldCookie != "" {
		if candidate, valid := opaqueSessionHash(oldCookie); valid {
			oldHash = candidate
		}
	}
	browserBinding, err := randomURLToken(32)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "could not start OIDC authentication")
		return
	}
	authorizationURL, _, err := s.oidc.Begin(oldHash, browserBinding)
	if err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "could not start OIDC authentication")
		return
	}
	if err := s.recordAudit(auditEvent(r, "anonymous", "auth.login.started", "attempt", http.StatusFound, "oidc", "")); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin audit store unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	setOIDCFlowCookie(w, r, browserBinding)
	http.Redirect(w, r, authorizationURL, http.StatusFound)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.securityErr != nil || s.securityDB == nil || s.oidc == nil || s.oidcErr != nil {
		errorJSON(w, http.StatusServiceUnavailable, "OIDC authentication is unavailable")
		return
	}
	query := r.URL.Query()
	states, stateOK := query["state"]
	if !stateOK || len(states) != 1 {
		s.writeLoginFailure(w, r)
		return
	}
	browserBinding, bindingPresent, bindingWellFormed := cookieValue(r, adminOIDCFlowCookie)
	if !bindingPresent || !bindingWellFormed {
		s.writeLoginFailure(w, r)
		return
	}
	transaction, validState := s.oidc.takeState(states[0], browserBinding)
	if !validState {
		s.writeLoginFailure(w, r)
		return
	}
	clearOIDCFlowCookie(w, r)
	if providerErrors := query["error"]; len(providerErrors) > 0 {
		s.writeLoginFailure(w, r)
		return
	}
	codes, codeOK := query["code"]
	if !codeOK || len(codes) != 1 {
		s.writeLoginFailure(w, r)
		return
	}
	identity, oldHash, err := s.oidc.Complete(r.Context(), transaction, codes[0])
	if err != nil {
		s.writeLoginFailure(w, r)
		return
	}
	sessionID, err := randomURLToken(32)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "could not create admin session")
		return
	}
	sessionHash := opaqueHash(sessionID)
	now := time.Now().UTC()
	cfg := s.adminConfigSnapshot()
	expires := now.Add(time.Duration(cfg.SessionTTLSeconds) * time.Second)
	roles := make([]string, 0, len(identity.Roles))
	for _, role := range identity.Roles {
		roles = append(roles, string(role))
	}
	event := auditEvent(r, identity.Subject, "auth.login.succeeded", "success", http.StatusSeeOther, "oidc", "")
	session := securitystore.Session{
		Subject: identity.Subject, Issuer: identity.Issuer, Roles: roles,
		PolicyHash: s.oidc.policyHash, CreatedAt: now, LastSeen: now, ExpiresAt: expires,
	}
	if err := s.securityDB.RotateSession(context.Background(), oldHash, sessionHash, session, event); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "could not persist admin session")
		return
	}
	setAdminSessionCookie(w, r, sessionID, expires)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func opaqueHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (s *Server) adminAuthLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.securityErr != nil || s.securityDB == nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin security store unavailable")
		return
	}
	if !strictSessionCSRFAllowed(r) {
		errorJSON(w, http.StatusForbidden, "same-origin request and valid CSRF token required")
		return
	}
	identity, hash, _, ok, err := s.sessionIdentity(r)
	if err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin session store unavailable")
		return
	}
	if !ok {
		clearAdminSessionCookie(w, r)
		errorJSON(w, http.StatusUnauthorized, "authenticated session required")
		return
	}
	event := auditEvent(r, identity.Subject, "auth.logout", "success", http.StatusOK, "oidc-session", "")
	if err := s.securityDB.RevokeSession(context.Background(), hash, event); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "could not persist logout")
		return
	}
	clearAdminSessionCookie(w, r)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) adminAuditRecords(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			errorJSON(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	events, err := s.securityDB.AuditRecords(ctx, limit)
	if err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin audit store unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
}

func (s *Server) writeLoginFailure(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := s.recordAudit(auditEvent(r, "anonymous", "auth.login.failed", "denied", http.StatusUnauthorized, "oidc", "")); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin audit store unavailable")
		return
	}
	errorJSON(w, http.StatusUnauthorized, "authentication failed")
}
