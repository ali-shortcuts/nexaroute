package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Agent D2 coverage: httpapi admin/auth/config files.
// Unreachable lines (verified, see final report) are NOT covered here:
//   - admin.go:1308-1309 weight default: dead — ProviderConfig.ApplyDefaults
//     (config.go:1980-1981) already sets Weight 0→1, and normalizeProvider
//     calls ApplyDefaults first, so Weight is never 0 at the check.
//   - admin_auth.go randomURLToken error branches (crypto/rand failure)
//   - clientauth.go newVirtualClientKey rand failure, non-string JSON key
//     (encoding/json Token() at a key position returns only a string or
//     an error — verified live), dead truncated-body branch (early
//     return), ceil clamp, legacy-identity fallback
//   - client_keys.go newVirtualClientKey rand failure branches
//   - conformance_snapshot.go missing-dimension continue (every manifest
//     entry declares all 16 dimensions)
//   - count_tokens.go tokens<1 clamp (readJSON never yields an empty body)

// d2HealthDecisionProvider reports a fixed degraded health message so
// the admin snapshot's health_message surfacing can be asserted.
type d2HealthDecisionProvider struct {
	id      string
	message string
}

func (p d2HealthDecisionProvider) ID() string { return p.id }
func (p d2HealthDecisionProvider) Capabilities() decision.Capabilities {
	return decision.Capabilities{CanSelect: true}
}
func (p d2HealthDecisionProvider) Health() decision.ProviderHealth {
	return decision.ProviderHealth{Status: decision.HealthDegraded, Message: p.message, CheckedAt: time.Now()}
}
func (p d2HealthDecisionProvider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	return decision.DecisionResult{}, nil
}

