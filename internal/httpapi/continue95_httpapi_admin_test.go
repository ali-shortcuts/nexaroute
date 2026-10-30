package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"

	"golang.org/x/oauth2"
)

// Continue95 admin NexaFlow task8 coverage (issue #663): NEW tests only.
// Helpers carry the Continue95Httpapi prefix; the task8* helpers from
// the existing test file are deliberately not reused here.

func Continue95HttpapiAdminServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "continue95-admin-secret"
	srv := testGateway(t, cfg)
	store := nexaflow.NewMemoryStore()
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "continue95"})
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

func Continue95HttpapiAdminDoRaw(t *testing.T, srv *Server, key, method, path, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(raw))
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

func Continue95HttpapiAdminDo(t *testing.T, srv *Server, key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		return Continue95HttpapiAdminDoRaw(t, srv, key, method, path, "")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return Continue95HttpapiAdminDoRaw(t, srv, key, method, path, string(raw))
}

func Continue95HttpapiAdminDoUnauth(t *testing.T, srv *Server, method, path string, suffix int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://gateway"+path, nil)
	// Distinct loopback addresses per unauthenticated call: the admin
	// bucket charges a full bucket to an unauthenticated address, so
	// reusing one address would turn later calls into 429s.
	req.RemoteAddr = fmt.Sprintf("127.0.0.%d:1234", suffix)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func Continue95HttpapiBranchDef() nexaflow.Definition {
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
		ID:            "continue95-branch", Name: "continue95-branch",
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

func TestContinue95HttpapiNexaflowErrStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nexaflow.ErrNotFound, http.StatusNotFound},
		{nexaflow.ErrAlreadyExists, http.StatusConflict},
		{nexaflow.ErrIdempotencyConflict, http.StatusConflict},
		{nexaflow.ErrApprovalConflict, http.StatusConflict},
		{nexaflow.ErrTerminalState, http.StatusConflict},
		{nexaflow.ErrStateConflict, http.StatusConflict},
		{nexaflow.ErrNotWaiting, http.StatusConflict},
		{errors.New("plain validation failure"), http.StatusBadRequest},
		{fmt.Errorf("wrapped: %w", nexaflow.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("wrapped: %w", nexaflow.ErrTerminalState), http.StatusConflict},
	}
	for _, tc := range cases {
		if got := nexaflowErrStatus(tc.err); got != tc.want {
			t.Fatalf("nexaflowErrStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestContinue95HttpapiNexaflowDecodeBodyAndMethod(t *testing.T) {
	// Valid JSON decodes.
	{
		var v struct {
			Name string `json:"name"`
		}
		req := httptest.NewRequest(http.MethodPost, "http://gateway/x", strings.NewReader(`{"name":"ok"}`))
		rr := httptest.NewRecorder()
		if !nexaflowDecodeBody(rr, req, &v) || v.Name != "ok" {
			t.Fatalf("valid decode: ok=%v v=%+v code=%d body=%s", true, v, rr.Code, rr.Body.String())
		}
	}
	// Invalid JSON -> 400.
	{
		var v struct {
			Name string `json:"name"`
		}
		req := httptest.NewRequest(http.MethodPost, "http://gateway/x", strings.NewReader(`{bad`))
		rr := httptest.NewRecorder()
		if nexaflowDecodeBody(rr, req, &v) || rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid JSON: code=%d body=%s, want 400", rr.Code, rr.Body.String())
		}
	}
	// Empty body is accepted (zero value).
	{
		var v struct {
			Name string `json:"name"`
		}
		req := httptest.NewRequest(http.MethodPost, "http://gateway/x", nil)
		rr := httptest.NewRecorder()
		if !nexaflowDecodeBody(rr, req, &v) {
			t.Fatalf("empty body must decode as zero value, code=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	// Unreadable body -> 400.
	{
		var v struct {
			Name string `json:"name"`
		}
		req := httptest.NewRequest(http.MethodPost, "http://gateway/x", &Continue95HttpapiErrorReader{})
		rr := httptest.NewRecorder()
		if nexaflowDecodeBody(rr, req, &v) || rr.Code != http.StatusBadRequest {
			t.Fatalf("unreadable body: code=%d body=%s, want 400", rr.Code, rr.Body.String())
		}
	}
	// Method gate: allowed passes, anything else answers 405.
	{
		gate := &Server{}
		req := httptest.NewRequest(http.MethodGet, "http://gateway/x", nil)
		rr := httptest.NewRecorder()
		if !gate.nexaflowRequireMethod(rr, req, http.MethodGet, http.MethodPost) {
			t.Fatalf("allowed method must pass")
		}
		req = httptest.NewRequest(http.MethodDelete, "http://gateway/x", nil)
		rr = httptest.NewRecorder()
		if gate.nexaflowRequireMethod(rr, req, http.MethodGet) || rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("disallowed method: code=%d, want 405", rr.Code)
		}
	}
}

type Continue95HttpapiErrorReader struct{}

func (Continue95HttpapiErrorReader) Read(p []byte) (int, error) {
	return 0, errors.New("continue95 simulated read failure")
}

func TestContinue95HttpapiNexaflowSummaries(t *testing.T) {
	run := nexaflow.Run{
		ID: "run-1", DefinitionID: "def-1", Revision: 3, State: nexaflow.RunState("succeeded"),
		Principal: "admin", ErrorCode: "E_NONE",
	}
	sum := nexaflowRunSummary(run)
	if sum["id"] != "run-1" || sum["definition_id"] != "def-1" || sum["revision"] != uint64(3) ||
		sum["state"] != nexaflow.RunState("succeeded") || sum["principal"] != "admin" || sum["error_code"] != "E_NONE" {
		t.Fatalf("run summary = %v", sum)
	}
	exec := nexaflow.NodeExecution{
		NodeID: "n1", Attempt: 2, State: nexaflow.NodeState("succeeded"), Output: map[string]any{"done": true},
		OutputTruncated: true, ErrorCode: "", Ambiguous: true, Dispatched: true,
	}
	view := nexaflowNodeView(exec)
	if view["node_id"] != "n1" || view["attempt"] != 2 || view["state"] != nexaflow.NodeState("succeeded") ||
		view["output_truncated"] != true || view["ambiguous"] != true || view["dispatched"] != true {
		t.Fatalf("node view = %v", view)
	}
	// The view must never carry raw error text or node inputs.
	if _, ok := view["error"]; ok {
		t.Fatalf("node view leaks an error field: %v", view)
	}
	if _, ok := view["input"]; ok {
		t.Fatalf("node view leaks an input field: %v", view)
	}
}

func TestContinue95HttpapiNexaflowAdminGate(t *testing.T) {
	// No service attached: admin gate fails closed.
	plain := &Server{}
	if _, ok := plain.nexaflowAdmin(httptest.NewRequest(http.MethodGet, "http://gateway/", nil)); ok {
		t.Fatalf("nexaflowAdmin with no service must fail")
	}
	// Service attached but no admin credential: fails closed.
	srv, key := Continue95HttpapiAdminServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/nexaflow/nodes", nil)
	req.RemoteAddr = "127.0.0.3:1234"
	if _, ok := srv.nexaflowAdmin(req); ok {
		t.Fatalf("nexaflowAdmin without a credential must fail")
	}
	// Service attached with the admin key: passes and yields an identity.
	req = httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/nexaflow/nodes", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("x-admin-key", key)
	identity, ok := srv.nexaflowAdmin(req)
	if !ok {
		t.Fatalf("nexaflowAdmin with the admin key must pass")
	}
	if identity.Subject == "" {
		t.Fatalf("nexaflowAdmin returned a zero identity: %+v", identity)
	}
}

func TestContinue95HttpapiNexaflowNodesTemplatesMethodAndAuth(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	// Unauthenticated nodes/templates: 401 (distinct loopback addrs).
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/nodes", 11); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth nodes = %d, want 401", rec.Code)
	}
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/templates", 12); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth templates = %d, want 401", rec.Code)
	}
	// Wrong method after auth: 405.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST nodes = %d, want 405", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodDelete, "/admin/api/nexaflow/templates", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE templates = %d, want 405", rec.Code)
	}
	// Happy paths still answer with content.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nexaroute.control.branch") {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/templates", nil); rec.Code != http.StatusOK {
		t.Fatalf("templates = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiNexaflowDefinitionsErrors(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	def := Continue95HttpapiBranchDef()
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	// Draft saves upsert: re-saving the same definition succeeds again.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "continue95-branch") {
		t.Fatalf("duplicate create = %d: %s, want 201 upsert", rec.Code, rec.Body.String())
	}
	// Invalid JSON -> 400; wrong method -> 405; unauthenticated -> 401.
	if rec := Continue95HttpapiAdminDoRaw(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON create = %d, want 400", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPut, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT definitions = %d, want 405", rec.Code)
	}
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/definitions", 13); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth definitions = %d, want 401", rec.Code)
	}
	// List still contains the one definition.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "continue95-branch") {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiNexaflowDefinitionByIDShapes(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	def := Continue95HttpapiBranchDef()
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := "continue95-branch"
	// Unknown sub-resource shape -> 404.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+id+"/bogus", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown shape = %d, want 404", rec.Code)
	}
	// Listing revisions of an unknown definition answers 200 with an
	// empty list (the store has no error for an unknown id here).
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/no-such/versions", nil); rec.Code != http.StatusOK {
		t.Fatalf("unknown versions list = %d, want 200 empty", rec.Code)
	}
	// Unknown definition in the other known shapes -> 404.
	for _, p := range []string{
		"/admin/api/nexaflow/definitions/no-such",
		"/admin/api/nexaflow/definitions/no-such/publish",
		"/admin/api/nexaflow/definitions/no-such/versions/1",
		"/admin/api/nexaflow/definitions/no-such/runs",
	} {
		method := http.MethodGet
		if strings.HasSuffix(p, "/publish") || strings.HasSuffix(p, "/runs") {
			method = http.MethodPost
		}
		if rec := Continue95HttpapiAdminDo(t, srv, key, method, p, map[string]any{}); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", method, p, rec.Code)
		}
	}
	// Canonical runs shape (admin_nexaflow_task8.go: "runs (GET lists,
	// POST starts)"): GET on a definition's runs lists (200), it is
	// not a wrong-method case. The old base's POST-only assumption
	// is superseded by the durable-line handler.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+id+"/runs", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runs"`) {
		t.Fatalf("GET %s/runs = %d: %s, want 200 with runs list", id, rec.Code, rec.Body.String())
	}
	// Wrong methods on known shapes -> 405.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/admin/api/nexaflow/definitions/" + id},
		{http.MethodGet, "/admin/api/nexaflow/definitions/" + id + "/publish"},
		{http.MethodPost, "/admin/api/nexaflow/definitions/" + id + "/versions"},
		{http.MethodPost, "/admin/api/nexaflow/definitions/" + id + "/versions/1"},
	} {
		if rec := Continue95HttpapiAdminDo(t, srv, key, tc.method, tc.path, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
	// Invalid revision -> 400.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+id+"/versions/abc", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid revision = %d, want 400", rec.Code)
	}
	// Draft fetch works; publish works; version fetch works.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("get draft = %d", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/"+id+"/publish", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revision":1`) {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+id+"/versions/1", nil); rec.Code != http.StatusOK {
		t.Fatalf("get version = %d", rec.Code)
	}
	// Unknown revision of a known definition -> 404.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+id+"/versions/99", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown revision = %d, want 404", rec.Code)
	}
	// Runs with an explicit unknown revision -> 404; invalid JSON -> 400.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/"+id+"/runs", map[string]any{"revision": 99}); rec.Code != http.StatusNotFound {
		t.Fatalf("run revision 99 = %d, want 404", rec.Code)
	}
	if rec := Continue95HttpapiAdminDoRaw(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/"+id+"/runs", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("run invalid JSON = %d, want 400", rec.Code)
	}
	// Unauthenticated definition access -> 401.
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/definitions/"+id, 14); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth definition = %d, want 401", rec.Code)
	}
}

