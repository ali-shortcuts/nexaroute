package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Studio port: the standalone page is served from the embedded web
// assets, links from the dashboard, and drives only the canonical API
// (registry schemas, templates with placeholders, draft-body validate,
// server-side instantiate, versions, runs, approve/cancel) with RTL
// and onboarding controls present. Duplicate, rollback, and delete
// UI were deliberately not ported: they are not required studio
// flows, so the page must not reference them.
func TestStudioPortPageServed(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	srv := testGateway(t, cfg)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	page := get("/studio.html")
	if page.Code != http.StatusOK {
		t.Fatalf("studio.html = %d, want 200", page.Code)
	}
	for _, want := range []string{`/studio.js`, `/studio.css`, `id="canvas"`, `id="template-list"`, `id="run-input"`, `id="version-list"`, `id="dir-select"`, `id="onboarding"`, `id="btn-zoom-in"`, `id="zoom-label"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("studio.html missing %q", want)
		}
	}
	js := get("/studio.js")
	if js.Code != http.StatusOK {
		t.Fatalf("studio.js = %d, want 200", js.Code)
	}
	for _, want := range []string{
		"/admin/api/nexaflow/nodes",      // registry-driven schemas, no frontend copy
		"/admin/api/nexaflow/templates/", // gallery instantiate
		"/instantiate",
		"/admin/api/nexaflow/definitions/validate",
		"/versions/",
		"/approve",
		"/cancel",
		"x-admin-key",
		"X-NexaRoute-CSRF",
	} {
		if !strings.Contains(js.Body.String(), want) {
			t.Errorf("studio.js missing %q", want)
		}
	}
	// Dirty badge (G1) lives in the page markup, not the script.
	if !strings.Contains(page.Body.String(), "unsaved changes") {
		t.Error("studio.html missing dirty badge (G1)")
	}
	for _, banned := range []string{"tenant_id", "rollback", "btn-duplicate", "btn-delete", "from_port", "to_port"} {
		if strings.Contains(js.Body.String(), banned) {
			t.Errorf("studio.js must not contain %q (not ported / retired model)", banned)
		}
	}
	css := get("/studio.css")
	if css.Code != http.StatusOK {
		t.Fatalf("studio.css = %d, want 200", css.Code)
	}
	if !strings.Contains(css.Body.String(), "@media (max-width: 700px)") {
		t.Error("studio.css missing narrow-viewport breakpoint (G4)")
	}
	appJS, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appJS), `href="/studio.html"`) {
		t.Error("dashboard workflows view does not link to the studio")
	}
	if strings.Contains(string(appJS), "canvas repoint is integration Phase 6 work") {
		t.Error("dashboard still carries the stale Phase 6 studio note")
	}
}

func TestStudioPortNodesExposeSchemas(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/nodes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("nodes decode: %v", err)
	}
	var manual map[string]any
	for _, n := range body.Nodes {
		if n["id"] == "nexaroute.trigger.manual" {
			manual = n
		}
	}
	if manual == nil {
		t.Fatal("nodes lacks nexaroute.trigger.manual")
	}
	for _, field := range []string{"input_schema", "output_schema", "config_schema", "permission", "side_effect", "requires_policy", "default_timeout_ms", "max_timeout_ms", "max_input_bytes", "max_output_bytes"} {
		if _, ok := manual[field]; !ok {
			t.Errorf("node view lacks %q", field)
		}
	}
}

func TestStudioPortTemplatesExposePlaceholders(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/templates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("templates = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Templates []map[string]any `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("templates decode: %v", err)
	}
	for _, tpl := range body.Templates {
		if tpl["id"] == "scheduled-operational-report" {
			ph, ok := tpl["placeholders"].([]any)
			if !ok || len(ph) != 2 {
				t.Fatalf("scheduled-operational-report placeholders = %v, want 2", tpl["placeholders"])
			}
			return
		}
	}
	t.Fatal("templates lacks scheduled-operational-report")
}