func TestD2AdminSnapshotExternalProviderHealthMessage(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "test-key", PrivacyMode: "metadata_only", BaseURL: "https://jev.example.invalid"},
	}
	s := testGateway(t, cfg)
	s.decisionRegistry.Register(d2HealthDecisionProvider{id: "jev-main", message: "upstream slow, retrying"})
	rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Decision struct {
			External []struct {
				ID            string `json:"id"`
				Health        string `json:"health"`
				HealthMessage string `json:"health_message"`
				KeyConfigured bool   `json:"key_configured"`
			} `json:"external_decision_providers"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	for _, p := range body.Decision.External {
		if p.ID != "jev-main" {
			continue
		}
		if p.Health != "degraded" || p.HealthMessage != "upstream slow, retrying" || !p.KeyConfigured {
			t.Fatalf("external provider = %+v, want degraded + health_message + key_configured", p)
		}
		return
	}
	t.Fatalf("jev-main missing from external_decision_providers: %s", rr.Body.String())
}

func TestD2AdminProviderDetailLastCredentialSelection(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, Enabled: true,
		Credentials: []config.CredentialConfig{
			{Name: "key-a", APIKey: "key-a", Enabled: true},
			{Name: "key-b", APIKey: "key-b", Enabled: true},
		},
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer key-a" && gotAuth != "Bearer key-b" {
		t.Fatalf("upstream auth = %q, want one of the two credentials", gotAuth)
	}
	detail := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	if detail.Code != http.StatusOK {
		t.Fatalf("provider detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	body := coverageAdminJSON(t, detail)
	states, ok := body["credential_states"].([]any)
	if !ok || len(states) != 2 {
		t.Fatalf("credential_states = %v, want 2 entries", body["credential_states"])
	}
	sel, ok := body["last_credential_selection"].(map[string]any)
	if !ok {
		t.Fatalf("last_credential_selection missing: %s", detail.Body.String())
	}
	idx, _ := sel["index"].(float64)
	if idx != 0 && idx != 1 {
		t.Fatalf("last_credential_selection index = %v, want 0 or 1", sel["index"])
	}
	if sel["name"] != "key-a" && sel["name"] != "key-b" {
		t.Fatalf("last_credential_selection name = %v", sel["name"])
	}
	if sel["reason"] == "" {
		t.Fatalf("last_credential_selection lacks a reason: %v", sel)
	}
}

func TestD2NormalizeProviderDefaultsZeroWeight(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "existing", Name: "Existing", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: false,
		Models: []config.ModelConfig{{ID: "existing-model", Model: "existing-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	put := coverageAdminRequest(s, http.MethodPut, "/admin/api/providers/existing",
		`{"provider":{"id":"existing","name":"Existing","type":"openai_compatible","base_url":"http://127.0.0.1:9","auth_mode":"none","enabled":false,"models":[{"model":"alpha","enabled":true,"weight":0}]}}`)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT provider status=%d body=%s", put.Code, put.Body.String())
	}
	get := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/existing", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET provider status=%d body=%s", get.Code, get.Body.String())
	}
	body := coverageAdminJSON(t, get)
	prov, ok := body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider missing: %s", get.Body.String())
	}
	models, ok := prov["models"].([]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %v, want 1", prov["models"])
	}
	m, _ := models[0].(map[string]any)
	if m["weight"] != float64(1) {
		t.Fatalf("model weight = %v, want 1 (zero weight must default)", m["weight"])
	}
	if m["id"] != "alpha" {
		t.Fatalf("model id = %v, want alpha (slug default)", m["id"])
	}
}

func TestD2AdminSecurityStorePathRequiresPath(t *testing.T) {
	if _, err := adminSecurityStorePath("", ""); err == nil {
		t.Fatal("adminSecurityStorePath(\"\", \"\") must fail")
	} else if !strings.Contains(err.Error(), "required") {
		t.Fatalf("error = %q, want a 'required' complaint", err.Error())
	}
}

// TestD2AdminSecurityStorePathDeletedCWD covers admin_auth.go:47-48:
// filepath.Abs fails when the process working directory was deleted.
func TestD2AdminSecurityStorePathDeletedCWD(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp, err := os.MkdirTemp("", "d2-deleted-cwd-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatalf("remove cwd: %v", err)
	}
	_, err = adminSecurityStorePath("security/nexaroute-security.db", "config.json")
	if err == nil || !strings.Contains(err.Error(), "resolve admin security store directory") {
		t.Fatalf("deleted-cwd path = %v, want resolve error", err)
	}
}

func TestD2AdminEvalLiveDuplicatePromptRejected(t *testing.T) {
	cfg := phaseHConfig(t.TempDir()+"/state.json", "")
	cfg.Evaluation.LiveEnabled = true
	s := testGateway(t, cfg)
	body := `{"mode":"live","suite_id":"reasoning","deployment_id":"p1/m1","prompts":[
		{"case_id":"reasoning-multi-step-arithmetic","prompt":"first"},
		{"case_id":"reasoning-multi-step-arithmetic","prompt":"second"}]}`
	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", body)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("duplicate live prompts status=%d, want 400: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "duplicate live prompt") {
		t.Fatalf("body lacks duplicate-prompt error: %s", res.Body.String())
	}
}

// d2RegistryWithFakeAdapter builds an otherwise empty provider registry
// whose map holds one non-*httpAdapter adapter. providers.Registry's map
// is unexported and NewRegistry only ever creates *httpAdapter entries, so
// the entry is injected with reflect+unsafe — a test-only seam (the same
// unsafe pattern is used by other *_test.go files in this repo).
// It exists for admin_eval.go:418-421: EvaluationTwin rejects every adapter
// that is not *httpAdapter, and no production config can produce one.
func d2RegistryWithFakeAdapter(t *testing.T, id string, adapter providers.Adapter) *providers.Registry {
	t.Helper()
	emptyCfg := config.Default()
	emptyCfg.ApplyDefaults()
	reg, err := providers.NewRegistry(emptyCfg)
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	field := reflect.ValueOf(reg).Elem().FieldByName("m")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(
		reflect.ValueOf(map[string]providers.Adapter{id: adapter}))
	return reg
}

// TestD2AdminEvalLiveNonTwinAdapterRejected covers admin_eval.go:418-421:
// a live run whose deployment resolves to a non-*httpAdapter adapter is
// rejected with 409 because no evaluation twin can be built for it.
func TestD2AdminEvalLiveNonTwinAdapterRejected(t *testing.T) {
	cfg := phaseHConfig(t.TempDir()+"/state.json", "")
	cfg.Evaluation.LiveEnabled = true
	s := testGateway(t, cfg)
	s.runtimeMu.Lock()
	s.reg = d2RegistryWithFakeAdapter(t, "p1", &Continue95HttpapiFakeAdapter{})
	s.runtimeMu.Unlock()
	body := `{"mode":"live","suite_id":"reasoning","deployment_id":"p1/m1","prompts":[{"case_id":"reasoning-multi-step-arithmetic","prompt":"first"}]}`
	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", body)
	if res.Code != http.StatusConflict {
		t.Fatalf("non-twin live run status=%d, want 409: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "does not support isolated live evaluation") {
		t.Fatalf("body lacks isolated-evaluation error: %s", res.Body.String())
	}
}

func TestD2NexaflowStudioValidateDraftIdentity(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/validate", map[string]any{"definition": map[string]any{}})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate draft status=%d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("validate decode: %v", err)
	}
	if out.Valid {
		t.Fatalf("empty draft must not validate: %s", rec.Body.String())
	}
	if len(out.Errors) == 0 {
		t.Fatalf("invalid draft must carry errors: %s", rec.Body.String())
	}
	list := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions", nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "validate-draft") {
		t.Fatalf("validate must not store the draft: %d %s", list.Code, list.Body.String())
	}
}

// d2VerifyErrorStore fails audit-chain verification outright.
type d2VerifyErrorStore struct{ *securitystore.Store }

func (d2VerifyErrorStore) VerifyAuditChain(context.Context) (securitystore.ChainReport, error) {
	return securitystore.ChainReport{}, errors.New("d2 injected verify failure")
}

// d2UnverifiedNoProblemsStore reports a broken chain with no detail.
type d2UnverifiedNoProblemsStore struct{ *securitystore.Store }

func (d2UnverifiedNoProblemsStore) VerifyAuditChain(context.Context) (securitystore.ChainReport, error) {
	return securitystore.ChainReport{Verified: false}, nil
}

// d2BackupFailStore verifies fine but cannot produce a snapshot.
type d2BackupFailStore struct{ *securitystore.Store }

func (d2BackupFailStore) BackupTo(context.Context, io.Writer) error {
	return errors.New("d2 injected backup failure")
}

func TestD2AdminSecuritySnapshotVerifyError(t *testing.T) {
	srv := testGateway(t, config.Default())
	orig, ok := srv.securityDB.(*securitystore.Store)
	if !ok {
		t.Fatalf("test gateway store is %T, want *securitystore.Store", srv.securityDB)
	}
	srv.securityDB = d2VerifyErrorStore{Store: orig}
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("verify-error snapshot status=%d, want 503: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "audit chain verification unavailable") {
		t.Fatalf("body must name the unavailable verification: %s", w.Body.String())
	}
}

// TestD2AdminSecuritySnapshotStoreUnavailable covers
// admin_security_snapshot.go:68-70: no security store at all.
func TestD2AdminSecuritySnapshotStoreUnavailable(t *testing.T) {
	srv := testGateway(t, config.Default())
	srv.securityDB = nil
	srv.securityErr = nil
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("store-unavailable snapshot status=%d, want 503: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin security store unavailable") {
		t.Fatalf("body must name the unavailable store: %s", w.Body.String())
	}
}

func TestD2AdminSecuritySnapshotUnverifiedWithoutProblems(t *testing.T) {
	srv := testGateway(t, config.Default())
	orig, ok := srv.securityDB.(*securitystore.Store)
	if !ok {
		t.Fatalf("test gateway store is %T, want *securitystore.Store", srv.securityDB)
	}
	srv.securityDB = d2UnverifiedNoProblemsStore{Store: orig}
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unverified snapshot status=%d, want 503: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown problem") {
		t.Fatalf("problem-less refusal must fall back to 'unknown problem': %s", w.Body.String())
	}
}

func TestD2AdminSecuritySnapshotBackupError(t *testing.T) {
	srv := testGateway(t, config.Default())
	if err := srv.securityDB.AppendAudit(context.Background(), securitystore.AuditEvent{
		Timestamp: time.Now().UTC(), Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}
	orig, ok := srv.securityDB.(*securitystore.Store)
	if !ok {
		t.Fatalf("test gateway store is %T, want *securitystore.Store", srv.securityDB)
	}
	srv.securityDB = d2BackupFailStore{Store: orig}
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("backup-error snapshot status=%d, want 503: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "security snapshot failed") {
		t.Fatalf("body must name the failed snapshot: %s", w.Body.String())
	}
}

func TestD2AdminClientKeyByIDRequiresID(t *testing.T) {
	s := testGateway(t, config.Default())
	rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/client-keys/", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty client-key id status=%d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "client key id is required") {
		t.Fatalf("body must name the missing id: %s", rr.Body.String())
	}
}

func TestD2RequestTopLevelValuesTruncatedObject(t *testing.T) {
	if found, ok := requestTopLevelValues([]byte(`{"model":"x"`), "model"); ok || found != nil {
		t.Fatalf("truncated object = (%v, %v), want (nil, false)", found, ok)
	}
	// A "{" delim where a key is expected is rejected by encoding/json
	// itself ("object member name must be a string"), so Token() at a key
	// position returns only a string or an error — the non-string-token
	// branch at clientauth.go:188-189 is unreachable (verified live).
	// The input still exercises the loop's error exit (line 183-185).
	if found, ok := requestTopLevelValues([]byte(`{{"a":1}}`), "model"); ok || found != nil {
		t.Fatalf("nested-object key = (%v, %v), want (nil, false)", found, ok)
	}
	// Loop exits normally (More() sees ']'), then the closing token is not
	// '}' (clientauth.go:200-201).
	if found, ok := requestTopLevelValues([]byte(`{"model":"x"]`), "model"); ok || found != nil {
		t.Fatalf("bracket-closed object = (%v, %v), want (nil, false)", found, ok)
	}
	found, ok := requestTopLevelValues([]byte(`{"model":"x"}`), "model")
	if !ok || len(found) != 1 || string(found[0]) != `"x"` {
		t.Fatalf("complete object = (%v, %v), want ([\"x\"], true)", found, ok)
	}
}

func TestD2ClientRPMBucketRefillClampsAtCapacity(t *testing.T) {
	s := &Server{}
	const rpm = 600
	if !s.clientRPMAllow("d2-rpm-key", rpm) {
		t.Fatal("first RPM allow must pass")
	}
	time.Sleep(300 * time.Millisecond)
	if !s.clientRPMAllow("d2-rpm-key", rpm) {
		t.Fatal("second RPM allow must pass after refill")
	}
	b := s.clientBuckets[keyDigest("d2-rpm-key")]
	if b == nil {
		t.Fatal("RPM bucket missing")
	}
	// 599 tokens remained after the first call; 300ms of refill at
	// 10/s adds 3, which must clamp to the 600 capacity (not 602)
	// before the second call spends one.
	if b.tokens != 599 {
		t.Fatalf("bucket tokens = %v, want exactly 599 (refill clamped at capacity)", b.tokens)
	}
}

func TestD2ClientTPMReserveRefillClampsAtCapacity(t *testing.T) {
	s := &Server{}
	now := time.Now()
	if _, ok, _ := s.clientTPMReserveAt("d2-tpm-key", 60, 1, now); !ok {
		t.Fatal("first TPM reserve must pass")
	}
	res, ok, _ := s.clientTPMReserveAt("d2-tpm-key", 60, 1, now.Add(120*time.Second))
	if !ok || res == nil {
		t.Fatal("second TPM reserve must pass after refill")
	}
	if res.charged != 1 {
		t.Fatalf("charged = %d, want 1", res.charged)
	}
	b := s.clientBuckets[keyDigest("d2-tpm-key")+"#tpm"]
	if b == nil {
		t.Fatal("TPM bucket missing")
	}
	// 59 tokens remained after the first charge; 120s of refill at
	// 1/s adds 120, which must clamp to the 60 capacity (not 179)
	// before the second charge spends one.
	if b.tokens != 59 {
		t.Fatalf("bucket tokens = %v, want exactly 59 (refill clamped at capacity)", b.tokens)
	}
}

func TestD2CodingProfilesCoverage(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{ID: "p1", Models: []config.ModelConfig{
				{ID: "m-capable", ContextWindow: 200000, Capabilities: config.Capabilities{Tools: true, Vision: true, Reasoning: true}},
				{ID: "m-plain", ContextWindow: 8000},
			}},
		},
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "codepool", Deployments: []string{"p1/m-capable"}},
			{ID: "longpool", Deployments: []string{"p1/m-capable", "p1/m-plain"}},
			{ID: "reasonpool", Deployments: []string{"p1/m-capable"}},
			{ID: "visionpool", Deployments: []string{"p1/m-capable"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "rp1", TaskPools: map[string]string{
				"coding": "missing-pool", "background": "codepool",
				"long_context": "longpool", "deep_reasoning": "reasonpool", "vision": "visionpool",
			}},
		},
	}
	snap := codingProfilesSnapshot(cfg)
	classes, _ := snap["classes"].([]map[string]any)
	byID := map[string]map[string]any{}
	for _, c := range classes {
		byID[c["id"].(string)] = c
	}
	mapping := func(classID string) map[string]any {
		t.Helper()
		ms, _ := byID[classID]["mappings"].([]map[string]any)
		if len(ms) != 1 {
			t.Fatalf("%s mappings = %v, want exactly 1", classID, byID[classID]["mappings"])
		}
		return ms[0]
	}
	// Unknown pool: nil members become an empty list and the
	// verdict is unmet (capabilitySatisfied is never consulted).
	coding := mapping("primary_coding")
	if coding["capability"] != "unmet" {
		t.Fatalf("primary_coding capability = %v, want unmet (missing pool)", coding["capability"])
	}
	if members, _ := coding["pool_members"].([]string); len(members) != 0 {
		t.Fatalf("primary_coding pool_members = %v, want empty", coding["pool_members"])
	}
	if coding["mode"] != "off" {
		t.Fatalf("primary_coding mode = %v, want off (empty TaskIntelMode)", coding["mode"])
	}
	// Requirement "none" is satisfied by any member and always met.
	bg := mapping("background")
	if bg["capability"] != "met" || bg["capable_members"] != 1 {
		t.Fatalf("background mapping = %v, want met with 1 capable member", bg)
	}
	// Vision and reasoning requirements read the declared flags.
	visual := mapping("visual")
	if visual["capability"] != "met" || visual["capable_members"] != 1 {
		t.Fatalf("visual mapping = %v, want met with 1 capable member", visual)
	}
	reasoning := mapping("reasoning")
	if reasoning["capability"] != "met" || reasoning["capable_members"] != 1 {
		t.Fatalf("reasoning mapping = %v, want met with 1 capable member", reasoning)
	}
	lm := mapping("long_context")
	if lm["capability"] != "partial" || lm["capable_members"] != 1 {
		t.Fatalf("long_context mapping = %v, want partial with 1 capable member", lm)
	}
}

func TestD2CompatEngineEncodeFailureEmitsBusEvent(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "oc", Type: "opencode-console", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true}},
	}}
	s := &Server{cfg: cfg, bus: events.New(100)}
	bundle := hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "oc/m", ProviderID: "oc", Model: "m"}}}
	// A canonical request with no messages cannot be encoded for the
	// OpenAI chat shape: the attempt must fail closed, unbuildable.
	if _, ok := s.finishCanonicalAttempt(bundle, canonical.Request{Model: "m"}, "openai_chat"); ok {
		t.Fatal("finishCanonicalAttempt with no messages must fail")
	}
	found := false
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "encode_fail" && e.Deployment == "oc/m" && e.ErrorType == "canonical_encode_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no encode_fail bus event for oc/m: %+v", s.bus.Snapshot())
	}
}

func TestD2ConfigHistoryRollbackUnavailable(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "test-admin"
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config/history", strings.NewReader(`{"steps":1}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "test-admin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("rollback with no history status=%d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "requested rollback is not available") {
		t.Fatalf("body must name the unavailable rollback: %s", w.Body.String())
	}
}

func TestD2ConfigHistoryRollbackApplyFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "test-admin"
	s := testGateway(t, cfg)
	if _, err := s.mutateConfig(func(c *config.Config) error { c.Routing.PublicModel = "first"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Rollback re-applies the stored config through ApplyEnvOverrides,
	// which rejects a non-boolean NEXAROUTE_ADMIN_BIND_LOCAL_ONLY.
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "not-a-bool")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config/history", strings.NewReader(`{"steps":1}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "test-admin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("rollback with bad env override status=%d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NEXAROUTE_ADMIN_BIND_LOCAL_ONLY") {
		t.Fatalf("body must name the bad env override: %s", w.Body.String())
	}
}

func TestD2ConfigSnapshotConcurrentReaders(t *testing.T) {
	s := testGateway(t, config.Default())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if snap := s.configSnapshot(); snap == nil {
					t.Error("configSnapshot returned nil under concurrency")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestD2ConfigSnapshotCASLoss covers config_snapshot.go:56: the
// CompareAndSwap loser path. A fresh &Server{} per attempt has no published
// snapshot, so barrier-released goroutines all pass the nil Load and build
// a snapshot; all but the first CAS lose and fall through to the reload.
func TestD2ConfigSnapshotCASLoss(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		s := &Server{}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if snap := s.configSnapshot(); snap == nil {
					t.Error("configSnapshot returned nil in CAS race")
				}
			}()
		}
		close(start)
		wg.Wait()
	}
}

