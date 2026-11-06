package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// Agent D2 coverage, part 2: nexaflow Task 8 handlers, the connection
// test handler and count_tokens.

// d2FaultyNexaflowStore wraps the memory store with injectable
// failures, so handler error branches that need a failing store can
// be exercised without touching production code.
type d2FaultyNexaflowStore struct {
	*nexaflow.MemoryStore
	failListDrafts    bool
	failListRuns      bool
	failListRevisions bool
	failOnCreated     bool
	failAfterApproval bool
	mu                sync.Mutex
	approved          map[string]bool
}

func (s *d2FaultyNexaflowStore) ListRevisions(ctx context.Context, definitionID string) ([]nexaflow.PublishedDefinition, error) {
	if s.failListRevisions {
		return nil, errors.New("d2 injected list revisions failure")
	}
	return s.MemoryStore.ListRevisions(ctx, definitionID)
}

func (s *d2FaultyNexaflowStore) ListDrafts(ctx context.Context) ([]nexaflow.Definition, error) {
	if s.failListDrafts {
		return nil, errors.New("d2 injected list drafts failure")
	}
	return s.MemoryStore.ListDrafts(ctx)
}

func (s *d2FaultyNexaflowStore) ListRuns(ctx context.Context, filter nexaflow.RunFilter) ([]nexaflow.Run, error) {
	if s.failListRuns {
		return nil, errors.New("d2 injected list runs failure")
	}
	return s.MemoryStore.ListRuns(ctx, filter)
}

func (s *d2FaultyNexaflowStore) RecordApproval(ctx context.Context, appr nexaflow.Approval) (nexaflow.Approval, error) {
	out, err := s.MemoryStore.RecordApproval(ctx, appr)
	if err == nil {
		s.mu.Lock()
		if s.approved == nil {
			s.approved = map[string]bool{}
		}
		s.approved[appr.RunID] = true
		s.mu.Unlock()
	}
	return out, err
}

func (s *d2FaultyNexaflowStore) GetNodeExecutions(ctx context.Context, runID string) ([]nexaflow.NodeExecution, error) {
	if run, err := s.MemoryStore.GetRun(ctx, runID); err == nil {
		if s.failOnCreated && run.State == nexaflow.RunCreated {
			return nil, errors.New("d2 injected node executions failure")
		}
		if s.failAfterApproval {
			s.mu.Lock()
			approved := s.approved[runID]
			s.mu.Unlock()
			if approved && run.State == nexaflow.RunRunning {
				return nil, errors.New("d2 injected node executions failure")
			}
		}
	}
	return s.MemoryStore.GetNodeExecutions(ctx, runID)
}

func d2Task8Server(t *testing.T, store *d2FaultyNexaflowStore) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "task8-admin-secret"
	srv := testGateway(t, cfg)
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "d2-test"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := nexaflow.NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv.AttachNexaflow(svc)
	return srv, cfg.Admin.APIKey
}

