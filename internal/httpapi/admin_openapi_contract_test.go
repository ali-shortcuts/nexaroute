package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// The Admin OpenAPI contract (M4-5): docs/api/admin-openapi.yaml must
// describe the API the server actually implements — same paths, same
// methods, same permissions — and the documented behavior (auth
// envelope, CSRF flow, audit limit) must hold against the real handler.
// Behavior wins over documentation: if a probe disagrees, fix the spec.
//
// Two different authorities are checked, on purpose:
//   - the fail-closed authorization matrix (adminPermissionForRequest)
//     is the CEILING: a documented operation the matrix denies, or with
//     a different permission, is a spec bug (and possibly a security
//     bug — the matrix never allows PATCH);
//   - the handlers are the METHOD authority: the set of methods a path
//     actually implements (everything that does not answer 405 or a
//     mux 404) must equal the documented set exactly.

type openAPIRef struct {
	Ref string `yaml:"$ref"`
}

type openAPISchemaProperty struct {
	Type    string     `yaml:"type"`
	Format  string     `yaml:"format"`
	Minimum float64    `yaml:"minimum"`
	Items   openAPIRef `yaml:"items"`
}

type openAPISchema struct {
	Type       string                           `yaml:"type"`
	Required   []string                         `yaml:"required"`
	Properties map[string]openAPISchemaProperty `yaml:"properties"`
}

type openAPIResponseContent struct {
	Schema openAPIRef `yaml:"schema"`
}

type openAPIResponse struct {
	Content map[string]openAPIResponseContent `yaml:"content"`
}

type openAPIOperation struct {
	Permission string                     `yaml:"x-permission"`
	Responses  map[string]openAPIResponse `yaml:"responses"`
}

type openAPIDoc struct {
	Paths      map[string]map[string]*openAPIOperation `yaml:"paths"`
	Components struct {
		Schemas map[string]openAPISchema `yaml:"schemas"`
	} `yaml:"components"`
}

func loadAdminOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api/admin-openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("spec has no paths")
	}
	return doc
}

var adminRoutePatternRE = regexp.MustCompile(`mux\.Handle(?:Func)?\("(/admin[^"]*)"`)

