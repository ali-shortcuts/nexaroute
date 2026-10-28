package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Phase 7 regression tests: authorization bypass, tenant-adjacent
// scoping, SSRF, secret redaction, untrusted workflow data, ambiguous
// billable outcomes, malformed inputs, resource bounds, and audit
// identity. They cover the hardening pass only; protections that need
// a runtime, HTTP API, store, or scheduler that does not exist yet are
// recorded in docs/nexaflow-node-adapters.md, not faked here.

func executeErr(t *testing.T, id string, principal authz.Identity, input, config map[string]any, backend Backend, policy bool) error {
	t.Helper()
	reg := DefaultRegistry()
	spec, err := reg.Lookup(id, "v1")
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	_, err = reg.Execute(context.Background(), spec, principal, input, config, backend, policy)
	return err
}

func TestPhase7ApprovalRequiresIdentifiedParties(t *testing.T) {
	// Anonymous approval: no approver.
	if err := executeErr(t, "nexaroute.control.approval", ownerPrincipal(),
		map[string]any{"decision": "approved", "requester": "alice"}, nil, nil, false); err == nil {
		t.Fatal("approval without approver accepted")
	}
	// Unattributed approval: no requester.
	if err := executeErr(t, "nexaroute.control.approval", ownerPrincipal(),
		map[string]any{"decision": "approved", "approver": "bob"}, nil, nil, false); err == nil {
		t.Fatal("approval without requester accepted")
	}
	// Self-approval is still rejected.
	if err := executeErr(t, "nexaroute.control.approval", ownerPrincipal(),
		map[string]any{"decision": "approved", "approver": "alice", "requester": "alice"}, nil, nil, false); err == nil {
		t.Fatal("self-approval accepted")
	}
	// Identified, distinct parties still succeed.
	out := runNode(t, "nexaroute.control.approval", ownerPrincipal(),
		map[string]any{"decision": "approved", "approver": "bob", "requester": "alice"}, nil, nil, false)
	if out["approved"] != true || out["approver"] != "bob" {
		t.Fatalf("identified approval = %v", out)
	}
}

func TestPhase7CredentialHeadersForbidden(t *testing.T) {
	fb := newFakeBackend()
	for _, header := range []string{"Cookie", "X-Api-Key", "Proxy-Authorization", "Set-Cookie", "x-goog-api-key"} {
		if err := executeErr(t, "nexaroute.http.request", ownerPrincipal(),
			map[string]any{"url": "https://example.com/", "headers": map[string]any{header: "sekret"}}, nil, fb, true); err == nil {
			t.Fatalf("credential header %q accepted in http node input", header)
		}
	}
	// Embedded URL credentials are rejected at the node, not only by egress.
	if err := executeErr(t, "nexaroute.http.request", ownerPrincipal(),
		map[string]any{"url": "https://user:pass@example.com/"}, nil, fb, true); err == nil {
		t.Fatal("userinfo URL accepted")
	}
	// The secretRef path still works: Authorization is set by the node,
	// never by workflow input.
	fb.HTTPResp = HTTPResponse{Status: 200, Body: []byte("ok")}
	out := runNode(t, "nexaroute.http.request", ownerPrincipal(),
		map[string]any{"url": "https://example.com/hook"}, map[string]any{"auth_secret_ref": "api-token"}, fb, true)
	if out["status"] != 200 {
		t.Fatalf("secretRef http node = %v", out)
	}
}

func TestPhase7HTTPDoChokePoint(t *testing.T) {
	var gotHeaders http.Header
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPaths = append(gotPaths, r.URL.RequestURI())
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	ctx := context.Background()
	// Credential headers are rejected even when HTTPDo is called directly.
	if _, err := client.HTTPDo(ctx, http.MethodGet, srv.URL+"/x", map[string]string{"Cookie": "session=abc"}, nil); err == nil {
		t.Fatal("HTTPDo accepted a Cookie header")
	}
	if _, err := client.HTTPDo(ctx, http.MethodGet, srv.URL+"/x", map[string]string{"X-Api-Key": "k"}, nil); err == nil {
		t.Fatal("HTTPDo accepted an X-Api-Key header")
	}
	if len(gotPaths) != 0 {
		t.Fatalf("rejected requests still reached the server: %v", gotPaths)
	}
	// Authorization (the secretRef path) passes through.
	if _, err := client.HTTPDo(ctx, http.MethodGet, srv.URL+"/x", map[string]string{"Authorization": "Bearer tok"}, nil); err != nil {
		t.Fatalf("HTTPDo rejected Authorization: %v", err)
	}
	if gotHeaders.Get("Authorization") != "Bearer tok" {
		t.Fatalf("Authorization not forwarded: %v", gotHeaders)
	}
	// Oversize request bodies are rejected before any network use.
	if _, err := client.HTTPDo(ctx, http.MethodPost, srv.URL+"/x", nil, make([]byte, (1<<20)+1)); err == nil {
		t.Fatal("oversize HTTPDo body accepted")
	}
}

