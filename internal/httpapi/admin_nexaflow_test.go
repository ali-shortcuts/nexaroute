package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func nexaflowTestClient(t *testing.T) (*Server, config.Config, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	// Explicit key: keyless emergency mode requires a loopback Host,
	// and these requests use host "gateway".
	cfg.Admin.APIKey = "test-nexaflow-admin-key"
	srv := testGateway(t, cfg)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		req := httptest.NewRequest(method, "http://gateway"+path, reader)
		req.RemoteAddr = "127.0.0.1:10001"
		req.Header.Set("x-admin-key", cfg.Admin.APIKey)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	return srv, cfg, do
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %d body %s: %v", rr.Code, rr.Body.String(), err)
	}
}

const nexaflowPureDraft = `{"nodes":[
	{"id":"t1","type":"nexaroute.trigger.manual","position":{"x":10,"y":10}},
	{"id":"x1","type":"nexaroute.control.json_transform","position":{"x":200,"y":10}},
	{"id":"c1","type":"nexaroute.control.condition","position":{"x":400,"y":10},"config":{"field":"a","operator":"eq","expected":"1"}}
],"edges":[
	{"id":"e1","from":"t1","from_port":"payload","to":"x1","to_port":"value"},
	{"id":"e2","from":"x1","from_port":"value","to":"c1","to_port":"value"}
],"inputs":["operations"]}`