// registeredAdminPatterns returns every /admin route pattern registered
// in server.go — the only file that registers admin routes.
func registeredAdminPatterns(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range adminRoutePatternRE.FindAllStringSubmatch(string(src), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func normalizeAdminPath(p string) string {
	if strings.HasSuffix(p, "/") {
		return strings.TrimSuffix(p, "/") + "/{}"
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") {
			parts[i] = "{}"
		}
	}
	return strings.Join(parts, "/")
}

func specPathByNorm(doc openAPIDoc) map[string]string {
	out := map[string]string{}
	for p := range doc.Paths {
		out[normalizeAdminPath(p)] = p
	}
	return out
}

func sampleAdminPath(specPath string) string {
	p := strings.ReplaceAll(specPath, "{id}", "contractprobe")
	return strings.ReplaceAll(p, "{deployment}", "contractprobe")
}

func TestAdminOpenAPIClientKeyBudgetContract(t *testing.T) {
	doc := loadAdminOpenAPI(t)

	key, ok := doc.Components.Schemas["ClientKey"]
	if !ok {
		t.Fatal("spec is missing ClientKey schema")
	}
	budget, ok := key.Properties["budget_usd"]
	if !ok || budget.Type != "number" || budget.Format != "double" || budget.Minimum < 0 {
		t.Fatalf("ClientKey.budget_usd schema = %+v, want non-negative number/double", budget)
	}
	for _, required := range []string{"id", "budget_usd"} {
		found := false
		for _, field := range key.Required {
			if field == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ClientKey schema must require %q (budget_usd is always emitted; zero means unlimited)", required)
		}
	}

	list, ok := doc.Components.Schemas["ClientKeyList"]
	if !ok {
		t.Fatal("spec is missing ClientKeyList schema")
	}
	keys := list.Properties["keys"]
	if keys.Type != "array" || keys.Items.Ref != "#/components/schemas/ClientKey" {
		t.Fatalf("ClientKeyList.keys = %+v, want array of ClientKey", keys)
	}

	collection := doc.Paths["/admin/api/client-keys"]
	if collection["get"] == nil || collection["get"].Responses["200"].Content["application/json"].Schema.Ref != "#/components/schemas/ClientKeyList" {
		t.Error("GET /admin/api/client-keys must return ClientKeyList")
	}
	if collection["post"] == nil || collection["post"].Responses["201"].Content["application/json"].Schema.Ref != "#/components/schemas/ClientKey" {
		t.Error("POST /admin/api/client-keys must document its 201 Created ClientKey response")
	}
	if _, wrongStatus := collection["post"].Responses["200"]; wrongStatus {
		t.Error("POST /admin/api/client-keys returns 201 Created, not 200")
	}

	rotate := doc.Paths["/admin/api/client-keys/{id}/rotate"]
	if rotate["post"] == nil || rotate["post"].Responses["200"].Content["application/json"].Schema.Ref != "#/components/schemas/ClientKey" {
		t.Error("POST /admin/api/client-keys/{id}/rotate must return ClientKey")
	}
}

func TestAdminOpenAPISpecMatchesRegisteredRoutes(t *testing.T) {
	doc := loadAdminOpenAPI(t)
	spec := specPathByNorm(doc)
	registered := map[string]string{}
	for _, p := range registeredAdminPatterns(t) {
		registered[normalizeAdminPath(p)] = p
	}
	for norm, src := range registered {
		if _, ok := spec[norm]; !ok {
			t.Errorf("registered route %s has no spec path", src)
		}
	}
	for norm, sp := range spec {
		if _, ok := registered[norm]; ok {
			continue
		}
		// One handler-internal sub-path is legitimate: the client-keys
		// prefix handler dispatches {id}/rotate itself. It stays in the
		// spec only while that dispatch exists in the handler source.
		if sp == "/admin/api/client-keys/{id}/rotate" {
			src, err := os.ReadFile("client_keys.go")
			if err != nil || !strings.Contains(string(src), `parts[1] == "rotate"`) {
				t.Errorf("spec path %s exists but the client-keys handler no longer dispatches a rotate action", sp)
			}
			continue
		}
		t.Errorf("spec path %s is not a registered route (invented endpoint)", sp)
	}
}

func TestAdminOpenAPISpecWithinPermissionMatrix(t *testing.T) {
	doc := loadAdminOpenAPI(t)
	for path, ops := range doc.Paths {
		if !strings.HasPrefix(path, "/admin/api/") {
			continue
		}
		for method, operation := range ops {
			m := strings.ToUpper(method)
			if m == "PATCH" {
				t.Errorf("spec documents PATCH %s; the matrix never allows PATCH", path)
				continue
			}
			req := httptest.NewRequest(m, sampleAdminPath(path), nil)
			perm, ok := adminPermissionForRequest(req)
			if !ok {
				t.Errorf("spec documents %s %s but the authorization matrix denies it", m, path)
				continue
			}
			if operation == nil || operation.Permission != string(perm) {
				got := ""
				if operation != nil {
					got = operation.Permission
				}
				t.Errorf("%s %s: spec x-permission=%q, matrix=%q", m, path, got, perm)
			}
		}
		// PATCH must be denied by the matrix on every API path.
		req := httptest.NewRequest(http.MethodPatch, sampleAdminPath(path), nil)
		if _, ok := adminPermissionForRequest(req); ok {
			t.Errorf("matrix allows PATCH %s; expected fail-closed denial", path)
		}
	}
}

// adminProbe performs one in-process admin request with a full-power
// (RoleAdmin) session and, for mutations, the CSRF pair. The admin API
// rate-limits per remote IP (capacity 90, unauthenticated requests cost
// the whole bucket), so the probe rotates loopback IPs per path — the
// contract under test is the method set, not the limiter (which has
// its own suite in admin_throttle_test.go).
type adminProbe struct {
	handler http.Handler
	session string
	csrfTok string
	ip      string
}

func newAdminProbe(t *testing.T, srv *Server) *adminProbe {
	t.Helper()
	p := &adminProbe{handler: srv.Handler(), session: installTestOIDCSession(t, srv, authz.RoleAdmin), ip: "127.0.0.1"}
	rr := p.do(http.MethodGet, "/admin/api/csrf-token", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("csrf-token: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Token == "" {
		t.Fatalf("csrf-token body: %v", err)
	}
	p.csrfTok = body.Token
	return p
}

func (p *adminProbe) do(method, path string, csrf bool) *httptest.ResponseRecorder {
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPut {
		// Deliberately malformed JSON: handlers that decode a body
		// reject it with 400 before applying anything, so probing a
		// config-replacing endpoint cannot rewrite the server's config
		// (and with it the OIDC policy this session is bound to)
		// mid-test. Method routing is decided before body decoding.
		body = strings.NewReader("{contract-probe-invalid")
	}
	req := httptest.NewRequest(method, "http://gateway"+path, body)
	req.RemoteAddr = p.ip + ":10001"
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: p.session})
	if csrf {
		req.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: p.csrfTok})
		req.Header.Set("X-NexaRoute-CSRF", p.csrfTok)
	}
	rr := httptest.NewRecorder()
	p.handler.ServeHTTP(rr, req)
	return rr
}

// implemented reports whether method+path is part of the implemented
// API, from the answer a full-permission admin session receives:
//   - 405: the handler does not implement the method;
//   - the mux's plain-text 404: the route is not registered;
//   - 403 "admin permission denied": the authorization matrix does not
//     map this method on this path at all (the middleware denies it
//     before the handler runs).
//
// Anything else — including a JSON 404 ("resource missing") or a
// handler-level 403 — means the method reached the handler.
func implemented(rr *httptest.ResponseRecorder) bool {
	if rr.Code == http.StatusMethodNotAllowed {
		return false
	}
	if rr.Code == http.StatusNotFound && !strings.Contains(rr.Header().Get("Content-Type"), "json") {
		return false
	}
	if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "admin permission denied") {
		return false
	}
	return true
}

