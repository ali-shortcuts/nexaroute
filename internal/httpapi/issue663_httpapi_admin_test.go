package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Issue #663 (httpapi scope): behavior tests for the admin handlers and
// helpers that the ba5ff2f coverprofile left below full coverage. Every
// test asserts a real status, body, or decision; nothing here only checks
// non-nil and nothing pads coverage.

func issue663AdminRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:23456"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func issue663Decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("status=%d invalid JSON: %v; body=%s", rr.Code, err, rr.Body.String())
	}
	return out
}

func issue663SessionValue() string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = 0xAB
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestIssue663HttpapiDiscoverMethodAndCredentialIndex(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"dm1"},{"id":"dm2"}]}`)
	}))
	defer upstream.Close()
	s := testGateway(t, config.Default())

	// The admin middleware denies GET on this path with 403 before the
	// handler runs, so the handler's own method branch is covered by a
	// direct call, the same pattern the existing admin coverage tests use.
	w := httptest.NewRecorder()
	s.adminProviderDiscover(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/provider-discover", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET discover status=%d want 405 body=%s", w.Code, w.Body.String())
	}
	var rr *httptest.ResponseRecorder

	rr = issue663AdminRequest(s, http.MethodPost, "/admin/api/provider-discover",
		`{"provider":{"id":"d1","type":"openai_compatible","base_url":"`+upstream.URL+`"},"credential_index":9}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "out of range") {
		t.Fatalf("out-of-range credential index status=%d body=%s", rr.Code, rr.Body.String())
	}

	body := `{"provider":{"id":"d1","type":"openai_compatible","base_url":"` + upstream.URL + `","auth_mode":"none",` +
		`"credentials":[{"name":"pool-a","api_key":"test-fake-pool-key-123","enabled":true}]},"credential_index":0}`
	rr = issue663AdminRequest(s, http.MethodPost, "/admin/api/provider-discover", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("discover status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := issue663Decode(t, rr)
	if out["ok"] != true || out["credential_index"] != float64(0) || out["credential_name"] != "pool-a" {
		t.Fatalf("discover credential fields wrong: %v", out)
	}
	models, ok := out["models"].([]any)
	if !ok || len(models) != 2 || models[0] != "dm1" || models[1] != "dm2" {
		t.Fatalf("discovered models=%v", out["models"])
	}
	if strings.Contains(rr.Body.String(), "test-fake-pool-key-123") {
		t.Fatal("discover response leaked the pool credential")
	}
}

func TestIssue663HttpapiCheckNoModelsEndpointAndProbeMethod(t *testing.T) {
	s := testGateway(t, config.Default())
	rr := issue663AdminRequest(s, http.MethodPost, "/admin/api/provider-check",
		`{"provider":{"id":"x","type":"opencode-go","base_url":"http://127.0.0.1:9"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("check status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := issue663Decode(t, rr)
	if out["ok"] != false || !strings.Contains(rr.Body.String(), "does not expose a models endpoint") {
		t.Fatalf("opencode-go check must report no models endpoint: %v", out)
	}
	w := httptest.NewRecorder()
	s.adminProbe(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/probe", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe status=%d want 405 body=%s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiNormalizeProviderWeightDefault(t *testing.T) {
	// Effective behavior only: normalizeProvider calls ApplyDefaults
	// first, and ApplyDefaults already forces a zero model weight to 1,
	// so normalizeProvider's own weight assignment below is unreachable
	// through this function (see the final report). This test pins the
	// observable outcome — weight 1, trimmed name, slug id — rather
	// than pretending to execute the dead branch.
	p := config.ProviderConfig{Models: []config.ModelConfig{
		{Model: " alpha ", Weight: 0},
		{Model: "beta", ID: "custom", Weight: 3},
	}}
	normalizeProvider(&p)
	if p.Models[0].Model != "alpha" || p.Models[0].Weight != 1 || p.Models[0].ID != "alpha" {
		t.Fatalf("weight-0 model not normalized: %+v", p.Models[0])
	}
	if p.Models[1].ID != "custom" || p.Models[1].Weight != 3 {
		t.Fatalf("explicit model changed: %+v", p.Models[1])
	}
}

func TestIssue663HttpapiAdminStoreUnavailable(t *testing.T) {
	// Direct unit checks: a missing security store must fail closed
	// instead of panicking or authorizing.
	if _, _, ok, err := (&Server{}).authenticateAdmin(httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)); err == nil || ok {
		t.Fatalf("authenticateAdmin without store: ok=%v err=%v", ok, err)
	}
	if err := (&Server{}).recordAudit(securitystore.AuditEvent{Action: "x"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("recordAudit without store: err=%v", err)
	}
	srv := testGateway(t, config.Default())
	srv.securityErr = errors.New("boom")
	w := httptest.NewRecorder()
	srv.authLogin(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "admin security store unavailable") {
		t.Fatalf("authLogin with store error: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.adminAuthLogout(w, httptest.NewRequest(http.MethodPost, "http://gateway/admin/auth/logout", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("logout with store error: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiSessionIdentityCookieBranches(t *testing.T) {
	srv := testGateway(t, config.Default())

	r := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	if _, _, present, ok, err := srv.sessionIdentity(r); err != nil || present || ok {
		t.Fatalf("no cookie: present=%v ok=%v err=%v", present, ok, err)
	}
	r = httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: ""})
	if _, _, present, ok, err := srv.sessionIdentity(r); err != nil || !present || ok {
		t.Fatalf("empty cookie: present=%v ok=%v err=%v", present, ok, err)
	}
	value := issue663SessionValue()
	r = httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: value})
	_, hash, present, ok, err := srv.sessionIdentity(r)
	if err != nil || !present || ok {
		t.Fatalf("valid cookie with OIDC disabled: present=%v ok=%v err=%v", present, ok, err)
	}
	digest := sha256.Sum256([]byte(value))
	if hash != hex.EncodeToString(digest[:]) {
		t.Fatalf("session hash=%q want sha256 of cookie value", hash)
	}
}

type issue663RevokeFailBackend struct {
	securitystore.Backend
}

func (b issue663RevokeFailBackend) RevokeSession(context.Context, string, securitystore.AuditEvent) error {
	return errors.New("revoke intentionally failed")
}

func TestIssue663HttpapiSessionIdentityRevokeFailure(t *testing.T) {
	srv := testGateway(t, config.Default())
	cookie := installTestOIDCSession(t, srv, authz.RoleViewer)
	now := time.Now().UTC().Truncate(time.Second)
	// A session whose policy fingerprint no longer matches the OIDC
	// policy must be revoked; if that revocation itself fails, the
	// identity lookup must surface the error, not a session.
	bad := securitystore.Session{
		Subject: "mismatch-user", Issuer: "https://issuer.example.test", Roles: []string{"viewer"},
		PolicyHash: "stale-policy", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour),
	}
	badCookie := issue663SessionValue()
	if err := srv.securityDB.CreateSession(context.Background(), opaqueHash(badCookie), bad,
		securitystore.AuditEvent{Actor: "mismatch-user", Action: "login", Outcome: "success", Status: http.StatusSeeOther}); err != nil {
		t.Fatal(err)
	}
	orig := srv.securityDB
	srv.securityDB = issue663RevokeFailBackend{orig}
	defer func() { srv.securityDB = orig }()
	r := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: badCookie})
	if _, _, present, ok, err := srv.sessionIdentity(r); err == nil || !present || ok {
		t.Fatalf("revoke failure: present=%v ok=%v err=%v", present, ok, err)
	}
	if _, _, ok, err := srv.authenticateAdmin(r); err == nil || ok {
		t.Fatalf("authenticateAdmin must propagate the revoke error: ok=%v err=%v", ok, err)
	}
	_ = cookie
}

func TestIssue663HttpapiAuthLoginPendingTableFull(t *testing.T) {
	srv := testGateway(t, config.Default())
	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: "https://issuer.example.test"}
	srv.runtimeMu.Unlock()
	pending := make(map[string]pendingOIDCLogin, maxPendingOIDCLoginsPerIP)
	for i := 0; i < maxPendingOIDCLoginsPerIP; i++ {
		pending[string(rune('a'+i))+strings.Repeat("k", 40)] = pendingOIDCLogin{
			sourceIP: "127.0.0.1", expiresAt: time.Now().Add(time.Hour),
		}
	}
	srv.oidc = &oidcAuthenticator{issuer: "https://issuer.example.test", policyHash: "coverage-policy", pending: pending}
	srv.oidcErr = nil
	r := httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	srv.authLogin(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "could not start OIDC authentication") {
		t.Fatalf("pending-full login: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiAuthCallbackUnavailable(t *testing.T) {
	srv := testGateway(t, config.Default())
	w := httptest.NewRecorder()
	srv.authCallback(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/callback?state=x&code=y", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "OIDC authentication is unavailable") {
		t.Fatalf("callback without OIDC: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiAuthLogoutSessionStoreError(t *testing.T) {
	srv := testGateway(t, config.Default())
	cookie := installTestOIDCSession(t, srv, authz.RoleViewer)
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := httptest.NewRequest(http.MethodPost, "http://gateway/admin/auth/logout", nil)
	r.Host = "gateway"
	r.Header.Set("Origin", "http://gateway")
	r.Header.Set("X-NexaRoute-CSRF", token)
	r.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: token})
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookie})
	w := httptest.NewRecorder()
	srv.adminAuthLogout(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "admin session store unavailable") {
		t.Fatalf("logout with closed session store: %d %s", w.Code, w.Body.String())
	}
}

type issue663ManyProblemsBackend struct {
	securitystore.Backend
}

func (b issue663ManyProblemsBackend) VerifyAuditChain(context.Context) (securitystore.ChainReport, error) {
	problems := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		problems = append(problems, "problem "+strings.Repeat("x", i))
	}
	return securitystore.ChainReport{TotalRecords: 12, ChainedRecords: 12, Verified: false, Problems: problems}, nil
}

func TestIssue663HttpapiAuditRecordsErrorsAndTruncation(t *testing.T) {
	srv := testGateway(t, config.Default())
	w := httptest.NewRecorder()
	srv.adminAuditRecords(w, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/audit", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("audit POST: %d allow=%q", w.Code, w.Header().Get("Allow"))
	}
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.adminAuditRecords(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/audit", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "admin audit store unavailable") {
		t.Fatalf("audit with closed store: %d %s", w.Code, w.Body.String())
	}

	srv2 := testGateway(t, config.Default())
	orig := srv2.securityDB
	srv2.securityDB = issue663ManyProblemsBackend{orig}
	defer func() { srv2.securityDB = orig }()
	w = httptest.NewRecorder()
	srv2.adminAuditRecords(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/audit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("audit with problem chain: %d %s", w.Code, w.Body.String())
	}
	payload := issue663Decode(t, w)
	integrity, ok := payload["integrity"].(map[string]any)
	if !ok || integrity["status"] != "failed" {
		t.Fatalf("integrity block wrong: %v", payload["integrity"])
	}
	problems, ok := integrity["problems"].([]any)
	if !ok || len(problems) != 10 {
		t.Fatalf("problems must be truncated to 10, got %v", integrity["problems"])
	}
}

func TestIssue663HttpapiRevealableBranches(t *testing.T) {
	t.Setenv("ISSUE663_REVEAL_ENV", "dummy-env-value-not-a-secret")
	intPtr := func(v int) *int { return &v }
	pool := config.ProviderConfig{Credentials: []config.CredentialConfig{
		{Name: "env-entry", APIKeyEnv: "ISSUE663_REVEAL_ENV", Enabled: true},
		{Name: "empty-entry", Enabled: true},
		{Name: "lit-entry", APIKey: "test-literal-key", Enabled: true},
	}}
	if _, _, status, _ := revealable(pool, intPtr(9)); status != http.StatusNotFound {
		t.Fatalf("out-of-range index status=%d want 404", status)
	}
	if _, envName, status, _ := revealable(pool, intPtr(0)); status != http.StatusConflict || envName != "ISSUE663_REVEAL_ENV" {
		t.Fatalf("env pool entry status=%d env=%q want 409", status, envName)
	}
	if _, _, status, _ := revealable(pool, intPtr(1)); status != http.StatusNotFound {
		t.Fatalf("empty pool entry status=%d want 404", status)
	}
	if secret, _, status, _ := revealable(pool, intPtr(2)); status != 0 || secret != "test-literal-key" {
		t.Fatalf("literal pool entry status=%d secret=%q", status, secret)
	}
	if _, _, status, msg := revealable(pool, nil); status != http.StatusBadRequest || !strings.Contains(msg, "credential_index is required") {
		t.Fatalf("pool without index status=%d msg=%q", status, msg)
	}
	envOnly := config.ProviderConfig{APIKeyEnv: "ISSUE663_REVEAL_ENV"}
	if _, envName, status, _ := revealable(envOnly, nil); status != http.StatusConflict || envName != "ISSUE663_REVEAL_ENV" {
		t.Fatalf("env primary status=%d env=%q want 409", status, envName)
	}
	if _, _, status, _ := revealable(config.ProviderConfig{}, nil); status != http.StatusNotFound {
		t.Fatalf("keyless provider status=%d want 404", status)
	}
}

func TestIssue663HttpapiRevealLimiterFullTable(t *testing.T) {
	now := time.Now()
	// Build the table directly: 256 fresh windows must refuse a new subject.
	full := &revealLimiter{windows: make(map[string]*revealWindow, maxRevealRateSubjs)}
	for i := 0; i < maxRevealRateSubjs; i++ {
		full.windows[fmt.Sprintf("subj-%03d", i)] = &revealWindow{start: now, count: 1}
	}
	if full.allow("brand-new-subject", now) {
		t.Fatal("full limiter table must refuse a new subject")
	}
	if len(full.windows) != maxRevealRateSubjs {
		t.Fatalf("full table changed size: %d", len(full.windows))
	}

	mixed := &revealLimiter{windows: make(map[string]*revealWindow, maxRevealRateSubjs)}
	for i := 0; i < maxRevealRateSubjs-1; i++ {
		mixed.windows[fmt.Sprintf("fresh-%03d", i)] = &revealWindow{start: now, count: 1}
	}
	mixed.windows["stale-subject"] = &revealWindow{start: now.Add(-2 * revealRateWindow), count: 1}
	if !mixed.allow("brand-new-subject", now) {
		t.Fatal("stale window must be pruned to admit a new subject")
	}
	if _, ok := mixed.windows["stale-subject"]; ok {
		t.Fatal("stale window was not pruned")
	}

	limiter := &revealLimiter{}
	if !limiter.allow("", now) {
		t.Fatal("empty subject must map to unknown and be allowed")
	}
	if _, ok := limiter.windows["unknown"]; !ok {
		t.Fatal("empty subject was not mapped to unknown")
	}
	for i := 0; i < revealRateMax; i++ {
		if !limiter.allow("s", now) {
			t.Fatalf("allow %d for one subject must succeed", i)
		}
	}
	if limiter.allow("s", now) {
		t.Fatal("subject past the per-window cap must be refused")
	}
}

func TestIssue663HttpapiRevealSecretInvalidJSONAndAuditFailure(t *testing.T) {
	s := testGateway(t, revealTestConfig())
	rr := adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{bad`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid JSON") {
		t.Fatalf("malformed reveal body: %d %s", rr.Code, rr.Body.String())
	}

	s2 := testGateway(t, revealTestConfig())
	s2.securityErr = errors.New("audit store down")
	w := httptest.NewRecorder()
	s2.adminProviderRevealSecret(w, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/providers/p/reveal-secret", nil), "p")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "admin audit store unavailable") {
		t.Fatalf("reveal with failed audit: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), revealCanary) {
		t.Fatal("failed audit must not leak the secret")
	}
}

func TestIssue663HttpapiCompatMatrixAndSecuritySnapshot(t *testing.T) {
	s := testGateway(t, phaseHConfig(t.TempDir()+"/evaluation-state.json", ""))
	rr := issue663AdminRequest(s, http.MethodGet, "/admin/api/compat", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("compat status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := issue663Decode(t, rr)
	rows, ok := out["deployments"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("compat deployments missing: %v", out)
	}
	found := false
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && m["deployment"] == "p1/m1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("compat rows lack p1/m1: %v", rows)
	}

	rr = issue663AdminRequest(s, http.MethodGet, "/admin/api/security-snapshot", "")
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/octet-stream" || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("security snapshot: %d headers=%v body-len=%d", rr.Code, rr.Header(), rr.Body.Len())
	}
	if rr.Body.Len() == 0 {
		t.Fatal("security snapshot body is empty")
	}

	buf := &snapshotBoundedBuffer{limit: 5}
	if n, err := buf.Write([]byte("12345")); err != nil || n != 5 {
		t.Fatalf("write at the limit: n=%d err=%v", n, err)
	}
	if _, err := buf.Write([]byte("x")); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("write past the limit must fail: err=%v", err)
	}
}

type issue663FailingWriter struct {
	header   http.Header
	attempts int
}

func (w *issue663FailingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *issue663FailingWriter) WriteHeader(int) {}

func (w *issue663FailingWriter) Write([]byte) (int, error) {
	w.attempts++
	return 0, errors.New("client write failed")
}

type issue663SSEWriter struct {
	mu      sync.Mutex
	header  http.Header
	buf     strings.Builder
	status  int
	flushed bool
}

func (w *issue663SSEWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *issue663SSEWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = code
}

func (w *issue663SSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *issue663SSEWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushed = true
}

func (w *issue663SSEWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestIssue663HttpapiEventStreamFailures(t *testing.T) {
	s := testGateway(t, config.Default())

	rec405 := httptest.NewRecorder()
	s.adminEventStream(rec405, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/events/stream", nil))
	if rec405.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST event stream status=%d want 405 body=%s", rec405.Code, rec405.Body.String())
	}

	fw := &issue663FailingWriter{}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil)
	s.adminEventStream(fw, req)
	if fw.attempts == 0 {
		t.Fatal("failing writer was never written to")
	}

	// An event whose payload cannot be marshaled (NaN confidence) must
	// end the stream without emitting a frame for it.
	s.bus.Add(events.Event{Kind: "issue663", TaskConfidence: math.NaN()})
	rec := httptest.NewRecorder()
	s.adminEventStream(rec, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil))
	if !strings.Contains(rec.Body.String(), ": connected") {
		t.Fatalf("connected frame missing: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "event: event") {
		t.Fatalf("unmarshalable event was emitted: %q", rec.Body.String())
	}
}

type issue663FailAfterFirstWriter struct {
	issue663SSEWriter
	writes int
}

func (w *issue663FailAfterFirstWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("client write failed after connect")
	}
	return w.issue663SSEWriter.Write(p)
}