// TestD2ConformanceSnapshotDimsMatchManifest pins the snapshot shape:
// every manifest entry's declared dimensions are listed, nothing more.
// No manifest entry omits a dimension today, so the snapshot's
// missing-dimension skip is defensive; this test locks the declared
// set so a future partial entry is a deliberate manifest change.
func TestD2ConformanceSnapshotDimsMatchManifest(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	snap := conformanceSnapshot(now)
	entries, _ := snap["entries"].([]map[string]any)
	if len(entries) != len(providers.Manifest()) {
		t.Fatalf("entries = %d, want %d", len(entries), len(providers.Manifest()))
	}
	declared := map[string]map[string]bool{}
	for _, e := range providers.Manifest() {
		set := map[string]bool{}
		for d := range e.Dimensions {
			set[string(d)] = true
		}
		declared[e.Dialect] = set
	}
	for _, e := range entries {
		dialect, _ := e["dialect"].(string)
		dims, _ := e["dimensions"].([]map[string]any)
		if len(dims) != len(declared[dialect]) {
			t.Fatalf("%s dims = %d, want %d (declared)", dialect, len(dims), len(declared[dialect]))
		}
		for _, d := range dims {
			if !declared[dialect][d["dimension"].(string)] {
				t.Fatalf("%s lists undeclared dimension %v", dialect, d["dimension"])
			}
		}
	}
}
