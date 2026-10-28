package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

func testNexaflowServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "nexaflow-test-admin-key"
	s := testGateway(t, cfg)
	store := nexaflow.NewMemoryStore()
	// The store is its own audit sink, as in production (FileStore):
	// otherwise the audit endpoint (which reads the store) would never
	// see the engine's events.
	if err := s.AttachNexaflow(store, store, "http://127.0.0.1:9", ""); err != nil {
		t.Fatalf("AttachNexaflow: %v", err)
	}
	return s
}

func nexaflowReq(t *testing.T, s *Server, method, path, body string, withKey bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "http://gateway"+path, reader)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	if withKey {
		req.Header.Set("x-admin-key", "nexaflow-test-admin-key")
	} else {
		req.RemoteAddr = "127.0.0.2:12345"
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func nexaflowTestDef(id string) nexaflow.Definition {
	return nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            id,
		Name:          id,
		TenantScope:   "tenant-a",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "t", NodeID: "nexaroute.control.json_transform", Version: nexaflow.SupportedVersion, Input: map[string]any{
				"value": map[string]any{"$ref": "input.value"},
				"operations": []any{
					map[string]any{"op": "set_const", "field": "done", "value": true},
				},
			}},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "t"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
}

func mustNexaflowJSON(t *testing.T, rr *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, want, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rr.Body.String())
	}
	return out
}

