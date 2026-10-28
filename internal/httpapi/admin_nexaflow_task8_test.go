package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Task 8 HTTP acceptance: the real admin routes over the canonical
// service, plus the disabled-by-default behaviour (scenario 1/12/15
// at the HTTP layer; engine-level scenarios live in internal/nexaflow).

// task8AttachProbeService attaches a canonical NexaFlow service
// (memory store, no backend) to a probe server, mirroring the video
// precedent in probeWebAPICall: the probe proves the route exists
// exactly as production wiring does when the feature is enabled.
// Pure-node workflows execute without a backend; backend nodes are
// not exercised by the contract probes.
func task8AttachProbeService(t *testing.T, srv *Server) {
	t.Helper()
	store := nexaflow.NewMemoryStore()
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "probe"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := nexaflow.NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv.AttachNexaflow(svc)
}

func task8HTTPServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "task8-admin-secret"
	srv := testGateway(t, cfg)
	store := nexaflow.NewMemoryStore()
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "http-test"})
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

func task8Do(t *testing.T, srv *Server, key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "http://gateway"+path, reader)
	// httptest.NewRequest defaults to a non-loopback RemoteAddr, which
	// admin auth (BindLocalOnly) rejects before the key is checked.
	// Unauthenticated attempts cost the whole admin bucket for their
	// address, so they use a distinct loopback address (the same
	// pattern as TestAdminEventStreamRequiresAuthAndStreamsSnapshotAndLive).
	if key == "" {
		req.RemoteAddr = "127.0.0.2:1234"
	} else {
		req.RemoteAddr = "127.0.0.1:1234"
	}
	if key != "" {
		req.Header.Set("x-admin-key", key)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func task8BranchDef() nexaflow.Definition {
	node := func(id, nodeID string, cfg, input map[string]any) nexaflow.DefinitionNode {
		return nexaflow.DefinitionNode{ID: id, NodeID: nodeID, Version: nexaflow.SupportedVersion, Config: cfg, Input: input}
	}
	leaf := func(id, lane string) nexaflow.DefinitionNode {
		return node(id, "nexaroute.control.json_transform", nil, map[string]any{
			"value":      map[string]any{"lane": lane},
			"operations": []any{map[string]any{"op": "set_const", "field": "done", "value": true}},
		})
	}
	return nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "http-branch", Name: "http-branch",
		Nodes: []nexaflow.DefinitionNode{
			node("start", "nexaroute.trigger.manual", nil, nil),
			node("chk", "nexaroute.control.condition", map[string]any{"field": "health.status", "operator": "eq", "expected": "ok"}, map[string]any{"value": "$.input.payload"}),
			node("br", "nexaroute.control.branch", map[string]any{"true_branch": "fast", "false_branch": "slow"}, map[string]any{"condition_result": "$.chk.result"}),
			leaf("fast", "fast"),
			leaf("slow", "slow"),
		},
		Edges: []nexaflow.DefinitionEdge{
			{ID: "e1", From: "start", To: "chk"},
			{ID: "e2", From: "chk", To: "br"},
			{ID: "e3", From: "br", To: "fast", Branch: "fast"},
			{ID: "e4", From: "br", To: "slow", Branch: "slow"},
		},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
}

func TestTask8HTTPNodesTemplatesAuth(t *testing.T) {
	srv, key := task8HTTPServer(t)
	// Unauthenticated: 401.
	if rec := task8Do(t, srv, "", http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated nodes = %d, want 401", rec.Code)
	}
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/nodes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nexaroute.control.branch") {
		t.Fatalf("nodes body lacks branch node: %s", rec.Body.String())
	}
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/templates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("templates = %d: %s", rec.Code, rec.Body.String())
	}
	var tplBody struct {
		Templates []struct {
			ID        string `json:"id"`
			Published bool   `json:"published"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tplBody); err != nil {
		t.Fatalf("templates decode: %v", err)
	}
	if len(tplBody.Templates) == 0 {
		t.Fatalf("no templates returned")
	}
	for _, tpl := range tplBody.Templates {
		if tpl.Published {
			t.Fatalf("template %q reports published=true, but templates are inert by design", tpl.ID)
		}
	}
}

func TestTask8HTTPDefinitionRunLifecycle(t *testing.T) {
	srv, key := task8HTTPServer(t)
	def := task8BranchDef()
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create definition = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "http-branch") {
		t.Fatalf("list definitions = %d: %s", rec.Code, rec.Body.String())
	}
	// Invalid definition is rejected at save time.
	bad := def
	bad.ID = "http-bad"
	bad.Nodes = append(append([]nexaflow.DefinitionNode{}, def.Nodes...),
		nexaflow.DefinitionNode{ID: "nope", NodeID: "nexaroute.control.nope", Version: nexaflow.SupportedVersion})
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid definition = %d, want 400", rec.Code)
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-branch/publish", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revision":1`) {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/http-branch/versions", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revision":1`) {
		t.Fatalf("versions = %d: %s", rec.Code, rec.Body.String())
	}
	// Execute through the API: fast branch for status ok.
	runBody := map[string]any{"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "idempotency_key": "http-run-1"}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-branch/runs", runBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run = %d: %s", rec.Code, rec.Body.String())
	}
	var runResp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runResp); err != nil {
		t.Fatalf("run decode: %v", err)
	}
	if runResp.State != "succeeded" {
		t.Fatalf("run state = %q, want succeeded", runResp.State)
	}
	// Inspect the run: per-node outputs, timing, attempts, failures.
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"selected_branch"`, `"fast"`, `"node_id":"slow"`, `"state":"skipped"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("run detail lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"error":"`) {
		t.Fatalf("run detail leaks raw error text (only codes may be exposed)")
	}
	// Audit endpoint records the lifecycle.
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/audit", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run_started") {
		t.Fatalf("audit = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTask8HTTPApprovalSeparationAndCancel(t *testing.T) {
	srv, key := task8HTTPServer(t)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "http-appr", Name: "http-appr",
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
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-appr/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-appr/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{}}, "idempotency_key": "http-appr-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}
	var runResp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &runResp)
	if runResp.State != "waiting_for_approval" {
		t.Fatalf("run state = %q, want waiting_for_approval", runResp.State)
	}
	// The admin-key identity created the run, so approving with the
	// same identity is self-approval and must be rejected.
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/approve",
		map[string]any{"node_id": "appr", "decision": "approved"})
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
		t.Fatalf("self-approval via HTTP = %d, want 400/409", rec.Code)
	}
	// Cancel by the owner admin identity succeeds.
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/cancel", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestTask8HTTPDisabledPreservesGateway(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "task8-admin-secret"
	srv := testGateway(t, cfg)
	// No AttachNexaflow call: routes must not exist.
	if rec := task8Do(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled nodes = %d, want 404", rec.Code)
	}
	if rec := task8Do(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/definitions", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled create = %d, want 404", rec.Code)
	}
	// Existing gateway behaviour is untouched.
	if rec := task8Do(t, srv, "", http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz with nexaflow disabled = %d, want 200", rec.Code)
	}
	// Attaching nil is the same as not attaching.
	srv.AttachNexaflow(nil)
	if rec := task8Do(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nil-attached nodes = %d, want 404", rec.Code)
	}
}