func TestIssue663HttpapiEventStreamKeepaliveWriteFailure(t *testing.T) {
	// The connected frame succeeds, then the 20s keepalive write fails:
	// the handler must return instead of looping on a dead client.
	s := testGateway(t, config.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil).WithContext(ctx)
	w := &issue663FailAfterFirstWriter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.adminEventStream(w, req)
	}()
	select {
	case <-done:
	case <-time.After(35 * time.Second):
		cancel()
		<-done
		t.Fatal("event stream did not return after failed keepalive write")
	}
	if w.writes < 2 {
		t.Fatalf("keepalive write never attempted: writes=%d", w.writes)
	}
	if !strings.Contains(w.String(), ": connected") || strings.Contains(w.String(), ": keepalive") {
		t.Fatalf("unexpected stream body: %q", w.String())
	}
}

func TestIssue663HttpapiEventStreamKeepalive(t *testing.T) {
	// The keepalive branch is driven by a hard-coded 20s ticker, so this
	// test genuinely waits for it; it polls the stream instead of
	// sleeping a fixed duration and asserting on timing.
	s := testGateway(t, config.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream", nil).WithContext(ctx)
	w := &issue663SSEWriter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.adminEventStream(w, req)
	}()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.String(), ": keepalive") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(w.String(), ": keepalive") {
		cancel()
		<-done
		t.Fatal("keepalive frame did not arrive within 35s")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream did not return after context cancel")
	}
	if !w.flushed {
		t.Fatal("keepalive path must flush")
	}
}