func TestAdminNexaflowEndToEnd(t *testing.T) {
	s := testNexaflowServer(t)

	// Catalog + templates are readable.
	out := mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/nodes", "", true), http.StatusOK)
	nodes, _ := out["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("node catalog is empty")
	}
	var foundBranch bool
	for _, raw := range nodes {
		if raw.(map[string]any)["id"] == "nexaroute.control.branch" {
			foundBranch = true
		}
	}
	if !foundBranch {
		t.Fatal("node catalog lacks the branch node")
	}
	out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/templates", "", true), http.StatusOK)
	if out["inert"] != true {
		t.Fatalf("templates response inert = %v", out["inert"])
	}

	// Unauthenticated access is refused before any handler runs.
	if rr := nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/nodes", "", false); rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated nodes status = %d, want 401/403", rr.Code)
	}

	// Create -> validate -> publish -> execute -> poll -> history.
	defBody, _ := json.Marshal(nexaflowTestDef("wf-api"))
	mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions", string(defBody), true), http.StatusOK)
	out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/wf-api/validate", "{}", true), http.StatusOK)
	if out["valid"] != true {
		t.Fatalf("validate = %v", out)
	}
	out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/wf-api/publish", "{}", true), http.StatusOK)
	if ver, _ := out["version"].(map[string]any); ver["revision"] != float64(1) {
		t.Fatalf("publish version = %v", out["version"])
	}
	out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/wf-api/runs", `{"input":{"value":{"a":1}}}`, true), http.StatusAccepted)
	runID, _ := out["run_id"].(string)
	if runID == "" {
		t.Fatalf("run dispatch returned no run id: %v", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/runs/"+runID, "", true), http.StatusOK)
		run, _ := out["run"].(map[string]any)
		if run["status"] == "succeeded" {
			steps, _ := run["steps"].([]any)
			if len(steps) < 2 {
				t.Fatalf("run steps = %d, want start + transform", len(steps))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not succeed: %v", run["status"])
		}
		time.Sleep(20 * time.Millisecond)
	}
	out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/definitions/wf-api/runs", "", true), http.StatusOK)
	if runs, _ := out["runs"].([]any); len(runs) != 1 {
		t.Fatalf("definition runs = %d, want 1", len(runs))
	}
	out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/audit?limit=50", "", true), http.StatusOK)
	events, _ := out["events"].([]any)
	var started bool
	for _, raw := range events {
		ev := raw.(map[string]any)
		if ev["run_id"] == runID && ev["action"] == "run_started" {
			started = true
		}
	}
	if !started {
		t.Fatal("audit trail lacks run_started for the API run")
	}

	// Unknown definition: 404, not a handler panic.
	mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/nope/runs", `{"input":{}}`, true), http.StatusNotFound)
}

func TestAdminNexaflowUnattachedIs503(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "nexaflow-test-admin-key"
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/nexaflow/nodes", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unattached nodes status = %d, want 503, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAdminNexaflowApprovalSelfApprovalRefused(t *testing.T) {
	s := testNexaflowServer(t)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "wf-api-approval",
		Name:          "wf-api-approval",
		TenantScope:   "tenant-a",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "appr", NodeID: "nexaroute.control.approval", Version: nexaflow.SupportedVersion},
			{ID: "done", NodeID: "nexaroute.control.json_transform", Version: nexaflow.SupportedVersion, Input: map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}},
		},
		Edges: []nexaflow.DefinitionEdge{
			{ID: "e1", From: "start", To: "appr"},
			{ID: "e2", From: "appr", To: "done"},
		},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	defBody, _ := json.Marshal(def)
	mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions", string(defBody), true), http.StatusOK)
	mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/wf-api-approval/publish", "{}", true), http.StatusOK)
	out := mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/wf-api-approval/runs", `{"input":{}}`, true), http.StatusAccepted)
	runID, _ := out["run_id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		out = mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodGet, "/admin/api/nexaflow/runs/"+runID, "", true), http.StatusOK)
		if out["run"].(map[string]any)["status"] == "awaiting_approval" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never awaited approval: %v", out["run"].(map[string]any)["status"])
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The emergency-key identity is also the run's actor, so approving
	// through the same identity is self-approval and must be refused.
	rr := nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/runs/"+runID+"/approve", `{"decision":"approved"}`, true)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("self-approval status = %d, want 403, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAdminNexaflowWebhookEndpoint(t *testing.T) {
	t.Setenv("NEXAFLOW_API_TEST_HOOK_SECRET", "hook-secret-value")
	s := testNexaflowServer(t)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            "wf-api-webhook",
		Name:          "wf-api-webhook",
		TenantScope:   "tenant-a",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "wh", NodeID: "nexaroute.trigger.webhook", Version: nexaflow.SupportedVersion,
				Config: map[string]any{"secret_ref": "NEXAFLOW_API_TEST_HOOK_SECRET"}},
			{ID: "done", NodeID: "nexaroute.control.json_transform", Version: nexaflow.SupportedVersion, Input: map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "wh", To: "done"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "wh", Kind: nexaflow.TriggerWebhook, Config: map[string]any{"secret_ref": "NEXAFLOW_API_TEST_HOOK_SECRET"}}},
	}
	defBody, _ := json.Marshal(def)
	mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions", string(defBody), true), http.StatusOK)
	mustNexaflowJSON(t, nexaflowReq(t, s, http.MethodPost, "/admin/api/nexaflow/definitions/wf-api-webhook/publish", "{}", true), http.StatusOK)

	body := `{"hello":"world"}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("hook-secret-value"))
	mac.Write([]byte(ts + "." + body))
	sig := hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-api-webhook/wh", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-NexaFlow-Timestamp", ts)
	req.Header.Set("X-NexaFlow-Signature", sig)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	out := mustNexaflowJSON(t, rr, http.StatusOK)
	run, _ := out["run"].(map[string]any)
	if run["status"] != "succeeded" {
		t.Fatalf("webhook run status = %v", run["status"])
	}
	// Replay of the same delivery is refused.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://gateway/nexaflow/webhooks/wf-api-webhook/wh", strings.NewReader(body))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("X-NexaFlow-Timestamp", ts)
	req2.Header.Set("X-NexaFlow-Signature", sig)
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code == http.StatusOK {
		t.Fatal("webhook replay was accepted through the HTTP endpoint")
	}
}