func d2CreateAndPublishBranch(t *testing.T, srv *Server, key string) {
	t.Helper()
	def := task8BranchDef()
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create definition = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-branch/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestD2NexaflowTemplateInstantiateUnauthenticated(t *testing.T) {
	srv, _ := task8HTTPServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/nexaflow/templates/x/instantiate", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.2:1234"
	rec := httptest.NewRecorder()
	srv.adminNexaflowTemplateInstantiate(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated instantiate = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

// TestD2NexaflowDefinitionValidateUnauthenticated covers
// admin_nexaflow_studio.go:35-37: the validate handler's own auth check,
// which the HTTP middleware normally answers before the handler runs.
func TestD2NexaflowDefinitionValidateUnauthenticated(t *testing.T) {
	srv, _ := task8HTTPServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/nexaflow/definitions/validate", strings.NewReader(`{"definition":{}}`))
	req.RemoteAddr = "127.0.0.2:1234"
	rec := httptest.NewRecorder()
	srv.adminNexaflowDefinitionValidate(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated validate = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestD2NexaflowTemplateInstantiateBadShape(t *testing.T) {
	srv, key := task8HTTPServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/nexaflow/templates/a/b/instantiate", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("x-admin-key", key)
	rec := httptest.NewRecorder()
	srv.adminNexaflowTemplateInstantiate(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("three-segment instantiate path = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "template not found") {
		t.Fatalf("body must name the missing template: %s", rec.Body.String())
	}
}

func TestD2NexaflowTemplateByIDUnauthenticated(t *testing.T) {
	srv, _ := task8HTTPServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/nexaflow/templates/model-health-alert", nil)
	req.RemoteAddr = "127.0.0.2:1234"
	rec := httptest.NewRecorder()
	srv.adminNexaflowTemplateByID(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated template detail = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestD2NexaflowTemplateByIDEmptyID(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/templates/", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty template id = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "template not found") {
		t.Fatalf("body must name the missing template: %s", rec.Body.String())
	}
}

func TestD2NexaflowDefinitionsListStoreError(t *testing.T) {
	store := &d2FaultyNexaflowStore{MemoryStore: nexaflow.NewMemoryStore(), failListDrafts: true}
	srv, key := d2Task8Server(t, store)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list drafts store error = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "d2 injected list drafts failure") {
		t.Fatalf("body must surface the store error: %s", rec.Body.String())
	}
}

func TestD2NexaflowDefinitionValidateWrongMethod(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/anything/validate", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET validate = %d, want 405: %s", rec.Code, rec.Body.String())
	}
}

func TestD2NexaflowVersionsUnknownDefinition(t *testing.T) {
	store := &d2FaultyNexaflowStore{MemoryStore: nexaflow.NewMemoryStore(), failListRevisions: true}
	srv, key := d2Task8Server(t, store)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/nope/versions", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("versions store error = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "d2 injected list revisions failure") {
		t.Fatalf("body must surface the store error: %s", rec.Body.String())
	}
}

func TestD2NexaflowDefinitionRunsListStoreError(t *testing.T) {
	store := &d2FaultyNexaflowStore{MemoryStore: nexaflow.NewMemoryStore(), failListRuns: true}
	srv, key := d2Task8Server(t, store)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/whatever/runs", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("definition runs store error = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "d2 injected list runs failure") {
		t.Fatalf("body must surface the store error: %s", rec.Body.String())
	}
}

func TestD2NexaflowDefinitionRunsWrongMethod(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodPut, "/admin/api/nexaflow/definitions/anything/runs", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT runs = %d, want 405: %s", rec.Code, rec.Body.String())
	}
}

func TestD2NexaflowStartRunExecuteStoreError(t *testing.T) {
	store := &d2FaultyNexaflowStore{MemoryStore: nexaflow.NewMemoryStore(), failOnCreated: true}
	srv, key := d2Task8Server(t, store)
	d2CreateAndPublishBranch(t, srv, key)
	runBody := map[string]any{"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "idempotency_key": "d2-exec-fail"}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-branch/runs", runBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("run with failing execute = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "d2 injected node executions failure") {
		t.Fatalf("body must surface the execute error: %s", rec.Body.String())
	}
}

func TestD2NexaflowRunsListStoreError(t *testing.T) {
	store := &d2FaultyNexaflowStore{MemoryStore: nexaflow.NewMemoryStore(), failListRuns: true}
	srv, key := d2Task8Server(t, store)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("runs list store error = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "d2 injected list runs failure") {
		t.Fatalf("body must surface the store error: %s", rec.Body.String())
	}
}

func TestD2NexaflowApproveExecuteStoreError(t *testing.T) {
	store := &d2FaultyNexaflowStore{MemoryStore: nexaflow.NewMemoryStore(), failAfterApproval: true}
	srv, key := d2Task8Server(t, store)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "d2-appr", Name: "d2-appr",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "appr", NodeID: "nexaroute.control.approval", Version: nexaflow.SupportedVersion},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "appr"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/d2-appr/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/d2-appr/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{}}, "idempotency_key": "d2-appr-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}
	var runResp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runResp); err != nil {
		t.Fatalf("run decode: %v", err)
	}
	if runResp.State != "waiting_for_approval" {
		t.Fatalf("run state = %q, want waiting_for_approval", runResp.State)
	}
	// A different principal approves: the OIDC session subject
	// (user-123) is not the admin-key run creator, so separation of
	// duties passes and the handler reaches ExecuteRun.
	cookieValue := installTestOIDCSession(t, srv, authz.RoleAdmin)
	sessionCookie := &http.Cookie{Name: adminSessionCookie, Value: cookieValue}
	csrfReq := httptest.NewRequest(http.MethodGet, "http://localhost/admin/api/csrf-token", nil)
	csrfReq.RemoteAddr = "127.0.0.1:1234"
	csrfReq.AddCookie(sessionCookie)
	csrfRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusOK {
		t.Fatalf("csrf-token = %d: %s", csrfRec.Code, csrfRec.Body.String())
	}
	var csrfBody struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(csrfRec.Body.Bytes(), &csrfBody); err != nil || len(csrfBody.Token) != 43 {
		t.Fatalf("invalid CSRF response: %s err=%v", csrfRec.Body.String(), err)
	}
	var csrfCookie *http.Cookie
	for _, c := range csrfRec.Result().Cookies() {
		if c.Name == adminCSRFCookie {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("csrf-token response set no CSRF cookie")
	}
	approveReq := httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/nexaflow/runs/"+runResp.ID+"/approve",
		strings.NewReader(`{"node_id":"appr","decision":"approved"}`))
	approveReq.RemoteAddr = "127.0.0.1:1234"
	approveReq.Header.Set("Content-Type", "application/json")
	approveReq.Header.Set("Origin", "http://localhost")
	approveReq.Header.Set("X-NexaRoute-CSRF", csrfBody.Token)
	approveReq.AddCookie(sessionCookie)
	approveReq.AddCookie(csrfCookie)
	approveRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(approveRec, approveReq)
	if approveRec.Code != http.StatusBadRequest {
		t.Fatalf("approve with failing execute = %d, want 400: %s", approveRec.Code, approveRec.Body.String())
	}
	if !strings.Contains(approveRec.Body.String(), "d2 injected node executions failure") {
		t.Fatalf("body must surface the execute error: %s", approveRec.Body.String())
	}
}