// Draft-body validation (G1): the verdict must reflect the submitted
// definition, not any saved draft — no save happens in this test at all.
func TestStudioPortValidateDraftBody(t *testing.T) {
	srv, key := task8HTTPServer(t)
	triggerless := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion, ID: "v1", Name: "v1",
		Nodes: []nexaflow.DefinitionNode{
			{ID: "m", NodeID: "nexaroute.cost.usage", Version: nexaflow.SupportedVersion},
		},
	}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/validate",
		map[string]any{"definition": triggerless})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate triggerless = %d: %s", rec.Code, rec.Body.String())
	}
	var verdict struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verdict); err != nil {
		t.Fatalf("verdict decode: %v", err)
	}
	if verdict.Valid || len(verdict.Errors) == 0 {
		t.Fatalf("triggerless verdict = %+v, want invalid with errors", verdict)
	}
	// The same id, now with a trigger, validates — proving the verdict
	// follows the submitted body, not a stale draft.
	valid := triggerless
	valid.Nodes = append([]nexaflow.DefinitionNode{
		{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
	}, valid.Nodes...)
	valid.Edges = []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "m"}}
	valid.Triggers = []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}}
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/validate",
		map[string]any{"definition": valid})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate valid = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verdict); err != nil {
		t.Fatalf("verdict decode: %v", err)
	}
	if !verdict.Valid {
		t.Fatalf("valid definition verdict = %+v, want valid", verdict)
	}
}

// Regression: a draft that has never been saved has no id or name
// yet (both are assigned by the save dialog). Draft-body validation
// must judge the graph, not the missing identity — otherwise Validate
// before first Save can never pass.
func TestStudioPortValidateUnsavedDraft(t *testing.T) {
	srv, key := task8HTTPServer(t)
	prospective := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion,
		Nodes: []nexaflow.DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
			{ID: "m", NodeID: "nexaroute.cost.usage", Version: nexaflow.SupportedVersion},
		},
		Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "m"}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
	}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/validate",
		map[string]any{"definition": prospective})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate unsaved = %d: %s", rec.Code, rec.Body.String())
	}
	var verdict struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verdict); err != nil {
		t.Fatalf("verdict decode: %v", err)
	}
	if !verdict.Valid {
		t.Fatalf("unsaved draft verdict = %+v, want valid", verdict)
	}
}

func TestStudioPortInstantiate(t *testing.T) {
	srv, key := task8HTTPServer(t)
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/model-compat-evaluation/instantiate",
		map[string]any{"placeholders": map[string]string{"deployment_id": "d1", "suite_id": "s1"}, "policy_approved": false})
	if rec.Code != http.StatusCreated {
		t.Fatalf("instantiate = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("instantiate decode: %v", err)
	}
	if !strings.HasPrefix(created.ID, "tpl-model-compat-evaluation-") {
		t.Fatalf("instantiate id = %q, want tpl- prefix", created.ID)
	}
	// The created draft is retrievable through the canonical API.
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/"+created.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get instantiated draft = %d: %s", rec.Code, rec.Body.String())
	}
	// Policy-gated template without approval fails; unknown template 404s.
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/video-generation-pipeline/instantiate",
		map[string]any{"placeholders": map[string]string{"prompt": "x"}, "policy_approved": false})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("policy instantiate without approval = %d, want 400", rec.Code)
	}
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/no-such/instantiate",
		map[string]any{"placeholders": map[string]string{}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown template instantiate = %d, want 404", rec.Code)
	}
	// Wrong shape under the subtree 404s; wrong method 405s.
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/model-compat-evaluation/delete", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown template sub-shape = %d, want 404", rec.Code)
	}
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/templates/model-compat-evaluation/instantiate", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("instantiate GET = %d, want 405", rec.Code)
	}
}

// Runs list honours ?definition_id= so the studio runs panel shows
// only the open definition's runs.
func TestStudioPortRunsDefinitionFilter(t *testing.T) {
	srv, key := task8HTTPServer(t)
	mk := func(id string) {
		def := nexaflow.Definition{
			SchemaVersion: nexaflow.DefinitionSchemaVersion, ID: id, Name: id,
			Nodes: []nexaflow.DefinitionNode{
				{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion},
				{ID: "appr", NodeID: "nexaroute.control.approval", Version: nexaflow.SupportedVersion},
			},
			Edges:    []nexaflow.DefinitionEdge{{ID: "e1", From: "start", To: "appr"}},
			Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual}},
		}
		if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions", def); rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", id, rec.Code, rec.Body.String())
		}
		if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/"+id+"/publish", nil); rec.Code != http.StatusOK {
			t.Fatalf("publish %s = %d: %s", id, rec.Code, rec.Body.String())
		}
		if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/"+id+"/runs",
			map[string]any{"input": map[string]any{}, "idempotency_key": id + "-run-1"}); rec.Code != http.StatusCreated {
			t.Fatalf("run %s = %d: %s", id, rec.Code, rec.Body.String())
		}
	}
	mk("filter-a")
	mk("filter-b")
	var list struct {
		Runs []map[string]any `json:"runs"`
	}
	rec := task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs?definition_id=filter-a", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("filtered runs decode: %v", err)
	}
	if len(list.Runs) != 1 || list.Runs[0]["definition_id"] != "filter-a" {
		t.Fatalf("filtered runs = %v, want exactly filter-a", list.Runs)
	}
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs?definition_id=filter-none", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("empty runs decode: %v", err)
	}
	if len(list.Runs) != 0 {
		t.Fatalf("unknown definition runs = %d, want 0", len(list.Runs))
	}
}