func TestPhase7HTTPDoSSRFAndRedirect(t *testing.T) {
	client := &GatewayClient{}
	ctx := context.Background()
	// Link-local metadata address: blocked by egress, never dialled.
	if _, err := client.HTTPDo(ctx, http.MethodGet, "https://169.254.169.254/latest/meta-data/", nil, nil); err == nil {
		t.Fatal("link-local SSRF target accepted")
	}
	// Private RFC1918 address over http (non-loopback): refused.
	if _, err := client.HTTPDo(ctx, http.MethodGet, "http://10.0.0.5/admin", nil, nil); err == nil {
		t.Fatal("private http target accepted")
	}
	// Cross-origin redirect: the loopback server redirects to a second
	// origin; the client must refuse instead of following with headers.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "second-origin")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	}))
	defer source.Close()
	if _, err := client.HTTPDo(ctx, http.MethodGet, source.URL+"/x", nil, nil); err == nil {
		t.Fatal("cross-origin redirect followed")
	}
	// Gateway client default also refuses cross-origin redirects.
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer chatSrv.Close()
	gc := &GatewayClient{BaseURL: chatSrv.URL}
	if _, err := gc.Chat(ctx, ChatRequest{Selector: "x", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("gateway client followed cross-origin redirect")
	}
}

func TestPhase7VideoPathEscapeAndUnlistedAsset(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.EscapedPath())
		switch {
		case r.URL.Path == "/v1/video/jobs/job-9":
			fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/job-9/assets/"):
			fmt.Fprint(w, "asset-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	ctx := context.Background()
	// Path traversal / query injection in the job id must not rewrite
	// the request: the id travels as one escaped segment.
	if _, err := client.VideoStatus(ctx, "../admin"); err == nil {
		t.Fatal("traversal job id unexpectedly succeeded")
	}
	if len(seen) == 0 || seen[len(seen)-1] != "/v1/video/jobs/..%2Fadmin" {
		t.Fatalf("traversal job id request path = %v", seen)
	}
	if _, err := client.VideoStatus(ctx, "a?x=1"); err == nil {
		t.Fatal("query-injecting job id unexpectedly succeeded")
	}
	// An asset that is not listed on the job is never downloaded.
	seen = nil
	if _, _, err := client.VideoAsset(ctx, "job-9", "a2"); err == nil || !strings.Contains(err.Error(), "asset_not_found") {
		t.Fatalf("unlisted asset error = %v, requests = %v", err, seen)
	}
	for _, p := range seen {
		if strings.Contains(p, "/assets/") {
			t.Fatalf("unlisted asset download was attempted: %v", seen)
		}
	}
	// The listed asset still downloads.
	if _, data, err := client.VideoAsset(ctx, "job-9", "a1"); err != nil || string(data) != "asset-bytes" {
		t.Fatalf("listed asset = %q, err = %v", data, err)
	}
}

func TestPhase7MCPServerScoping(t *testing.T) {
	var callNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "tools/list" {
			fmt.Fprint(w, `{"result":{"tools":[{"name":"tools__search_files","description":"search","inputSchema":{}},{"name":"other__read_file","description":"other","inputSchema":{}}]}}`)
			return
		}
		callNames = append(callNames, req.Params.Name)
		fmt.Fprint(w, `{"result":{"content":"called"}}`)
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	ctx := context.Background()
	// Listing is scoped to the requested server, prefix stripped.
	tools, err := client.MCPList(ctx, "tools")
	if err != nil || len(tools) != 1 || tools[0].Name != "search_files" {
		t.Fatalf("scoped mcp list = %+v, err = %v", tools, err)
	}
	// Calls are namespaced for the gateway.
	if _, err := client.MCPCall(ctx, "tools", "search_files", map[string]any{}); err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	if len(callNames) != 1 || callNames[0] != "tools__search_files" {
		t.Fatalf("mcp call names = %v", callNames)
	}
	// Node-level: a tool from another server is not callable through
	// this server, even when it is in the node allowlist.
	fb := newFakeBackend()
	fb.MCPTools = map[string][]MCPTool{"tools": {{Name: "search_files"}}}
	if err := executeErr(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "read_file", "arguments": map[string]any{}},
		map[string]any{"allowlist": []any{"read_file"}}, fb, false); err == nil || !strings.Contains(err.Error(), "tool_forbidden") {
		t.Fatalf("cross-server tool error = %v", err)
	}
}

func TestPhase7SecretRedactionGaps(t *testing.T) {
	redacted := RedactJSON(map[string]any{
		"set-cookie":          "session=abc123",
		"cookie":              "session=abc123",
		"X-Api-Key":           "sekret",
		"total_prompt_tokens": float64(42),
		"nested":              map[string]any{"proxy-authorization": "Basic xyz"},
	})
	if strings.Contains(redacted, "abc123") || strings.Contains(redacted, "sekret") || strings.Contains(redacted, "xyz") {
		t.Fatalf("secret survived redaction: %s", redacted)
	}
	if !strings.Contains(redacted, "total_prompt_tokens") || !strings.Contains(redacted, "42") {
		t.Fatalf("legitimate counter redacted: %s", redacted)
	}
	if got := RedactError(errors.New("proxy failed with Basic dXNlcjpwYXNz")); strings.Contains(got, "dXNlcjpwYXNz") {
		t.Fatalf("basic credential survived RedactError: %s", got)
	}
	// Response headers carrying Set-Cookie are redacted by Execute.
	fb := newFakeBackend()
	fb.HTTPResp = HTTPResponse{Status: 200, Body: []byte("ok"), Headers: map[string]string{"Set-Cookie": "session=abc123", "Content-Type": "text/plain"}}
	out := runNode(t, "nexaroute.http.request", ownerPrincipal(),
		map[string]any{"url": "https://example.com/"}, nil, fb, true)
	headers, _ := out["headers"].(map[string]any)
	if headers["Set-Cookie"] != "[REDACTED]" {
		t.Fatalf("set-cookie header survived Execute redaction: %v", headers)
	}
}

func TestPhase7DeepNestingBounded(t *testing.T) {
	deep := map[string]any{}
	cur := deep
	for i := 0; i < 500; i++ {
		next := map[string]any{}
		cur["next"] = next
		cur = next
	}
	cur["api_key"] = "sekret"
	// Must not panic or recurse unboundedly, and the deep secret must
	// not surface (it sits below the redaction depth bound).
	out := RedactJSON(deep)
	if strings.Contains(out, "sekret") {
		t.Fatal("deep secret surfaced")
	}
	if !strings.Contains(out, "[TRUNCATED]") {
		t.Fatal("deep value was not depth-truncated")
	}
}

func TestPhase7ConfigBoundAndAllowlistCap(t *testing.T) {
	if err := executeErr(t, "nexaroute.control.branch", ownerPrincipal(),
		map[string]any{"condition_result": true},
		map[string]any{"true_branch": strings.Repeat("x", MaxConfigBytes+1), "false_branch": "no"}, nil, false); err == nil || !strings.Contains(err.Error(), "config_too_large") {
		t.Fatalf("oversize config error = %v", err)
	}
	allowlist := make([]any, 0, 1001)
	for i := 0; i < 1001; i++ {
		allowlist = append(allowlist, "tool-"+strconv.Itoa(i))
	}
	if err := executeErr(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "search_files"},
		map[string]any{"allowlist": allowlist}, newFakeBackend(), false); err == nil {
		t.Fatal("oversize MCP allowlist accepted")
	}
}

