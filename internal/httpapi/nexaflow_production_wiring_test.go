package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Production NexaFlow wiring tests (issue #765). The delta over the
// existing Task 8 HTTP tests is the production store type: the real
// httpapi Server object with a file-backed service attached exactly
// as cmd/gateway attaches it (same AttachNexaflow call, same
// OpenFileStore), plus the Studio routes from PR #764 and restart
// persistence across two server objects sharing one store path.
// Run lifecycle/authz basics already covered against the memory
// store (TestTask8HTTPDefinitionRunLifecycle,
// TestTask8HTTPApprovalSeparationAndCancel) are not duplicated here.

func prodWiringServer(t *testing.T, storePath string) (*Server, string, *nexaflow.Service, *nexaflow.FileStore) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "prod-wiring-key"
	srv := testGateway(t, cfg)
	store, err := nexaflow.OpenFileStore(storePath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	reg := nexaflow.DefaultRegistry()
	// Combined wiring backend: loopback gateway client (no token ref in
	// these tests, so backend nodes fail closed at the loopback call)
	// plus a direct health/compat backend, exactly as cmd/gateway
	// constructs it. The dead-port base URL makes any backend call
	// fail fast with connection refused, never with a fake success.
	backend := &nexaflow.CompositeBackend{
		Gateway: &nexaflow.GatewayClient{
			BaseURL:        "http://127.0.0.1:1",
			SecretResolver: nexaflow.EnvSecretResolver,
		},
		HealthCompat: &nexaflow.DirectHealthCompatBackend{Health: nil},
	}
	engine, err := nexaflow.NewEngine(nexaflow.EngineOptions{Store: store, Registry: reg, Backend: backend})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := nexaflow.NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv.AttachNexaflow(svc)
	return srv, cfg.Admin.APIKey, svc, store
}

// prodWiringDo mirrors task8Do but takes an explicit loopback address:
// unauthenticated attempts cost the whole admin bucket for their
// address, so unauthenticated, wrong-key, and authenticated requests
// must each use a distinct loopback address or later requests answer
// 429 instead of their real verdict.
func prodWiringDo(t *testing.T, srv *Server, key, remoteAddr, method, path string, body any) *httptest.ResponseRecorder {
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
	req.RemoteAddr = remoteAddr
	if key != "" {
		req.Header.Set("x-admin-key", key)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNexaflowProductionWiringStudioRoutesOnFileStore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "nexaflow.json")
	srv, key, _, store := prodWiringServer(t, storePath)
	defer store.Close()

	// Auth boundary: unauthenticated and wrong-key both 401, each
	// from its own loopback address so the admin bucket is not
	// charged against the authenticated address.
	if rec := prodWiringDo(t, srv, "", "127.0.0.2:1234", http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth nodes = %d, want 401", rec.Code)
	}
	if rec := prodWiringDo(t, srv, "wrong-key", "127.0.0.3:1234", http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key nodes = %d, want 401", rec.Code)
	}
	// Studio page is static and always served; the API behind it is
	// what attachment gates.
	if rec := task8Do(t, srv, key, http.MethodGet, "/studio.html", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "NexaFlow Visual Studio") {
		t.Fatalf("studio page = %d, want 200 with studio title", rec.Code)
	}
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d, want 200", rec.Code)
	}
	// Draft-body validation: verdict is always 200; invalid answers
	// valid:false, never a 4xx.
	valid := task8BranchDef()
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/validate", map[string]any{"definition": valid}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("validate valid = %d: %s", rec.Code, rec.Body.String())
	}
	invalid := valid
	invalid.Nodes = append(append([]nexaflow.DefinitionNode{}, valid.Nodes...),
		nexaflow.DefinitionNode{ID: "nope", NodeID: "nexaroute.control.nope", Version: nexaflow.SupportedVersion})
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/validate", map[string]any{"definition": invalid}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":false`) {
		t.Fatalf("validate invalid = %d: %s", rec.Code, rec.Body.String())
	}
	// Template instantiation: a non-policy template instantiates
	// without policy approval; a policy template fails closed.
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/model-compat-evaluation/instantiate",
		map[string]any{"placeholders": map[string]any{"deployment_id": "d1", "suite_id": "s1"}, "policy_approved": false}); rec.Code != http.StatusCreated {
		t.Fatalf("instantiate non-policy = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/video-generation-pipeline/instantiate",
		map[string]any{"placeholders": map[string]any{"prompt": "x"}, "policy_approved": false}); rec.Code == http.StatusCreated {
		t.Fatalf("instantiate policy template without approval = %d, want rejection", rec.Code)
	}
	// The instantiated draft is visible through the definitions list.
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "model-compat-evaluation") {
		t.Fatalf("definitions list = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNexaflowProductionWiringRestartPersistsAcrossServers(t *testing.T) {
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "nexaflow.json")
	srv, key, _, store := prodWiringServer(t, storePath)
	def := task8BranchDef()
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-branch/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/http-branch/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "idempotency_key": "restart-run-1"})
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
	// Restart: close the store, reopen the same path behind a second
	// real server object, run startup recovery, and verify the draft
	// and the run survived.
	if err := store.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}
	srv2, key2, svc2, store2 := prodWiringServer(t, storePath)
	defer store2.Close()
	if err := svc2.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rec := task8Do(t, srv2, key2, http.MethodGet, "/admin/api/nexaflow/definitions", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "http-branch") {
		t.Fatalf("definitions after restart = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv2, key2, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("run after restart = %d: %s", rec.Code, rec.Body.String())
	}
	// A resumed terminal run is a no-op success, not a re-execution.
	if rec := task8Do(t, srv2, key2, http.MethodPost, "/admin/api/nexaflow/runs/"+runResp.ID+"/resume", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("resume terminal = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNexaflowProductionWiringLLMNodeWithoutTokenFailsClosed(t *testing.T) {
	ctx := context.Background()
	// Service-level, against the same file-backed service shape
	// production attaches, but with the combined loopback backend and
	// no token reference: an LLM node must fail at the loopback call,
	// never produce output.
	storePath := filepath.Join(t.TempDir(), "nexaflow.json")
	_, _, svc, store := prodWiringServer(t, storePath)
	defer store.Close()
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion, ID: "wf-llm", Name: "llm",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "chat", NodeID: "nexaroute.llm.chat", Version: nexaflow.SupportedVersion,
				Config: map[string]any{"route_or_alias": "test-route"}, Input: map[string]any{"prompt": "hello"}},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "chat"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	// The LLM node requires routing.read; a bare subject fails at the
	// permission check before any backend question arises, so the
	// requester carries the admin role here to reach the backend
	// question itself.
	requester := authz.Identity{Subject: "alice", Roles: []authz.Role{authz.RoleAdmin}}
	pub, err := svc.Publish(ctx, def.ID, requester)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run, err := svc.StartRun(ctx, pub, requester, map[string]any{"payload": map[string]any{}}, "llm-fail-closed")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// First drive parks at the operator-policy gate (as in the
	// canonical LLM scenario); no backend call happens yet.
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #1: %v", err)
	}
	if run.State != nexaflow.RunWaitingForApproval {
		t.Fatalf("state #1 = %q, want waiting_for_approval", run.State)
	}
	// A distinct approver releases the gate; with no token reference
	// the node must then fail at the loopback call, never as a fake
	// success.
	if _, err := svc.ApproveRun(ctx, run.ID, "chat", authz.Identity{Subject: "bob"}, "approved"); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #2: %v", err)
	}
	if run.State != nexaflow.RunFailed {
		t.Fatalf("state #2 = %q, want failed", run.State)
	}
	execs, err := store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	found := false
	for _, e := range execs {
		if e.NodeID == "chat" && (e.ErrorCode != "" || e.Error != "") {
			found = true
		}
		if e.NodeID == "chat" && e.Output != nil {
			if _, hasText := e.Output["text"]; hasText {
				t.Fatalf("chat node produced output without a token: %v", e.Output)
			}
		}
	}
	if !found {
		t.Fatalf("chat node lacks an error: %+v", execs)
	}
}