func TestD2NexaflowWebhookStartNotFound(t *testing.T) {
	srv, _ := task8HTTPServer(t)
	svc, err := nexaflow.NewWebhookService(nexaflow.NewWebhookVerifier(), nexaflow.NewMemoryDeliveryLedger(),
		func(ctx context.Context, defID, trigger string, body []byte) (string, error) {
			return "", nexaflow.ErrNotFound
		})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	srv.AttachNexaflowWebhook(svc, map[string]string{"wf-1": "hook-secret"})
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := `{"event":"ping"}`
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	mac.Write([]byte(ts + "." + body))
	sig := hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-1/hook", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Nexaflow-Timestamp", ts)
	req.Header.Set("X-Nexaflow-Signature", sig)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("webhook with unknown start = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestD2ConnectionTestDialectDefaultsToAnthropic(t *testing.T) {
	s := &Server{cfg: config.Config{Providers: []config.ProviderConfig{
		{ID: "p", Type: "anthropic_compatible", BaseURL: "http://127.0.0.1:1", Models: []config.ModelConfig{{ID: "m", Model: "x"}}},
	}}}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/connection-test", strings.NewReader(`{"deployment_id":"p/m","dialect":"weird"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.adminConnectionTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("connection test = %d: %s", rec.Code, rec.Body.String())
	}
	var rep connectionTestReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report decode: %v", err)
	}
	if rep.Dialect != "anthropic" {
		t.Fatalf("dialect = %q, want anthropic for an anthropic_compatible provider", rep.Dialect)
	}
}