func TestContinue95HttpapiNexaflowRunsAndApprovalErrors(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	def := Continue95HttpapiBranchDef()
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-branch/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	runBody := map[string]any{"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "idempotency_key": "continue95-run-1"}
	rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-branch/runs", runBody)
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
	if runResp.State != "succeeded" {
		t.Fatalf("run state = %q, want succeeded", runResp.State)
	}
	// Idempotent replay with the SAME input returns the original run
	// (201, same id) instead of creating a second run.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-branch/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "idempotency_key": "continue95-run-1"}); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), runResp.ID) {
		t.Fatalf("idempotent replay = %d: %s, want 201 with the original run id", rec.Code, rec.Body.String())
	}
	// Same key with DIFFERENT input is a conflict (409), not a silent
	// replay: engine.CreateRun documents that returning the original
	// run would hide that a different request was never executed.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-branch/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "bad"}}}, "idempotency_key": "continue95-run-1"}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "idempotency key already exists") {
		t.Fatalf("different-input replay = %d: %s, want 409 idempotency conflict", rec.Code, rec.Body.String())
	}
	// Runs list: unauth 401, wrong method 405, happy path lists the run.
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/runs", 15); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth runs = %d, want 401", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST runs = %d, want 405", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), runResp.ID) {
		t.Fatalf("list runs = %d: %s", rec.Code, rec.Body.String())
	}
	// Run-by-ID shapes.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID+"/bogus", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run shape = %d, want 404", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/no-such-run", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST run detail = %d, want 405", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID+"/approve", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve = %d, want 405", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID+"/cancel", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET cancel = %d, want 405", rec.Code)
	}
	// Approve validation: missing node_id -> 400, bad decision -> 400,
	// invalid JSON -> 400, unknown run -> 404.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/approve", map[string]any{"decision": "approved"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("approve missing node_id = %d, want 400", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/approve", map[string]any{"node_id": "chk", "decision": "maybe"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("approve bad decision = %d, want 400", rec.Code)
	}
	if rec := Continue95HttpapiAdminDoRaw(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/approve", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("approve invalid JSON = %d, want 400", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/no-such-run/approve", map[string]any{"node_id": "chk", "decision": "approved"}); rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
		t.Fatalf("approve unknown run = %d, want 404/400/409", rec.Code)
	}
	// Cancelling a succeeded run is a no-op success: 200, still
	// reported as succeeded (cancel is idempotent, not an error).
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/cancel", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "succeeded") {
		t.Fatalf("cancel succeeded run = %d: %s, want 200 succeeded", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/no-such-run/cancel", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown run = %d, want 404", rec.Code)
	}
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID, 16); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth run detail = %d, want 401", rec.Code)
	}
	// Audit: unauth 401, wrong method 405, happy path records the run.
	if rec := Continue95HttpapiAdminDoUnauth(t, srv, http.MethodGet, "/admin/api/nexaflow/audit", 17); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth audit = %d, want 401", rec.Code)
	}
	// The authorization matrix denies POST on the audit route before
	// the handler runs (403), so the handler's own 405 is exercised
	// by a direct call, as the route-preview tests do for theirs.
	{
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/nexaflow/audit", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("x-admin-key", key)
		rr := httptest.NewRecorder()
		srv.adminNexaflowAudit(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST audit (direct) = %d, want 405", rr.Code)
		}
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/audit", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run_started") {
		t.Fatalf("audit = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiNexaflowApprovalLifecycle(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "continue95-appr", Name: "continue95-appr",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "appr", NodeID: "nexaroute.control.approval", Version: nexaflow.SupportedVersion},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "appr"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-appr/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-appr/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{}}, "idempotency_key": "continue95-appr-1"})
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
	// Self-approval by the run's creator identity is rejected.
	rec = Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/approve",
		map[string]any{"node_id": "appr", "decision": "approved"})
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
		t.Fatalf("self-approval = %d, want 400/409", rec.Code)
	}
	// Cancel by the owner identity succeeds and reports cancelled.
	rec = Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/cancel", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}
	// Cancelling an already-cancelled run is idempotent: 200 again.
	rec = Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/cancel", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("second cancel = %d: %s, want 200 cancelled", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiNexaflowWebhookEdges(t *testing.T) {
	// No webhook service attached: the route does not exist (404).
	srv, _ := Continue95HttpapiAdminServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-1/hook", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("webhook without service = %d, want 404", rec.Code)
	}

	// Attach a webhook service with one secret and one empty secret.
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
	srv.AttachNexaflowWebhook(svc, map[string]string{"wf-1": "hook-secret", "wf-empty": ""})
	sign := func(ts, body string) string {
		mac := hmac.New(sha256.New, []byte("hook-secret"))
		mac.Write([]byte(ts + "." + body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	post := func(path, ts, sig, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
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
	// Wrong method -> 405.
	req = httptest.NewRequest(http.MethodGet, "http://gateway/nexaflow/webhooks/wf-1/hook", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET webhook = %d, want 405", rec.Code)
	}
	// Malformed path (one segment) -> 404.
	if rec := post("/nexaflow/webhooks/onlyone", ts, sig, body); rec.Code != http.StatusNotFound {
		t.Fatalf("one-segment webhook = %d, want 404", rec.Code)
	}
	// Definition without a configured secret -> 404.
	if rec := post("/nexaflow/webhooks/wf-empty/hook", ts, sig, body); rec.Code != http.StatusNotFound {
		t.Fatalf("empty-secret webhook = %d, want 404", rec.Code)
	}
	// Unknown definition -> 404.
	if rec := post("/nexaflow/webhooks/unknown/hook", ts, sig, body); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown webhook = %d, want 404", rec.Code)
	}
	// Valid delivery, then its replay, then a bad signature.
	if rec := post("/nexaflow/webhooks/wf-1/hook", ts, sig, body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run-1") {
		t.Fatalf("valid webhook = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post("/nexaflow/webhooks/wf-1/hook", ts, sig, body); rec.Code != http.StatusConflict {
		t.Fatalf("replay webhook = %d, want 409", rec.Code)
	}
	if rec := post("/nexaflow/webhooks/wf-1/hook", ts, "deadbeef", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-signature webhook = %d, want 401", rec.Code)
	}
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

// Continue95 wave 2 (issue #663): task8 direct auth, admin auth, admin
// provider/discovery edges, and simple-route edges.

func TestContinue95HttpapiNexaflowDirectAuth(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	// Direct handler calls without any admin identity: 401 from every handler.
	direct := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		path    string
		method  string
	}{
		{"nodes", srv.adminNexaflowNodes, "/admin/api/nexaflow/nodes", http.MethodGet},
		{"templates", srv.adminNexaflowTemplates, "/admin/api/nexaflow/templates", http.MethodGet},
		{"definitions", srv.adminNexaflowDefinitions, "/admin/api/nexaflow/definitions", http.MethodGet},
		{"definitionByID", srv.adminNexaflowDefinitionByID, "/admin/api/nexaflow/definitions/x", http.MethodGet},
		{"runs", srv.adminNexaflowRuns, "/admin/api/nexaflow/runs", http.MethodGet},
		{"runByID", srv.adminNexaflowRunByID, "/admin/api/nexaflow/runs/x", http.MethodGet},
		{"audit", srv.adminNexaflowAudit, "/admin/api/nexaflow/audit", http.MethodGet},
	}
	for _, c := range direct {
		req := httptest.NewRequest(c.method, "http://gateway"+c.path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		c.handler(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("direct %s = %d, want 401", c.name, rec.Code)
		}
	}
	// Empty-id 404s through the authenticated handler path.
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("empty definition id = %d, want 404", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("empty run id = %d, want 404", rec.Code)
	}
	// Webhook handler without an attached service: 404 by direct call.
	rec := httptest.NewRecorder()
	srv.nexaflowWebhookHandler(rec, httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-1/hook", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("webhook without service (direct) = %d, want 404", rec.Code)
	}
}

func TestContinue95HttpapiNexaflowWebhookUnreadableAndValidation(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	start := func(ctx context.Context, defID, trigger string, body []byte) (string, error) {
		return "run-1", nil
	}
	ledger := nexaflow.NewMemoryDeliveryLedger()
	svc, err := nexaflow.NewWebhookService(nexaflow.NewWebhookVerifier(), ledger, start)
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	srv.AttachNexaflowWebhook(svc, map[string]string{"wf-1": "hook-secret"})
	req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-1/hook", Continue95HttpapiErrorReader{})
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.nexaflowWebhookHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable webhook body = %d, want 400", rec.Code)
	}
	// SaveDraft validation failure: a definition referencing an unknown node.
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "continue95-badnode", Name: "continue95-badnode",
		Nodes: []nexaflow.DefinitionNode{{ID: "n1", NodeID: "nexaroute.nope.unknown", Version: nexaflow.SupportedVersion}},
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown-node definition = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiNexaflowApproveSuccess(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "continue95-appr2", Name: "continue95-appr2",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "appr", NodeID: "nexaroute.control.approval", Version: nexaflow.SupportedVersion},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "appr"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-appr2/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	pub, err := srv.nexaflow.LatestPublished(ctx, "continue95-appr2")
	if err != nil {
		t.Fatalf("LatestPublished: %v", err)
	}
	run, err := srv.nexaflow.StartRun(ctx, pub, authz.Identity{Subject: "continue95-creator"}, map[string]any{"payload": map[string]any{}}, "continue95-appr2-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err = srv.nexaflow.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if run.State != "waiting_for_approval" {
		t.Fatalf("run state = %q, want waiting_for_approval", run.State)
	}
	// The HTTP approver (admin key identity) differs from the creator, so
	// self-approval rejection does not fire and the run completes.
	rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+run.ID+"/approve",
		map[string]any{"node_id": "appr", "decision": "approved"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "succeeded") {
		t.Fatalf("approve = %d: %s, want 200 succeeded", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiAdminAuthDirect(t *testing.T) {
	srv, _ := Continue95HttpapiAdminServer(t)
	// Two cookies with the session name: malformed, unauthenticated.
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/status", nil)
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: strings.Repeat("A", 43)})
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: strings.Repeat("A", 43)})
	_, _, present, authed, err := srv.sessionIdentity(req)
	if err != nil || !present || authed {
		t.Fatalf("two-cookie session: present=%v authed=%v err=%v, want present + !authed", present, authed, err)
	}
	// One well-formed cookie with OIDC disabled: hash returned, not authed.
	req2 := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/status", nil)
	req2.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: strings.Repeat("A", 43)})
	_, hash, present, authed, err := srv.sessionIdentity(req2)
	if err != nil || !present || authed || hash == "" {
		t.Fatalf("oidc-disabled session: present=%v authed=%v hash=%q err=%v", present, authed, hash, err)
	}
	// recordAudit with no security store.
	if err := (&Server{}).recordAudit(securitystore.AuditEvent{}); err == nil {
		t.Fatalf("recordAudit without a store unexpectedly succeeded")
	}
	// authLogin with a broken security store: 503 by direct call.
	broken := testGateway(t, config.Default())
	broken.securityErr = errors.New("continue95 security failure")
	rec := httptest.NewRecorder()
	broken.authLogin(rec, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authLogin broken store = %d, want 503", rec.Code)
	}
	// authCallback with OIDC disabled: 503 by direct call.
	rec = httptest.NewRecorder()
	broken.securityErr = nil
	broken.authCallback(rec, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/auth/callback?state=x&code=y", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authCallback oidc-disabled = %d, want 503", rec.Code)
	}
	// Logout with no security store: 503 by direct call on an empty server.
	rec = httptest.NewRecorder()
	(&Server{}).adminAuthLogout(rec, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/logout", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("logout without store = %d, want 503", rec.Code)
	}
}

