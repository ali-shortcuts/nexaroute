package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Production NexaFlow wiring (issue #765) acceptance through the real
// gateway server object: default-off proves no routes and no store
// side effects; opt-in proves routes mount, the studio page serves,
// template instantiation and draft validation work, and the file store
// survives a close/reopen. This mirrors the Task 8 HTTP precedent
// (internal/httpapi/admin_nexaflow_task8_test.go) without importing
// its unexported helpers.

// wiringTestServer builds a real httpapi.Server the same way the
// httpapi package's own testGateway does, so Handler() exercises the
// production route table, admin authentication, and static assets.
func wiringTestServer(t *testing.T, cfg config.Config) *httpapi.Server {
	t.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := httpapi.New(cfg, configDir+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	if err := srv.SecurityStoreError(); err != nil {
		t.Fatalf("create test security store: %v", err)
	}
	t.Cleanup(func() { _ = srv.CloseSecurityStore() })
	return srv
}

func wiringTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "wiring-admin-secret"
	return cfg
}

// wiringDo mirrors the Task 8 task8Do helper: authenticated requests
// come from loopback (BindLocalOnly), unauthenticated ones from a
// distinct loopback address so they do not burn the admin bucket.
func wiringDo(t *testing.T, srv *httpapi.Server, key, method, path string, body any) *httptest.ResponseRecorder {
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

func TestNexaflowWiringDefaultOff(t *testing.T) {
	cfg := wiringTestConfig(t)
	// Even a fully specified file store must stay untouched while off.
	storePath := filepath.Join(t.TempDir(), "nexaflow.json")
	cfg.Nexaflow.Store = "file"
	cfg.Nexaflow.StorePath = storePath
	srv := wiringTestServer(t, cfg)
	closer, err := attachNexaflow(cfg, srv, nil)
	if err != nil {
		t.Fatalf("attachNexaflow disabled: %v", err)
	}
	if closer != nil {
		t.Fatalf("disabled attach returned a closer, want nil")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("disabled attach created store file (err=%v)", err)
	}
	if rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled nodes = %d, want 404", rec.Code)
	}
	if rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/definitions", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled create = %d, want 404", rec.Code)
	}
	// Existing gateway behaviour is untouched.
	if rec := wiringDo(t, srv, "", http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz with nexaflow disabled = %d, want 200", rec.Code)
	}
}

