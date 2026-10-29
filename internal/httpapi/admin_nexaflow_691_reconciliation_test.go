package httpapi

// HTTP-layer reconciliation tests for PR #691 against the canonical
// Task 8 admin contract (admin_nexaflow_task8.go). #691's own
// admin_nexaflow.go was retired; these tests pin the routes ported
// onto the canonical handler: resume, validate, per-definition runs
// listing, idempotency/conflict mapping, run-limit mapping, and the
// signed-webhook rejection mapping. Ownership denial itself is
// proven at the service layer (the admin-key identity used here is
// an owner); here, unauthenticated and wrong-method attempts must
// still be refused.

import (
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

	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

func reconHTTPDef(id string, limit int) nexaflow.Definition {
	node := func(nid, nodeID string, cfg, input map[string]any) nexaflow.DefinitionNode {
		return nexaflow.DefinitionNode{ID: nid, NodeID: nodeID, Version: nexaflow.SupportedVersion, Config: cfg, Input: input}
	}
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		ID:            id, Name: id,
		Nodes: []nexaflow.DefinitionNode{
			node("start", "nexaroute.trigger.manual", nil, nil),
			node("appr", "nexaroute.control.approval", nil, nil),
			node("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value":      map[string]any{"lane": "done"},
				"operations": []any{map[string]any{"op": "set_const", "field": "done", "value": true}},
			}),
		},
		Edges: []nexaflow.DefinitionEdge{
			{ID: "e1", From: "start", To: "appr"},
			{ID: "e2", From: "appr", To: "done"},
		},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	def.Settings.MaxConcurrentRuns = limit
	return def
}

func reconHTTPStart(t *testing.T, srv *Server, key, defID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/"+defID+"/runs", body)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return rec.Code, out
}