func TestAdminOpenAPIMethodSetsMatchHandlers(t *testing.T) {
	doc := loadAdminOpenAPI(t)
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}
	// A fresh server per path: several handlers legitimately mutate or
	// reset configuration (settings/client-keys DELETE, drain POST),
	// which would otherwise poison every later probe on the same server
	// (a config reset rebuilds the OIDC authenticator the test session
	// is bound to). Isolation keeps each method answer attributable to
	// its own handler.
	for _, pattern := range registeredAdminPatterns(t) {
		if !strings.HasPrefix(pattern, "/admin/api/") {
			continue
		}
		specPath := specPathByNorm(doc)[normalizeAdminPath(pattern)]
		if specPath == "" {
			continue // the routes test reports this
		}
		if specPath == "/admin/api/events/stream" {
			continue // covered by TestAdminOpenAPIEventStream below
		}
		srv := testGateway(t, config.Default())
		if strings.HasPrefix(pattern, "/admin/api/nexaflow/") {
			task8AttachProbeService(t, srv)
		}
		p := newAdminProbe(t, srv)
		for _, m := range methods {
			rr := p.do(m, sampleAdminPath(specPath), m != http.MethodGet)
			live := implemented(rr)
			_, documented := doc.Paths[specPath][strings.ToLower(m)]
			switch {
			case live && !documented:
				t.Errorf("%s %s is implemented (status %d) but not documented", m, specPath, rr.Code)
			case !live && documented:
				t.Errorf("%s %s is documented but the handler answers %d", m, specPath, rr.Code)
			}
		}
	}
}

func TestAdminOpenAPIClientKeyRotate(t *testing.T) {
	doc := loadAdminOpenAPI(t)
	const path = "/admin/api/client-keys/{id}/rotate"
	if _, ok := doc.Paths[path]["post"]; !ok {
		t.Fatal("spec does not document the rotate operation")
	}
	srv := testGateway(t, config.Default())
	p := newAdminProbe(t, srv)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := p.do(m, "/admin/api/client-keys/contractprobe/rotate", m != http.MethodGet)
		live := implemented(rr)
		if live != (m == http.MethodPost) {
			t.Errorf("%s %s: live=%v (status %d), want only POST implemented", m, path, live, rr.Code)
		}
	}
}

func TestAdminOpenAPIEventStream(t *testing.T) {
	// Mirrors the established stream test pattern (admin_events_test.go):
	// API-key auth over a real HTTP server. (Session/OIDC auth for the
	// stream is covered there and in the auth suites; this contract test
	// pins the documented media type and status.)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "contract-stream-secret"
	srv := testGateway(t, cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/admin/api/events/stream?limit=4", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events/stream GET: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("events/stream content type %q, want text/event-stream", ct)
	}
}

func TestAdminOpenAPIAuthEnvelopeAndAuditLimit(t *testing.T) {
	srv := testGateway(t, config.Default())
	// Engage the OIDC policy so anonymous requests are rejected (the
	// default local-trust mode is a separate, documented deployment
	// posture); installTestOIDCSession configures it and returns a
	// session for the authenticated half of the test.
	rawID := installTestOIDCSession(t, srv, authz.RoleAdmin)
	handler := srv.Handler()
	// Anonymous requests cost the whole per-IP rate bucket, so each
	// actor gets its own loopback IP.
	serveFrom := func(ip, path, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		req.RemoteAddr = ip + ":10001"
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookie})
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	anon := serveFrom("127.0.0.1", "/admin/api/snapshot", "")
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated snapshot: status %d body %s", anon.Code, anon.Body.String())
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(anon.Body.Bytes(), &envelope); err != nil || envelope.Error.Type != "gateway_error" || envelope.Error.Message == "" {
		t.Fatalf("unauthenticated body is not the documented envelope: %s", anon.Body.String())
	}

	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if err := srv.securityDB.AppendAudit(context.Background(), securitystore.AuditEvent{
			Timestamp: now.Add(time.Duration(i) * time.Second), Actor: "op", Action: fmt.Sprintf("contract.%d", i), Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	limited := serveFrom("127.0.0.2", "/admin/api/audit?limit=2", rawID)
	var out struct {
		Events []any `json:"events"`
	}
	if err := json.Unmarshal(limited.Body.Bytes(), &out); err != nil || limited.Code != http.StatusOK || len(out.Events) != 2 {
		t.Fatalf("audit limit=2: code %d events %d err %v", limited.Code, len(out.Events), err)
	}
	if bad := serveFrom("127.0.0.2", "/admin/api/audit?limit=0", rawID); bad.Code != http.StatusBadRequest {
		t.Fatalf("audit limit=0: status %d, want 400", bad.Code)
	}
}

func TestAdminOpenAPIViewerForbiddenFromKeys(t *testing.T) {
	// A read-only role is forbidden from key management, live. (Its own
	// server: the session-install helper uses a fixed session id, so a
	// server can hold only one installed test session.)
	srv := testGateway(t, config.Default())
	viewer := installTestOIDCSession(t, srv, authz.RoleViewer)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/client-keys", nil)
	req.RemoteAddr = "127.0.0.1:10001"
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: viewer})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer client-keys: status %d, want 403", rr.Code)
	}
}