func TestD2ConnectionTestCredentialIndexOutOfRange(t *testing.T) {
	prov := config.ProviderConfig{ID: "fake", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", APIKey: "k"}
	idx := 3
	rep := runConnectionTestWithCredential(context.Background(), prov, config.ModelConfig{ID: "m", Model: "x"}, "fake/m", "openai", &idx)
	if rep.Error == "" || !strings.Contains(rep.Error, "out of range") {
		t.Fatalf("report = %+v, want an out-of-range credential error", rep)
	}
	if rep.OK {
		t.Fatalf("out-of-range credential must not report ok: %+v", rep)
	}
}

func TestD2ConnectionTestAdapterError(t *testing.T) {
	prov := config.ProviderConfig{ID: "fake", Type: "bogus", BaseURL: "http://127.0.0.1:1", APIKey: "k"}
	rep := runConnectionTestWithCredential(context.Background(), prov, config.ModelConfig{ID: "m", Model: "x"}, "fake/m", "openai", nil)
	if rep.Error == "" || !strings.Contains(rep.Error, "adapter:") {
		t.Fatalf("report = %+v, want an adapter error", rep)
	}
	if rep.OK {
		t.Fatalf("adapter error must not report ok: %+v", rep)
	}
}

func TestD2ParseConnectionTestResponseShapes(t *testing.T) {
	if ok, in, out := parseConnectionTestResponse("anthropic", []byte(`{"type":"error"}`)); ok || in != nil || out != nil {
		t.Fatalf("anthropic non-message = (%v,%v,%v), want (false,nil,nil)", ok, in, out)
	}
	if ok, _, _ := parseConnectionTestResponse("anthropic", []byte(`{bad`)); ok {
		t.Fatal("anthropic malformed JSON must not match")
	}
	ok, in, out := parseConnectionTestResponse("anthropic", []byte(`{"type":"message"}`))
	if !ok || in != nil || out != nil {
		t.Fatalf("anthropic message without usage = (%v,%v,%v), want (true,nil,nil)", ok, in, out)
	}
	ok, in, out = parseConnectionTestResponse("anthropic", []byte(`{"type":"message","usage":{"input_tokens":3,"output_tokens":4}}`))
	if !ok || in == nil || *in != 3 || out == nil || *out != 4 {
		t.Fatalf("anthropic message with usage = (%v,%v,%v), want (true,3,4)", ok, in, out)
	}
	ok, in, out = parseConnectionTestResponse("openai", []byte(`{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	if !ok || in != nil || out != nil {
		t.Fatalf("openai choices without usage = (%v,%v,%v), want (true,nil,nil)", ok, in, out)
	}
	ok, in, out = parseConnectionTestResponse("openai", []byte(`{"choices":[{}],"usage":{"prompt_tokens":6,"completion_tokens":1}}`))
	if !ok || in == nil || *in != 6 || out == nil || *out != 1 {
		t.Fatalf("openai choices with usage = (%v,%v,%v), want (true,6,1)", ok, in, out)
	}
}

func TestD2CountTokensUnauthenticated(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("count_tokens without a client key = %d, want 401: %s", rr.Code, rr.Body.String())
	}
}

func TestD2CountTokensStaleCandidateSkipped(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		CountTokensPath: "/v1/messages/count_tokens",
		Models:          []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	// The router still knows the deployment, but the adapter registry
	// no longer has it: the candidate must be skipped, never called.
	empty, err := providers.NewRegistry(config.Config{})
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	s.reg.Replace(empty)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("count_tokens = %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["estimated"] != true {
		t.Fatalf("stale candidate must fall back to the local estimate: %#v", got)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0 (stale candidate never called)", calls)
	}
}

func TestD2CountTokensInvalidUpstreamModelFallsBack(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		CountTokensPath: "/v1/messages/count_tokens",
		// An upstream model id that is not valid UTF-8 cannot be
		// patched into the native payload: the candidate is skipped.
		Models: []config.ModelConfig{{ID: "m", Model: "bad\xffmodel", Aliases: []string{"client"}, Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages/count_tokens",
		strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("count_tokens = %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["estimated"] != true {
		t.Fatalf("unpatchable model must fall back to the local estimate: %#v", got)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0 (unpatchable candidate never called)", calls)
	}
}