func TestNexaflowWiringOptInExplicitMemory(t *testing.T) {
	cfg := wiringTestConfig(t)
	cfg.Nexaflow.Enabled = true
	cfg.Nexaflow.Store = "memory" // Explicit only: memory is never the silent default.
	srv := wiringTestServer(t, cfg)
	closer, err := attachNexaflow(cfg, srv, nil)
	if err != nil {
		t.Fatalf("attachNexaflow enabled: %v", err)
	}
	if closer != nil {
		t.Fatalf("memory attach returned a closer, want nil")
	}
	// Admin authentication still fronts every route.
	if rec := wiringDo(t, srv, "", http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated nodes = %d, want 401", rec.Code)
	}
	rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nexaroute.control.branch") {
		t.Fatalf("nodes body lacks branch node: %s", rec.Body.String())
	}
	// The studio page serves through the real server object.
	rec = wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/studio.html", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="admin-key"`) {
		t.Fatalf("studio = %d, admin-key field missing", rec.Code)
	}
	// Run listing works (empty, not an error) through the wired service.
	rec = wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/runs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("runs = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNexaflowWiringEmptyStoreDefaultsToFile(t *testing.T) {
	cfg := wiringTestConfig(t)
	cfg.Nexaflow.Enabled = true // Store empty: the durable file default.
	storePath := filepath.Join(t.TempDir(), "nexaflow.json")
	cfg.Nexaflow.StorePath = storePath
	srv := wiringTestServer(t, cfg)
	closer, err := attachNexaflow(cfg, srv, nil)
	if err != nil {
		t.Fatalf("attachNexaflow empty store: %v", err)
	}
	if closer == nil {
		t.Fatalf("empty-store attach returned nil closer, want the file closer")
	}
	t.Cleanup(func() { _ = closer() })
	// The file store creates its file lazily on first write, not on
	// open: instantiate once, then the durable file must exist. A
	// memory store would never create it, so this proves the default.
	rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/templates/model-health-alert/instantiate",
		map[string]any{"placeholders": map[string]string{"deployment_id": "probe-deployment"}, "policy_approved": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("instantiate = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("empty-store attach created no store file: %v", err)
	}
	rec = wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNexaflowWiringInstantiateAndDraftValidate(t *testing.T) {
	cfg := wiringTestConfig(t)
	cfg.Nexaflow.Enabled = true
	cfg.Nexaflow.Store = "memory" // Explicit: this test covers instantiate/draft-validate, not the default.
	srv := wiringTestServer(t, cfg)
	if _, err := attachNexaflow(cfg, srv, nil); err != nil {
		t.Fatalf("attachNexaflow: %v", err)
	}
	// Template instantiation through the real gateway server object.
	rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/templates/model-health-alert/instantiate",
		map[string]any{"placeholders": map[string]string{"deployment_id": "probe-deployment"}, "policy_approved": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("instantiate = %d: %s", rec.Code, rec.Body.String())
	}
	var instantiated struct {
		ID         string         `json:"id"`
		Definition map[string]any `json:"definition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &instantiated); err != nil {
		t.Fatalf("instantiate decode: %v", err)
	}
	if instantiated.ID == "" || instantiated.Definition == nil {
		t.Fatalf("instantiate lacks id/definition: %s", rec.Body.String())
	}
	// The instantiated draft is listed, proving it was saved.
	rec = wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/definitions", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), instantiated.ID) {
		t.Fatalf("definitions = %d, instantiated draft %q missing: %s", rec.Code, instantiated.ID, rec.Body.String())
	}
	// Draft-body validation: the instantiated definition validates.
	rec = wiringDo(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/definitions/validate",
		map[string]any{"definition": instantiated.Definition})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate = %d: %s", rec.Code, rec.Body.String())
	}
	var verdict struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verdict); err != nil {
		t.Fatalf("validate decode: %v", err)
	}
	if !verdict.Valid {
		t.Fatalf("instantiated definition did not validate: %v", verdict.Errors)
	}
	// An invalid definition answers valid:false with HTTP 200 (the
	// studio contract), never a stubbed success and never a 4xx that
	// the UI would conflate with a failed request.
	bad := map[string]any{}
	for k, v := range instantiated.Definition {
		bad[k] = v
	}
	bad["nodes"] = []any{map[string]any{"id": "nope", "node_id": "nexaroute.no.such.node", "version": "1"}}
	rec = wiringDo(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/definitions/validate",
		map[string]any{"definition": bad})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate invalid = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verdict); err != nil {
		t.Fatalf("validate invalid decode: %v", err)
	}
	if verdict.Valid || len(verdict.Errors) == 0 {
		t.Fatalf("unknown-node definition reported valid: %s", rec.Body.String())
	}
}

func TestNexaflowWiringFileStoreSurvivesReopen(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "nexaflow.json")
	cfg := wiringTestConfig(t)
	cfg.Nexaflow.Enabled = true
	cfg.Nexaflow.Store = "file"
	cfg.Nexaflow.StorePath = storePath
	srv := wiringTestServer(t, cfg)
	closer, err := attachNexaflow(cfg, srv, nil)
	if err != nil {
		t.Fatalf("attachNexaflow file: %v", err)
	}
	if closer == nil {
		t.Fatalf("file attach returned nil closer")
	}
	rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodPost, "/admin/api/nexaflow/templates/model-health-alert/instantiate",
		map[string]any{"placeholders": map[string]string{"deployment_id": "probe-deployment"}, "policy_approved": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("instantiate = %d: %s", rec.Code, rec.Body.String())
	}
	var instantiated struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &instantiated); err != nil {
		t.Fatalf("instantiate decode: %v", err)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("file store was not created: %v", err)
	}
	// A second live store on the same path must fail (single-writer
	// flock), proving the first store actually holds the lock.
	if _, err := attachNexaflow(cfg, wiringTestServer(t, cfg), nil); err == nil {
		t.Fatalf("second live file store opened, want flock failure")
	}
	if err := closer(); err != nil {
		t.Fatalf("close file store: %v", err)
	}
	// Reopen: the instantiated draft survives the restart.
	srv2 := wiringTestServer(t, cfg)
	closer2, err := attachNexaflow(cfg, srv2, nil)
	if err != nil {
		t.Fatalf("reattach file store: %v", err)
	}
	defer func() { _ = closer2() }()
	rec = wiringDo(t, srv2, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/definitions", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), instantiated.ID) {
		t.Fatalf("definitions after reopen = %d, draft %q missing: %s", rec.Code, instantiated.ID, rec.Body.String())
	}
}

func TestNexaflowWiringInvalidStoreFailsClosed(t *testing.T) {
	cfg := wiringTestConfig(t)
	cfg.Nexaflow.Enabled = true
	cfg.Nexaflow.Store = "postgres" // Deliberately not offered.
	srv := wiringTestServer(t, cfg)
	if _, err := attachNexaflow(cfg, srv, nil); err == nil {
		t.Fatalf("postgres store attached, want failure")
	}
	if rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nodes after failed attach = %d, want 404", rec.Code)
	}
	cfg.Nexaflow.Store = "file" // No path: fails closed, attaches nothing.
	cfg.Nexaflow.StorePath = ""
	if _, err := attachNexaflow(cfg, srv, nil); err == nil {
		t.Fatalf("file store without path attached, want failure")
	}
	cfg.Nexaflow.Store = "" // Empty means the file default: no path, no attach.
	cfg.Nexaflow.StorePath = ""
	if _, err := attachNexaflow(cfg, srv, nil); err == nil {
		t.Fatalf("empty store without path attached, want failure")
	}
	if rec := wiringDo(t, srv, cfg.Admin.APIKey, http.MethodGet, "/admin/api/nexaflow/nodes", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nodes after failed attach = %d, want 404", rec.Code)
	}
}