func TestPhase7TimeoutRetryableFollowsPolicy(t *testing.T) {
	reg := DefaultRegistry()
	slow := func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return map[string]any{}, nil
		}
	}
	// Non-retryable spec: timeout must NOT be marked retryable.
	noRetry := &NodeSpec{ID: "test.noretry", Version: "v1", Retry: RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 10 * time.Millisecond, MaxTimeout: 10 * time.Millisecond,
		MaxInputBytes: 1024, MaxOutputBytes: 1024, Run: slow}
	r1, err := NewRegistry([]*NodeSpec{noRetry})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r1.Execute(context.Background(), noRetry, ownerPrincipal(), nil, nil, nil, false)
	var ne *NodeError
	if !errors.As(err, &ne) || ne.Code != "timeout" || ne.Retryable {
		t.Fatalf("non-retryable timeout = %v (retryable=%v)", err, ne != nil && ne.Retryable)
	}
	// Retryable spec: timeout IS marked retryable.
	retryable := &NodeSpec{ID: "test.retry", Version: "v1",
		Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout"}},
		DefaultTimeout: 10 * time.Millisecond, MaxTimeout: 10 * time.Millisecond,
		MaxInputBytes: 1024, MaxOutputBytes: 1024, Run: slow}
	r2, err := NewRegistry([]*NodeSpec{retryable})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r2.Execute(context.Background(), retryable, ownerPrincipal(), nil, nil, nil, false)
	if !errors.As(err, &ne) || ne.Code != "timeout" || !ne.Retryable {
		t.Fatalf("retryable timeout = %v", err)
	}
	// The LLM node itself must not declare a retryable billable path.
	spec, _ := reg.Lookup("nexaroute.llm.chat", "v1")
	if spec.Retry.MaxAttempts != 1 {
		t.Fatalf("llm node MaxAttempts = %d, want 1 (ambiguous billable timeouts are never auto-retried)", spec.Retry.MaxAttempts)
	}
	for _, code := range spec.Retry.RetryableCodes {
		if code == "timeout" {
			t.Fatal("llm node still declares timeout retryable")
		}
	}
}