func TestReconciliationHTTPResumeValidateRuns(t *testing.T) {
	srv, key := task8HTTPServer(t)
	ctx := context.Background()
	def := reconHTTPDef("recon-http", 0)
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create definition = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/recon-http/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	// Validate the stored draft: valid.
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/recon-http/validate", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("validate valid = %d: %s", rec.Code, rec.Body.String())
	}
	// Start a run: it parks at the approval node (non-terminal).
	code, run := reconHTTPStart(t, srv, key, "recon-http", map[string]any{"input": map[string]any{"payload": map[string]any{}}})
	if code != http.StatusCreated {
		t.Fatalf("start = %d: %v", code, run)
	}
	runID, _ := run["id"].(string)
	if runID == "" {
		t.Fatalf("start response has no run id: %v", run)
	}
	if run["state"] != "waiting_for_approval" {
		t.Fatalf("run state = %v, want waiting_for_approval", run["state"])
	}
	// Per-definition runs listing (ported from #691) shows the run.
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/recon-http/runs", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), runID) {
		t.Fatalf("definition runs listing = %d: %s", rec.Code, rec.Body.String())
	}
	// Resume is ownership-checked and re-drives: the run is still
	// waiting (no approval recorded), so resume leaves it waiting
	// rather than failing or completing it.
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/"+runID+"/resume", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume waiting = %d: %s", rec.Code, rec.Body.String())
	}
	// Wrong method and unauthenticated attempts are refused.
	if rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/"+runID+"/resume", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("resume GET = %d, want 405", rec.Code)
	}
	if rec := task8Do(t, srv, "", http.MethodPost, "/admin/api/nexaflow/runs/"+runID+"/resume", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("resume unauthenticated = %d, want 401", rec.Code)
	}
	// Parked run via direct store crafting: resume must answer 409,
	// never revive the run.
	svc := srv.nexaflow
	if svc == nil {
		t.Fatal("no nexaflow service attached")
	}
	parked := nexaflow.Run{ID: "recon-parked", DefinitionID: "recon-http", Definition: def, State: nexaflow.RunNeedsManualAction}
	if err := svc.Store.CreateRun(ctx, parked); err != nil {
		t.Fatalf("seed parked run: %v", err)
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/runs/recon-parked/resume", nil); rec.Code != http.StatusConflict {
		t.Fatalf("resume parked = %d, want 409", rec.Code)
	}
	// Validate an invalid draft seeded directly in the store (the
	// service save path would have rejected it): 200 valid=false.
	bad := reconHTTPDef("recon-bad", 0)
	bad.Nodes = nil
	if err := svc.Store.SaveDraft(ctx, bad); err != nil {
		t.Fatalf("seed invalid draft: %v", err)
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/recon-bad/validate", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":false`) {
		t.Fatalf("validate invalid = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReconciliationHTTPIdempotencyAndLimit(t *testing.T) {
	srv, key := task8HTTPServer(t)
	def := reconHTTPDef("recon-idem", 1)
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/recon-idem/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	input := map[string]any{"payload": map[string]any{}}
	code1, run1 := reconHTTPStart(t, srv, key, "recon-idem", map[string]any{"input": input, "idempotency_key": "http-idem"})
	if code1 != http.StatusCreated {
		t.Fatalf("start #1 = %d: %v", code1, run1)
	}
	// Identical replay: same run id.
	code2, run2 := reconHTTPStart(t, srv, key, "recon-idem", map[string]any{"input": input, "idempotency_key": "http-idem"})
	if code2 != http.StatusCreated || run2["id"] != run1["id"] {
		t.Fatalf("idempotent replay = %d %v, want 201 same id %v", code2, run2, run1["id"])
	}
	// Same key, different input: 409, never a silent replay.
	code3, _ := reconHTTPStart(t, srv, key, "recon-idem", map[string]any{"input": map[string]any{"payload": map[string]any{"other": true}}, "idempotency_key": "http-idem"})
	if code3 != http.StatusConflict {
		t.Fatalf("different-input replay = %d, want 409", code3)
	}
	// The first run still waits (limit 1 held): a second distinct
	// run is refused with 409 (ErrRunLimit mapping).
	code4, _ := reconHTTPStart(t, srv, key, "recon-idem", map[string]any{"input": input, "idempotency_key": "http-idem-2"})
	if code4 != http.StatusConflict {
		t.Fatalf("over-limit start = %d, want 409", code4)
	}
}

func TestReconciliationHTTPWebhookRejections(t *testing.T) {
	srv, key := task8HTTPServer(t)
	_ = key
	ledger := nexaflow.NewMemoryDeliveryLedger()
	starts := 0
	svc, err := nexaflow.NewWebhookService(nexaflow.NewWebhookVerifier(), ledger, func(ctx context.Context, defID, trigger string, body []byte) (string, error) {
		starts++
		return fmt.Sprintf("wh-run-%d", starts), nil
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	srv.AttachNexaflowWebhook(svc, map[string]string{"wf-1": "wh-secret"})
	sign := func(secret, ts, body string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "." + body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	doWebhook := func(ts, sig, body string) *httptest.ResponseRecorder {
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
	if rec := doWebhook(ts, sign("wh-secret", ts, body), body); rec.Code != http.StatusOK {
		t.Fatalf("valid webhook = %d: %s", rec.Code, rec.Body.String())
	}
	// Identical replay: 409, no second start.
	if rec := doWebhook(ts, sign("wh-secret", ts, body), body); rec.Code != http.StatusConflict || starts != 1 {
		t.Fatalf("replay = %d starts=%d, want 409/1", rec.Code, starts)
	}
	// Bad signature, tampered body, stale timestamp: 401 each,
	// indistinguishable from outside, none reaching Start.
	if rec := doWebhook(ts, sign("wrong-secret", ts, body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", rec.Code)
	}
	if rec := doWebhook(ts, sign("wh-secret", ts, body), `{"event":"pong"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body = %d, want 401", rec.Code)
	}
	stale := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	if rec := doWebhook(stale, sign("wh-secret", stale, body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp = %d, want 401", rec.Code)
	}
	if starts != 1 {
		t.Fatalf("starts after rejections = %d, want 1", starts)
	}
}