func TestContinue95HttpapiAdminProviderEdges(t *testing.T) {
	srv := testGateway(t, config.Default())
	// Connection check for a provider type without a models endpoint.
	// Keyless admin access requires a loopback Host, hence localhost here.
	req := httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/provider-check",
		strings.NewReader(`{"provider":{"id":"x","type":"opencode-go","base_url":"http://127.0.0.1:9"}}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "does not expose a models endpoint") {
		t.Fatalf("opencode-go connection check = %d: %s", rec.Code, rec.Body.String())
	}
	// Direct method rejections.
	rec = httptest.NewRecorder()
	srv.adminProbe(rec, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/probe", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("adminProbe GET = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.adminProviderDiscover(rec, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/provider-discover", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("adminProviderDiscover GET = %d, want 405", rec.Code)
	}
	// Discover with an out-of-range credential index: 400.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/provider-discover",
		strings.NewReader(`{"provider":{"id":"p1","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"x"},"credential_index":5}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("discover bad credential index = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Discover success with a credential index: loopback models endpoint.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	t.Cleanup(fake.Close)
	body := fmt.Sprintf(`{"provider":{"id":"p1","type":"openai_compatible","base_url":%q,"api_key":"x","credentials":[{"api_key":"x","enabled":true}]},"credential_index":0}`, fake.URL)
	req = httptest.NewRequest(http.MethodPost, "http://localhost/admin/api/provider-discover", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"credential_index":0`) {
		t.Fatalf("discover with index = %d: %s, want 200 + credential_index", rec.Code, rec.Body.String())
	}
	// Provider PUT/DELETE edges on a server with providers p1/p2.
	srv2 := testGateway(t, simpleRouteTestConfig())
	put := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "http://localhost"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv2.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := put("/admin/api/providers/nosuch", `{"provider":{"id":"nosuch","type":"openai_compatible","base_url":"https://example.com/v1"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown provider = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if rec := put("/admin/api/providers/p1", `{"provider":{"type":"openai_compatible","base_url":"https://example.com/v1"}}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"saved":true`) {
		t.Fatalf("PUT empty-id provider = %d, want 200 saved: %s", rec.Code, rec.Body.String())
	}
	// normalizeProvider defaults a zero weight to 1.
	p := &config.ProviderConfig{Models: []config.ModelConfig{{Model: " m ", Weight: 0}}}
	normalizeProvider(p)
	if p.Models[0].Weight != 1 {
		t.Fatalf("normalize weight = %v, want 1", p.Models[0].Weight)
	}
	// Credential-pool update error propagation: indexed entry without any key.
	zero := 0
	if _, err := applyCredentialPoolUpdate([]config.CredentialConfig{{}}, credentialPoolUpdate{Entries: []credentialPoolEntry{{Index: &zero}}}); err == nil || !strings.Contains(err.Error(), "api_key or api_key_env") {
		t.Fatalf("pool update without key: err=%v", err)
	}
}

func TestContinue95HttpapiSimpleRoutesEdges(t *testing.T) {
	// Create validation, duplicate, and ByID errors.
	s := testGateway(t, simpleRouteTestConfig())
	bad := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"route-coding","name":"Coding","public_model":"coding","mode":"sideways","deployments":["p1/m1"],"enabled":true}`)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "mode must be") {
		t.Fatalf("bad mode = %d: %s", bad.Code, bad.Body.String())
	}
	good := `{"id":"route-coding","name":"Coding","public_model":"coding","mode":"automatic","deployments":["p1/m1","p2/m2"],"enabled":true}`
	if rec := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", good); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", good); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("duplicate = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := simpleRouteRequest(t, s, http.MethodGet, "/admin/api/simple-routes/", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty by-id = %d, want 400", rec.Code)
	}
	// A literally unescaped path cannot be built by httptest.NewRequest
	// (it parses the URL), so rewrite the path after construction; the
	// handler's PathUnescape then fails exactly as for a real bad escape.
	badEsc := httptest.NewRequest(http.MethodGet, "http://localhost/admin/api/simple-routes/placeholder", nil)
	badEsc.URL.Path = "/admin/api/simple-routes/%zz"
	badEsc.RemoteAddr = "127.0.0.1:12345"
	badRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(badRec, badEsc)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("bad-escape by-id = %d, want 400", badRec.Code)
	}
	// PUT edges: id mismatch, normalize error, unknown id.
	if rec := simpleRouteRequest(t, s, http.MethodPut, "/admin/api/simple-routes/route-coding",
		`{"id":"other","public_model":"coding","mode":"automatic","deployments":["p1/m1"]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "must match URL") {
		t.Fatalf("PUT id mismatch = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := simpleRouteRequest(t, s, http.MethodPut, "/admin/api/simple-routes/route-coding",
		`{"public_model":"coding","mode":"sideways","deployments":["p1/m1"]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "mode must be") {
		t.Fatalf("PUT normalize error = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := simpleRouteRequest(t, s, http.MethodPut, "/admin/api/simple-routes/nosuch",
		`{"public_model":"coding","mode":"automatic","deployments":["p1/m1"]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("PUT unknown = %d: %s", rec.Code, rec.Body.String())
	}
	// DELETE of an advanced-managed virtual endpoint.
	cfg := simpleRouteTestConfig()
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "adv", PublicModel: "adv-model", RouteProfile: "custom-profile"}}
	sAdv := testGateway(t, cfg)
	if rec := simpleRouteRequest(t, sAdv, http.MethodDelete, "/admin/api/simple-routes/adv", ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "advanced-managed") {
		t.Fatalf("DELETE advanced-managed = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiSimpleRoutesCollisions(t *testing.T) {
	good := `{"id":"route-coding","name":"Coding","public_model":"coding","mode":"automatic","deployments":["p1/m1","p2/m2"],"enabled":true}`
	// Route-profile id collision.
	cfg := simpleRouteTestConfig()
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "route-coding-profile"}}
	s := testGateway(t, cfg)
	if rec := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", good); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "conflicts") {
		t.Fatalf("profile collision = %d: %s", rec.Code, rec.Body.String())
	}
	// Candidate-pool stage-prefix collision.
	cfg = simpleRouteTestConfig()
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "route-coding-stage-1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	s = testGateway(t, cfg)
	if rec := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", good); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "conflicts") {
		t.Fatalf("pool-prefix collision = %d: %s", rec.Code, rec.Body.String())
	}
	// Fallback-chain id collision.
	cfg = simpleRouteTestConfig()
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "route-coding-fallback", Pools: []string{"p1/m1"}}}
	s = testGateway(t, cfg)
	if rec := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", good); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "conflicts") {
		t.Fatalf("fallback collision = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiSimpleRoutesPrimitives(t *testing.T) {
	cfg := simpleRouteTestConfig()
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "other", PublicModel: "other-model", RouteProfile: "other-profile"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "other-profile", CandidatePool: "unrelated-pool"}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "unrelated-pool", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.FallbackChains = []config.FallbackChainConfig{
		{ID: "zzz", Pools: []string{"unrelated-pool"}},
		{ID: "adv-fb", Pools: []string{"route-coding-pool"}},
	}
	s := testGateway(t, cfg)
	good := `{"id":"route-coding","name":"Coding","public_model":"coding","mode":"automatic","deployments":["p1/m1","p2/m2"],"enabled":true}`
	if rec := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", good); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := simpleRouteRequest(t, s, http.MethodDelete, "/admin/api/simple-routes/route-coding", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	after := s.currentConfig()
	for _, ve := range after.VirtualEndpoints {
		if ve.ID == "route-coding" {
			t.Fatalf("deleted route's virtual endpoint survived")
		}
	}
	foundOtherVE, foundZZZ, foundAdvFB, foundPool := false, false, false, false
	for _, ve := range after.VirtualEndpoints {
		if ve.ID == "other" {
			foundOtherVE = true
		}
	}
	for _, fc := range after.FallbackChains {
		if fc.ID == "zzz" {
			foundZZZ = true
		}
		if fc.ID == "adv-fb" {
			foundAdvFB = true
		}
	}
	for _, cp := range after.CandidatePools {
		if cp.ID == "route-coding-pool" {
			foundPool = true
		}
	}
	if !foundOtherVE || !foundZZZ || !foundAdvFB || !foundPool {
		t.Fatalf("primitives kept: otherVE=%v zzz=%v advFB=%v pool=%v (all must survive)", foundOtherVE, foundZZZ, foundAdvFB, foundPool)
	}
	for _, rp := range after.RouteProfiles {
		if rp.ID == "route-coding-profile" {
			t.Fatalf("deleted route's profile survived")
		}
	}
}

// ---- Wave 3 (issue #663 continuation) ----

func TestContinue95HttpapiOidcDirect(t *testing.T) {
	// newOIDCAuthenticator with an invalid config fails validation.
	if _, err := newOIDCAuthenticator(context.Background(), config.OIDCConfig{}); err == nil {
		t.Fatalf("empty OIDC config did not error")
	}
	// Valid config but an unset secret env var errors.
	if _, err := newOIDCAuthenticator(context.Background(), config.OIDCConfig{
		IssuerURL: "https://example.com", ClientID: "c",
		ClientSecretEnv: "CONTINUE95_OIDC_SECRET_MISSING_7f3a",
	}); err == nil || !strings.Contains(err.Error(), "unset") {
		t.Fatalf("unset secret err=%v, want unset mention", err)
	}
	// Nil ctx falls back to Background, then discovery fails (loopback
	// port 1 refuses immediately).
	t.Setenv("CONTINUE95_OIDC_SECRET_SET", "x")
	if _, err := newOIDCAuthenticator(nil, config.OIDCConfig{
		IssuerURL: "http://127.0.0.1:1/", ClientID: "c",
		ClientSecretEnv: "CONTINUE95_OIDC_SECRET_SET",
	}); err == nil {
		t.Fatalf("nil-ctx discovery failure did not error")
	}
	// validateOIDCDiscovery with a nil provider errors.
	if err := validateOIDCDiscovery(nil, "https://example.com"); err == nil {
		t.Fatalf("nil provider discovery did not error")
	}
	// validOIDCEndpoint accepts a same-host https endpoint.
	if !validOIDCEndpoint("https://example.com/authorize", "https://example.com") {
		t.Fatalf("same-host https endpoint rejected")
	}
	// isLoopbackHTTP: unparseable -> false, localhost -> true.
	if isLoopbackHTTP("://bad") {
		t.Fatalf("unparseable URL treated as loopback")
	}
	if !isLoopbackHTTP("http://localhost:8080/x") {
		t.Fatalf("localhost URL not treated as loopback")
	}
	// Begin with an invalid browser binding errors.
	a := &oidcAuthenticator{pending: map[string]pendingOIDCLogin{}}
	if _, _, err := a.Begin("127.0.0.1", "", "short"); err == nil {
		t.Fatalf("invalid browser binding did not error")
	}
	// Begin prunes expired pending entries before inserting the new one.
	binding, err := randomURLToken(32)
	if err != nil {
		t.Fatalf("randomURLToken: %v", err)
	}
	a2 := &oidcAuthenticator{
		pending: map[string]pendingOIDCLogin{
			"expired": {sourceIP: "127.0.0.1", expiresAt: time.Now().Add(-time.Hour)},
		},
		oauth: &oauth2.Config{
			ClientID:    "c",
			RedirectURL: "http://localhost/cb",
			Endpoint:    oauth2.Endpoint{AuthURL: "https://example.com/auth"},
		},
	}
	if _, _, err := a2.Begin("127.0.0.1", "", binding); err != nil {
		t.Fatalf("Begin with expired entry failed: %v", err)
	}
	if _, stillThere := a2.pending["expired"]; stillThere {
		t.Fatalf("expired pending entry was not pruned")
	}
	if len(a2.pending) != 1 {
		t.Fatalf("pending entries = %d, want 1 (the new login)", len(a2.pending))
	}
	// takeState: a decodable state with an undecodable binding fails.
	// (The state itself must be 32-256 chars; the binding must be 43
	// chars to reach the decode check, so the bad binding is 43 '!'.)
	state, err := randomURLToken(32)
	if err != nil {
		t.Fatalf("randomURLToken state: %v", err)
	}
	a3 := &oidcAuthenticator{pending: map[string]pendingOIDCLogin{
		hashState(state): {sourceIP: "127.0.0.1", browserBindingHash: hashState(binding), expiresAt: time.Now().Add(time.Hour)},
	}}
	if _, ok := a3.takeState(state, strings.Repeat("!", 43)); ok {
		t.Fatalf("undecodable binding takeState succeeded")
	}
	if _, ok := a3.takeState(state, binding); !ok {
		t.Fatalf("valid binding takeState failed")
	}
	// claimStringValues wraps a bare string in a list.
	vals, err := claimStringValues(json.RawMessage(`"viewer"`))
	if err != nil || len(vals) != 1 || vals[0] != "viewer" {
		t.Fatalf("claimStringValues = (%v, %v), want [viewer]", vals, err)
	}
}

func TestContinue95HttpapiAdminAuthDirect2(t *testing.T) {
	// authLogin Begin error: 32 pending logins for the source IP trip the
	// per-IP cap, so authLogin 503s with "could not start OIDC
	// authentication".
	srv, _ := Continue95HttpapiAdminServer(t)
	srv.cfg.Admin.OIDC.Enabled = true
	pending := make(map[string]pendingOIDCLogin, 32)
	for i := 0; i < 32; i++ {
		pending[fmt.Sprintf("k%d", i)] = pendingOIDCLogin{sourceIP: "127.0.0.1", expiresAt: time.Now().Add(time.Hour)}
	}
	srv.oidc = &oidcAuthenticator{
		pending: pending,
		oauth: &oauth2.Config{
			ClientID:    "c",
			RedirectURL: "http://localhost/cb",
			Endpoint:    oauth2.Endpoint{AuthURL: "https://example.com/auth"},
		},
	}
	srv.oidcErr = nil
	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin/api/auth/login", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.authLogin(rr, req)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "could not start OIDC authentication") {
		t.Fatalf("Begin-cap login status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiAdminDirect2(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	// Provider-test custom concurrency: MaxConcurrency 2 is honoured
	// (the branch replaces the default 16-worker semaphore).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"upstream-model"}]}`)
	}))
	t.Cleanup(up.Close)
	ptBody := fmt.Sprintf(`{"provider":{"id":"p1","type":"openai_compatible","base_url":%q,"api_key":"x","max_concurrency":2,"models":[{"id":"m","model":"upstream-model","enabled":true}]},"test_models":["upstream-model"]}`, up.URL)
	ptReq := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/provider-test", strings.NewReader(ptBody))
	ptReq.Header.Set("Content-Type", "application/json")
	ptReq.Header.Set("x-admin-key", key)
	ptReq.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, ptReq)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "upstream-model") {
		t.Fatalf("provider-test concurrency status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Provider DELETE mutate-failure: deleting a provider still referenced
	// by a pricing version fails config validation with 400 (the
	// else-branch, as opposed to the 404 not-found branch).
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up2.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "continue95-admin-secret"
	cfg.Pricing.Versions = []config.PriceVersionConfig{{
		Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z",
		Prices: map[string]config.PriceEntryConfig{"p1/m": {InputPerMTok: 1, OutputPerMTok: 2}},
	}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: up2.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true}},
	}}
	srv2 := testGateway(t, cfg)
	rr = Continue95HttpapiAdminDo(t, srv2, key, http.MethodDelete, "/admin/api/providers/p1", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("priced-provider delete status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiClientKeysDirect(t *testing.T) {
	srv, key := Continue95HttpapiAdminServer(t)
	// POST with an invalid expires_at is rejected.
	rr := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/client-keys", map[string]any{
		"expires_at": "not-a-date",
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "expires_at") {
		t.Fatalf("bad expires_at status=%d body=%s", rr.Code, rr.Body.String())
	}
	// GET clamps a negative remaining budget to zero.
	if _, err := srv.mutateConfig(func(cfg *config.Config) error {
		cfg.ClientAuth.VirtualKeys = append(cfg.ClientAuth.VirtualKeys, config.VirtualKeyConfig{ID: "vk1", KeyHash: keyDigest("continue95-vk-secret"), BudgetUSD: 1})
		cfg.ClientAuth.Enabled = true
		return nil
	}); err != nil {
		t.Fatalf("seed virtual key: %v", err)
	}
	if _, ok := srv.budgets.Reserve("vk1", 10, 5, time.Now()); !ok {
		t.Fatalf("budget reserve failed")
	}
	rr = Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/client-keys", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("client-keys GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	var keysDoc struct {
		Keys []struct {
			ID                 string   `json:"id"`
			BudgetRemainingUSD *float64 `json:"budget_remaining_usd"`
			BudgetCommittedUSD *float64 `json:"budget_committed_usd"`
			BudgetReservedUSD  *float64 `json:"budget_reserved_usd"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &keysDoc); err != nil {
		t.Fatalf("client-keys JSON: %v (%s)", err, rr.Body.String())
	}
	found := false
	for _, k := range keysDoc.Keys {
		if k.ID == "vk1" {
			found = true
			if k.BudgetRemainingUSD == nil || *k.BudgetRemainingUSD != 0 {
				t.Fatalf("vk1 remaining = %v, want clamped 0", k.BudgetRemainingUSD)
			}
			if k.BudgetReservedUSD == nil || *k.BudgetReservedUSD != 5 {
				t.Fatalf("vk1 reserved = %v, want 5", k.BudgetReservedUSD)
			}
		}
	}
	if !found {
		t.Fatalf("vk1 missing from client-keys: %s", rr.Body.String())
	}
	// Rotate of an unknown id 404s.
	rr = Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/client-keys/nope/rotate", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("rotate unknown status=%d body=%s, want 404", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiTask8WebhookNotFound(t *testing.T) {
	srv, _ := Continue95HttpapiAdminServer(t)
	// The handler's ErrNotFound branch: a registered secret for an unknown
	// definition, with a valid HMAC signature, reaches Start, which finds
	// no such definition.
	if srv.nexaflowSecrets == nil {
		srv.nexaflowSecrets = map[string]string{}
	}
	srv.nexaflowSecrets["ghost"] = "continue95-secret"
	body := `{"hello":"world"}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte("continue95-secret"))
	_, _ = mac.Write([]byte(ts + "." + body))
	sig := hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/ghost/trig", strings.NewReader(body))
	req.Header.Set("X-Nexaflow-Timestamp", ts)
	req.Header.Set("X-Nexaflow-Signature", sig)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("ghost webhook status=%d body=%s, want 404", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiAutopilotDirect2(t *testing.T) {
	// adminAutopilotAction with malformed JSON 400s.
	cfg := config.Default()
	cfg.Autopilot.Enabled = true
	cfg.Decision.Policy = "balanced"
	ctrl := newAutopilotController(cfg, []policy.Policy{{ID: "balanced", Version: "1"}})
	if ctrl == nil {
		t.Fatalf("autopilot controller did not build")
	}
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	srv.autopilot = ctrl
	badReq := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/autopilot", strings.NewReader(`{bad`))
	badRr := httptest.NewRecorder()
	srv.adminAutopilotAction(badRr, badReq)
	if badRr.Code != http.StatusBadRequest {
		t.Fatalf("autopilot bad JSON status=%d body=%s", badRr.Code, badRr.Body.String())
	}
	// Canary/shadow branch: a staged candidate policy builds a canary
	// controller instead of a plain one.
	canaryCfg := config.Default()
	canaryCfg.Autopilot.Enabled = true
	canaryCfg.Autopilot.CandidatePolicyID = "candidate"
	canaryCfg.Autopilot.CanaryPercent = 100
	canaryCfg.Decision.Policy = "balanced"
	canaryCtrl := newAutopilotController(canaryCfg, []policy.Policy{{ID: "balanced", Version: "1"}, {ID: "candidate", Version: "2"}})
	if canaryCtrl == nil {
		t.Fatalf("canary controller did not build")
	}
	st := canaryCtrl.Status()
	if st.State != autopilot.StateCanary || st.Candidate == nil || st.Candidate.ID != "candidate" {
		t.Fatalf("canary status = %+v, want canary/candidate", st)
	}
	// RecordFinish with an empty requestID returns early.
	srv.autopilotRecordFinish("", 200, "", 0)
	// Promote success: a canary with enough all-success samples passes
	// the gate and promotes.
	promoteCfg := config.Default()
	promoteCfg.Autopilot.Enabled = true
	promoteCfg.Autopilot.CandidatePolicyID = "candidate"
	promoteCfg.Autopilot.CanaryPercent = 100
	promoteCfg.Autopilot.MinSamples = 10
	promoteCfg.Autopilot.MinPromoteSamples = 20
	promoteCfg.Decision.Policy = "balanced"
	pCtrl := newAutopilotController(promoteCfg, []policy.Policy{{ID: "balanced", Version: "1"}, {ID: "candidate", Version: "2"}})
	if pCtrl == nil {
		t.Fatalf("promote controller did not build")
	}
	for i := 0; i < 50; i++ {
		lease := pCtrl.Select(fmt.Sprintf("continue95-promote-%d", i))
		// Full gate-signal coverage: each sample must explicitly report
		// both the no-eligible and budget-anomaly signals (false here).
		pCtrl.Record(lease, autopilot.Sample{Success: true, HasNoEligibleSignal: true, HasBudgetAnomalySignal: true})
	}
	if res := pCtrl.Evaluate(); res.Verdict != autopilot.VerdictPromote {
		t.Fatalf("gate verdict = %v reasons %v, want Promote", res.Verdict, res.Reasons)
	}
	if _, ok := pCtrl.Promote(); !ok {
		t.Fatalf("gate-passing canary did not promote")
	}
}

// ---- Wave 4 (issue #663 continuation) ----

// Continue95HttpapiFailingGuard is a BudgetGuard that always refuses the
// reservation, so the runs-creation StartRun error branch can be driven
// without a real budget backend.
type Continue95HttpapiFailingGuard struct{}

func (Continue95HttpapiFailingGuard) Reserve(ctx context.Context, key string, estimateUSD float64) (func(), error) {
	return nil, errors.New("continue95 guard boom")
}

func TestContinue95HttpapiAdminCompatFallback(t *testing.T) {
	// With deployments present but the capability store empty, the
	// matrix falls back to dialectFor for the protocol instead of
	// reading a contract.
	up := httptest.NewServer(&Continue95HttpapiFakeUpstream{})
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "continue95-admin-secret"
	cfg.Providers = []config.ProviderConfig{{ID: "ga", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1}}}}
	srv := testGateway(t, cfg)
	srv.capStore.Reset()
	rec := Continue95HttpapiAdminDo(t, srv, "continue95-admin-secret", http.MethodGet, "/admin/api/compat", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ga/m") {
		t.Fatalf("compat fallback = %d: %s, want 200 with ga/m", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiAdminCompatReset(t *testing.T) {
	// Compat reset: malformed JSON is rejected, and a single deployment
	// reset reports that deployment.
	srv, key := Continue95HttpapiAdminServer(t)
	bad := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/compat/reset", strings.NewReader(`{bad`))
	bad.Header.Set("Content-Type", "application/json")
	bad.Header.Set("x-admin-key", key)
	bad.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reset bad JSON = %d, want 400", rec.Code)
	}
	one := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/compat/reset", strings.NewReader(`{"deployment":"ga/m"}`))
	one.Header.Set("Content-Type", "application/json")
	one.Header.Set("x-admin-key", key)
	one.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, one)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ga/m") {
		t.Fatalf("reset single = %d: %s, want 200 with ga/m", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiAdminEventsDirect2(t *testing.T) {
	// Event stream guards: wrong method, bad limit, bad since, a valid
	// stream over a cancelled context (connected frame, then return),
	// and subscriber-capacity exhaustion.
	srv, key := Continue95HttpapiAdminServer(t)
	// The admin authorization matrix denies POST on the stream route
	// before the handler runs, so the handler's own 405 is exercised by
	// a direct call (the same pattern as the audit 405).
	postRec := httptest.NewRecorder()
	srv.adminEventStream(postRec, httptest.NewRequest(http.MethodPost, "/admin/api/events/stream", nil))
	if postRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("stream POST = %d, want 405", postRec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/events/stream?limit=abc", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("stream bad limit = %d, want 400", rec.Code)
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/events/stream?since=abc", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("stream bad since = %d, want 400", rec.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/events/stream?limit=2", nil).WithContext(ctx)
	req.Header.Set("x-admin-key", key)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ": connected") {
		t.Fatalf("stream cancelled = %d: %s, want 200 with : connected", rec.Code, rec.Body.String())
	}
	var cancels []func()
	for i := 0; i < 256; i++ {
		_, _, cancelFn, ok := srv.bus.SubscribeSnapshotSince(0, 64)
		if !ok {
			break
		}
		cancels = append(cancels, cancelFn)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodGet, "/admin/api/events/stream?limit=1", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stream capacity-exhausted = %d, want 503", rec.Code)
	}
}

func TestContinue95HttpapiAdminNormalizeWeight(t *testing.T) {
	// normalizeProvider defaults a zero model weight to 1.
	p := &config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		Models: []config.ModelConfig{{ID: "m1", Model: "m", Weight: 0}}}
	normalizeProvider(p)
	if p.Models[0].Weight != 1 {
		t.Fatalf("weight = %v, want 1", p.Models[0].Weight)
	}
}

func Continue95HttpapiGuardServer(t *testing.T, guard nexaflow.BudgetGuard) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "continue95-admin-secret"
	srv := testGateway(t, cfg)
	store := nexaflow.NewMemoryStore()
	reg := nexaflow.DefaultRegistry()
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "continue95"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := nexaflow.NewService(store, engine, reg, guard)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv.AttachNexaflow(svc)
	return srv, cfg.Admin.APIKey
}

func TestContinue95HttpapiTask8GuardFail(t *testing.T) {
	// A failing budget guard makes run creation fail at StartRun.
	srv, key := Continue95HttpapiGuardServer(t, Continue95HttpapiFailingGuard{})
	def := Continue95HttpapiBranchDef()
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-branch/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := Continue95HttpapiAdminDo(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/continue95-branch/runs",
		map[string]any{"idempotency_key": "continue95-guard-1"})
	if rec.Code == http.StatusCreated || !strings.Contains(rec.Body.String(), "budget reservation") {
		t.Fatalf("guard-fail run = %d: %s, want non-201 budget reservation error", rec.Code, rec.Body.String())
	}
}