// The Workflows page exists in the served assets whether or not the
// NexaFlow service is attached: the page itself degrades gracefully
// when the routes are absent.
func TestTask8HTTPWorkflowsPagePresent(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-page="workflows"`) {
		t.Fatalf("index = %d, workflows nav missing", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://gateway/app.js", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "workflowsView") || !strings.Contains(rec.Body.String(), "/admin/api/nexaflow/runs") {
		t.Fatalf("app.js = %d, workflows view missing", rec.Code)
	}
}

func TestTask8HTTPWebhook(t *testing.T) {
	srv, _ := task8HTTPServer(t)
	starts := 0
	start := func(ctx context.Context, defID, trigger string, body []byte) (string, error) {
		starts++
		return fmt.Sprintf("run-%d", starts), nil
	}
	ledger := nexaflow.NewMemoryDeliveryLedger()
	svc, err := nexaflow.NewWebhookService(nexaflow.NewWebhookVerifier(), ledger, start)
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	srv.AttachNexaflowWebhook(svc, map[string]string{"wf-1": "hook-secret"})
	sign := func(ts, body string) string {
		mac := hmac.New(sha256.New, []byte("hook-secret"))
		mac.Write([]byte(ts + "." + body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	post := func(ts, sig, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-1/hook", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Nexaflow-Timestamp", ts)
		req.Header.Set("X-Nexaflow-Signature", sig)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := `{"event":"ping"}`
	sig := sign(ts, body)
	if rec := post(ts, sig, body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run-1") {
		t.Fatalf("valid webhook = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post(ts, sig, body); rec.Code != http.StatusConflict {
		t.Fatalf("replay webhook = %d, want 409", rec.Code)
	}
	if rec := post(ts, "deadbeef", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-signature webhook = %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/unknown/hook", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Nexaflow-Timestamp", ts)
	req.Header.Set("X-Nexaflow-Signature", sig)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-definition webhook = %d, want 404", rec.Code)
	}
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}