func TestPhase7MalformedNumbersAndStrictCondition(t *testing.T) {
	// NaN defeats every range comparison; it must be rejected, not pass.
	if err := executeErr(t, "nexaroute.control.wait", ownerPrincipal(),
		map[string]any{"duration_ms": math.NaN()}, nil, nil, false); err == nil {
		t.Fatal("NaN wait duration accepted")
	}
	if err := executeErr(t, "nexaroute.cost.budget_check", ownerPrincipal(),
		map[string]any{"key": "team", "threshold_usd": math.NaN()}, nil, newFakeBackend(), false); err == nil {
		t.Fatal("NaN budget threshold accepted")
	}
	if err := executeErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "x", "duration_seconds": math.NaN()}, nil, newFakeBackend(), true); err == nil {
		t.Fatal("NaN video duration accepted")
	}
	// "123abc" is not the number 123: strict parsing must reject it
	// instead of branching on a prefix the author never wrote.
	if err := executeErr(t, "nexaroute.control.condition", ownerPrincipal(),
		map[string]any{"value": map[string]any{"n": float64(124)}},
		map[string]any{"field": "n", "operator": "gt", "expected": "123abc"}, nil, false); err == nil {
		t.Fatal("loose numeric parse accepted trailing junk")
	}
	out := runNode(t, "nexaroute.control.condition", ownerPrincipal(),
		map[string]any{"value": map[string]any{"n": float64(124)}},
		map[string]any{"field": "n", "operator": "gt", "expected": "123"}, nil, false)
	if out["result"] != true {
		t.Fatalf("strict numeric condition = %v", out)
	}
}

func TestPhase7TemplatePlaceholderSafety(t *testing.T) {
	// Embedded placeholders are substituted, not left literal.
	tpl := Template{ID: "t1", Version: "v1",
		Nodes:        []TemplateNode{{NodeID: "nexaroute.control.branch", Version: "v1", Config: map[string]any{"true_branch": "ok-{{suffix}}", "false_branch": "no"}}},
		Placeholders: []string{"suffix"}}
	inst, err := tpl.Instantiate(map[string]string{"suffix": "x"}, false)
	if err != nil {
		t.Fatalf("embedded placeholder instantiation: %v", err)
	}
	if inst.Nodes[0].Config["true_branch"] != "ok-x" {
		t.Fatalf("embedded placeholder = %v", inst.Nodes[0].Config)
	}
	// An unresolved placeholder pattern is an error, never a value.
	bad := Template{ID: "t2", Version: "v1",
		Nodes: []TemplateNode{{NodeID: "nexaroute.control.branch", Version: "v1", Config: map[string]any{"true_branch": "ok-{{missing}}", "false_branch": "no"}}}}
	if _, err := bad.Instantiate(map[string]string{}, false); err == nil {
		t.Fatal("unresolved placeholder accepted")
	}
	// A template referencing an unknown node cannot instantiate.
	unknown := Template{ID: "t3", Version: "v1",
		Nodes: []TemplateNode{{NodeID: "nexaroute.nope", Version: "v1", Config: map[string]any{}}}}
	if _, err := unknown.Instantiate(map[string]string{}, false); err == nil {
		t.Fatal("unknown-node template accepted")
	}
}

func TestPhase7WebhookReplayRace(t *testing.T) {
	v := NewWebhookVerifier()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := "ping"
	sig := signWebhook(t, ts, body, "s3cret")
	var wg sync.WaitGroup
	success := make(chan bool, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			success <- v.Verify(ts, sig, body, "s3cret") == nil
		}()
	}
	wg.Wait()
	close(success)
	wins := 0
	for ok := range success {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent webhook replay wins = %d, want exactly 1", wins)
	}
}

func TestPhase7ViewerCannotRunBillableNodes(t *testing.T) {
	// Permission enforcement is per node, not per workflow: a viewer
	// may read routing but may not submit a billable video job, and an
	// anonymous principal may execute nothing at all.
	if err := executeErr(t, "nexaroute.video.submit", viewerPrincipal(),
		map[string]any{"prompt": "x", "duration_seconds": float64(5)}, nil, newFakeBackend(), true); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("viewer video submit error = %v", err)
	}
	if err := executeErr(t, "nexaroute.llm.chat", authz.Identity{},
		map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding"}, newFakeBackend(), true); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("anonymous llm error = %v", err)
	}
}