func nexaflowCreatePublished(t *testing.T, do func(string, string, string) *httptest.ResponseRecorder, name, draft string) string {
	t.Helper()
	nameJSON, _ := json.Marshal(name)
	rr := do(http.MethodPost, "/admin/api/nexaflow/workflows", `{"name":`+string(nameJSON)+`}`)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	decodeBody(t, rr, &created)
	id := created["id"].(string)
	rr = do(http.MethodPut, "/admin/api/nexaflow/workflows/"+id, `{"draft":`+draft+`}`)
	if rr.Code != 200 {
		t.Fatalf("save draft: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/publish", "")
	if rr.Code != 200 {
		t.Fatalf("publish: %d %s", rr.Code, rr.Body.String())
	}
	return id
}

func nexaflowPollRun(t *testing.T, do func(string, string, string) *httptest.ResponseRecorder, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rr := do(http.MethodGet, "/admin/api/nexaflow/runs/"+id, "")
		if rr.Code != 200 {
			t.Fatalf("get run: %d %s", rr.Code, rr.Body.String())
		}
		var run map[string]any
		decodeBody(t, rr, &run)
		if run["status"] == want {
			return run
		}
		if st := run["status"].(string); st == "succeeded" || st == "failed" || st == "cancelled" {
			t.Fatalf("run ended %s, want %s: %s", st, want, rr.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", id, want)
	return nil
}

func TestNexaflowNodesRequireAuthAndExposeRegistry(t *testing.T) {
	srv, _, do := nexaflowTestClient(t)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/nexaflow/nodes", nil)
	// Distinct loopback bucket: the failed attempt must not consume
	// the live client's admin-rate-limit allowance.
	req.RemoteAddr = "127.0.0.2:1234"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated nodes status=%d, want 401", rr.Code)
	}
	rr = do(http.MethodGet, "/admin/api/nexaflow/nodes", "")
	if rr.Code != 200 {
		t.Fatalf("nodes: %d %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Version string `json:"version"`
		Nodes   []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
		} `json:"nodes"`
	}
	decodeBody(t, rr, &payload)
	if payload.Version != "v1" || len(payload.Nodes) != 23 {
		t.Fatalf("registry version=%q nodes=%d, want v1/23", payload.Version, len(payload.Nodes))
	}
}

// Regression: a freshly created workflow has an empty graph, which used
// to serialize as JSON null slices. The browser editor pushes onto
// draft.nodes, so null there crashed node creation (pageerror "Cannot
// read properties of null (reading 'push')", found by the browser test).
func TestNexaflowEmptyGraphSerializesAsArrays(t *testing.T) {
	_, _, do := nexaflowTestClient(t)
	rr := do(http.MethodPost, "/admin/api/nexaflow/workflows", `{"name":"empty-graph"}`)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	decodeBody(t, rr, &created)
	rr = do(http.MethodGet, "/admin/api/nexaflow/workflows/"+created["id"].(string), "")
	if rr.Code != 200 {
		t.Fatalf("detail: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Inputs is omitempty on the Graph type, so an empty inputs list is
	// absent rather than []; the editor normalizes that. Nodes/edges
	// must be real arrays, never null.
	for _, want := range []string{`"nodes":[]`, `"edges":[]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"nodes":null`) || strings.Contains(body, `"edges":null`) {
		t.Fatalf("detail body has null graph slices: %s", body)
	}
}

func TestNexaflowWorkflowLifecycleOverHTTP(t *testing.T) {
	_, _, do := nexaflowTestClient(t)
	id := nexaflowCreatePublished(t, do, "http-lifecycle", nexaflowPureDraft)

	// Validate endpoint.
	rr := do(http.MethodGet, "/admin/api/nexaflow/workflows/"+id+"/validate", "")
	if rr.Code != 200 {
		t.Fatalf("validate: %d %s", rr.Code, rr.Body.String())
	}
	var validation map[string]any
	decodeBody(t, rr, &validation)
	if validation["valid"] != true {
		t.Fatalf("expected valid draft: %s", rr.Body.String())
	}

	// Versions carry a content hash and the published graph.
	rr = do(http.MethodGet, "/admin/api/nexaflow/workflows/"+id+"/versions", "")
	var versions struct {
		Versions []struct {
			Number      int    `json:"number"`
			ContentHash string `json:"content_hash"`
		} `json:"versions"`
	}
	decodeBody(t, rr, &versions)
	if len(versions.Versions) != 1 || versions.Versions[0].ContentHash == "" {
		t.Fatalf("versions payload: %s", rr.Body.String())
	}

	// Run the published version end to end.
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/runs",
		`{"input":{"payload":{"a":1,"b":2},"operations":[{"op":"pick","field":"a"}]}}`)
	if rr.Code != 201 {
		t.Fatalf("start run: %d %s", rr.Code, rr.Body.String())
	}
	var started map[string]any
	decodeBody(t, rr, &started)
	run := nexaflowPollRun(t, do, started["id"].(string), "succeeded")
	nodes := run["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("run nodes: %v", run["nodes"])
	}
	// Editing the draft must not change the run's snapshot: the run
	// already finished from version 1, and a fresh run still uses it.
	edited := strings.Replace(nexaflowPureDraft, `"expected":"1"`, `"expected":"999"`, 1)
	rr = do(http.MethodPut, "/admin/api/nexaflow/workflows/"+id, `{"draft":`+edited+`}`)
	if rr.Code != 200 {
		t.Fatalf("edit draft: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/runs",
		`{"input":{"payload":{"a":1,"b":2},"operations":[{"op":"pick","field":"a"}]}}`)
	var started2 map[string]any
	decodeBody(t, rr, &started2)
	run2 := nexaflowPollRun(t, do, started2["id"].(string), "succeeded")
	if run2["version_number"].(float64) != 1 {
		t.Fatalf("fresh run used version %v, want 1 (draft edits must not move runs)", run2["version_number"])
	}
}

func TestNexaflowPublishRejectsInvalidGraph(t *testing.T) {
	_, _, do := nexaflowTestClient(t)
	rr := do(http.MethodPost, "/admin/api/nexaflow/workflows", `{"name":"bad-graph"}`)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	decodeBody(t, rr, &created)
	id := created["id"].(string)
	// Condition node without config and without an edge for its
	// required input: publish must fail with structured issues.
	rr = do(http.MethodPut, "/admin/api/nexaflow/workflows/"+id,
		`{"draft":{"nodes":[{"id":"c1","type":"nexaroute.control.condition","position":{"x":0,"y":0}}],"edges":[]}}`)
	if rr.Code != 200 {
		t.Fatalf("save invalid draft: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/publish", "")
	if rr.Code != 422 {
		t.Fatalf("publish invalid: %d %s, want 422", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	decodeBody(t, rr, &payload)
	if payload["issues"] == nil {
		t.Fatalf("publish error lacks issues: %s", rr.Body.String())
	}
}

func TestNexaflowApprovalAndCancelOverHTTP(t *testing.T) {
	_, _, do := nexaflowTestClient(t)
	approvalDraft := `{"nodes":[
		{"id":"t1","type":"nexaroute.trigger.manual","position":{"x":10,"y":10}},
		{"id":"a1","type":"nexaroute.control.approval","position":{"x":200,"y":10}}
	],"edges":[]}`
	id := nexaflowCreatePublished(t, do, "http-approval", approvalDraft)
	rr := do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/runs", `{"input":{}}`)
	var started map[string]any
	decodeBody(t, rr, &started)
	nexaflowPollRun(t, do, started["id"].(string), "awaiting_approval")
	// This test client authenticates every call as the same admin
	// identity, so the approver IS the run starter: Phase 7's
	// separation-of-duties rule must reject it. The positive path
	// (distinct approver) is covered at the runtime layer, where two
	// identities can be expressed directly.
	rr = do(http.MethodPost, "/admin/api/nexaflow/runs/"+started["id"].(string)+"/approve", `{"decision":"approved"}`)
	if rr.Code != 200 {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	failed := nexaflowPollRun(t, do, started["id"].(string), "failed")
	if !strings.Contains(failed["error"].(string), "must not be the requester") {
		t.Fatalf("expected self-approval failure, got %v", failed["error"])
	}

	// Cancel path: a long wait run.
	waitDraft := `{"nodes":[
		{"id":"t1","type":"nexaroute.trigger.manual","position":{"x":10,"y":10}},
		{"id":"w1","type":"nexaroute.control.wait","position":{"x":200,"y":10}}
	],"edges":[],"inputs":["duration_ms"]}`
	id2 := nexaflowCreatePublished(t, do, "http-cancel", waitDraft)
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id2+"/runs", `{"input":{"duration_ms":120000}}`)
	var started2 map[string]any
	decodeBody(t, rr, &started2)
	time.Sleep(200 * time.Millisecond)
	rr = do(http.MethodPost, "/admin/api/nexaflow/runs/"+started2["id"].(string)+"/cancel", "")
	if rr.Code != 200 {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	nexaflowPollRun(t, do, started2["id"].(string), "cancelled")
}

func TestNexaflowRenameDuplicateDisableDeleteOverHTTP(t *testing.T) {
	_, _, do := nexaflowTestClient(t)
	id := nexaflowCreatePublished(t, do, "http-ops", nexaflowPureDraft)
	rr := do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/rename", `{"name":"http-renamed"}`)
	if rr.Code != 200 {
		t.Fatalf("rename: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/duplicate", "")
	if rr.Code != 201 {
		t.Fatalf("duplicate: %d %s", rr.Code, rr.Body.String())
	}
	var dup map[string]any
	decodeBody(t, rr, &dup)
	if dup["id"] == id || dup["published_version"].(float64) != 0 {
		t.Fatalf("duplicate must be a fresh unpublished workflow: %s", rr.Body.String())
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/enabled", `{"enabled":false}`)
	if rr.Code != 200 {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/workflows/"+id+"/runs", `{"input":{}}`)
	if rr.Code != 409 {
		t.Fatalf("disabled run: %d %s, want 409", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodDelete, "/admin/api/nexaflow/workflows/"+id, "")
	if rr.Code != 200 {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodGet, "/admin/api/nexaflow/workflows/"+id, "")
	if rr.Code != 404 {
		t.Fatalf("deleted workflow: %d, want 404", rr.Code)
	}
}

// Task 6: the template gallery is backend-driven. It must serve the
// inert starter templates (never published, no secrets, no resolved
// URLs), reject non-GET methods, and require authentication like the
// node registry it sits beside.
func TestNexaflowTemplatesEndpoint(t *testing.T) {
	srv, _, do := nexaflowTestClient(t)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/nexaflow/templates", nil)
	req.RemoteAddr = "127.0.0.2:1234"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated templates status=%d, want 401", rr.Code)
	}
	rr = do(http.MethodPost, "/admin/api/nexaflow/templates", "")
	if rr.Code != 405 {
		t.Fatalf("templates POST: %d, want 405", rr.Code)
	}
	rr = do(http.MethodGet, "/admin/api/nexaflow/templates", "")
	if rr.Code != 200 {
		t.Fatalf("templates: %d %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Version   string `json:"version"`
		Templates []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Nodes []struct {
				NodeID string         `json:"node_id"`
				Config map[string]any `json:"config"`
			} `json:"nodes"`
			Placeholders   []string `json:"placeholders"`
			RequiresPolicy bool     `json:"requires_policy"`
			Published      bool     `json:"published"`
		} `json:"templates"`
	}
	decodeBody(t, rr, &payload)
	if payload.Version != "v1" || len(payload.Templates) != 5 {
		t.Fatalf("templates version=%q count=%d, want v1/5", payload.Version, len(payload.Templates))
	}
	for _, tmpl := range payload.Templates {
		if tmpl.Published {
			t.Fatalf("template %s must never be published", tmpl.ID)
		}
		if len(tmpl.Nodes) == 0 {
			t.Fatalf("template %s has no nodes", tmpl.ID)
		}
		if tmpl.Placeholders == nil {
			t.Fatalf("template %s placeholders must be an array, not null", tmpl.ID)
		}
	}
	// No template config may carry a resolved secret-looking value:
	// placeholders stay placeholders until the operator fills them.
	if strings.Contains(rr.Body.String(), "Bearer ") || strings.Contains(rr.Body.String(), "sk-") {
		t.Fatalf("templates must not expose secret values: %s", rr.Body.String())
	}
}