// Redaction: run detail must never echo the run input (here carrying
// a marker string) and node views must not carry node inputs.
func TestStudioPortRunDetailRedaction(t *testing.T) {
	srv, key := task8HTTPServer(t)
	def := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion, ID: "redact", Name: "redact",
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
	if rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/redact/publish", nil); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec := task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/definitions/redact/runs",
		map[string]any{"input": map[string]any{"payload": map[string]any{"marker": "SECRET_MARKER_X"}}, "idempotency_key": "redact-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}
	var runResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runResp); err != nil {
		t.Fatalf("run decode: %v", err)
	}
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/runs/"+runResp.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run detail = %d: %s", rec.Code, rec.Body.String())
	}
	// Boundary is field-level, not content-level: the manual trigger
	// passes run input verbatim, so its (redacted) output legitimately
	// carries the marker. What must never appear is a node input
	// field, a raw error field, or a run-level input field.
	var detail struct {
		Run        map[string]any   `json:"run"`
		Nodes      []map[string]any `json:"nodes"`
		Definition map[string]any   `json:"definition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("run detail decode: %v", err)
	}
	if _, ok := detail.Run["input"]; ok {
		t.Fatal("run summary carries an input field")
	}
	if _, ok := detail.Run["error"]; ok {
		t.Fatal("run summary carries a raw error field")
	}
	for _, n := range detail.Nodes {
		if _, ok := n["input"]; ok {
			t.Fatalf("node view %v carries an input field", n["node_id"])
		}
		if _, ok := n["error"]; ok {
			t.Fatalf("node view %v carries a raw error field", n["node_id"])
		}
	}
	if defNodes, ok := detail.Definition["nodes"].([]any); ok {
		for _, raw := range defNodes {
			if m, ok := raw.(map[string]any); ok {
				if _, has := m["input"]; has {
					t.Fatalf("definition node %v carries an input mapping in this fixture", m["id"])
				}
			}
		}
	}
	if strings.Contains(rec.Body.String(), `"error":"`) {
		t.Fatal("run detail leaks raw error text")
	}
}

// Error matrix (G3): unauthenticated, wrong method, unknown shape,
// and malformed body all answer with generic codes and no backend
// detail. A 403 is not reachable under the interim admin-key mapping
// (the admin key holds config.read/config.write), so it is not faked
// here; it becomes testable when workflow.* permissions land.
func TestStudioPortErrorMatrix(t *testing.T) {
	srv, key := task8HTTPServer(t)
	// 401: no admin key (distinct loopback address, as task8Do does
	// for unauthenticated attempts, so the admin bucket is untouched).
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/nexaflow/definitions/validate", strings.NewReader(`{"definition":{}}`))
	req.RemoteAddr = "127.0.0.2:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("validate without key = %d, want 401", rec.Code)
	}
	// 405: wrong method on validate.
	rec = task8Do(t, srv, key, http.MethodGet, "/admin/api/nexaflow/definitions/validate", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("validate GET = %d, want 405", rec.Code)
	}
	// 404: unknown template shape.
	rec = task8Do(t, srv, key, http.MethodPost, "/admin/api/nexaflow/templates/x/unknown", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown template shape = %d, want 404", rec.Code)
	}
	// 400: malformed JSON body, with no backend detail leaked.
	req = httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/nexaflow/definitions/validate", strings.NewReader(`{bad json`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("x-admin-key", key)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed validate body = %d, want 400", rec.Code)
	}
	for _, leak := range []string{"DSN", "postgres", "/home/", "panic"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("error body leaks %q: %s", leak, rec.Body.String())
		}
	}
}
