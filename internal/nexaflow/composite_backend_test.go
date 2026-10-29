package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// fakeGatewayServer serves every GatewayClient endpoint with canned
// responses and records which paths were hit, so tests can prove a
// call did (or did not) reach the gateway.
type fakeGatewayServer struct {
	server *httptest.Server
	paths  []string
}

func newFakeGatewayServer(t *testing.T) *fakeGatewayServer {
	t.Helper()
	f := &fakeGatewayServer{}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("fake gateway encode: %v", err)
		}
	}
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "team/coding"}, {"id": "prov/model"}}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		writeJSON(w, map[string]any{
			"model":   "prov/model",
			"choices": []map[string]any{{"message": map[string]any{"content": "hello"}}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("fake gateway mcp decode: %v", err)
		}
		switch req.Method {
		case "tools/list":
			writeJSON(w, map[string]any{"result": map[string]any{"tools": []map[string]any{
				{"name": "srv__echo", "description": "echo", "inputSchema": map[string]any{}},
			}}})
		case "tools/call":
			writeJSON(w, map[string]any{"result": map[string]any{"echoed": true}})
		default:
			writeJSON(w, map[string]any{"error": map[string]any{"message": "unknown method"}})
		}
	})
	mux.HandleFunc("/v1/video/jobs", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		if r.Method == http.MethodPost {
			writeJSON(w, map[string]any{"job_id": "job-1", "state": "queued"})
			return
		}
		writeJSON(w, map[string]any{"job_id": "job-1", "state": "completed", "output_assets": []map[string]any{
			{"id": "a1", "filename": "out.mp4", "content_type": "video/mp4", "sha256": "abc", "size_bytes": 4},
		}})
	})
	mux.HandleFunc("/v1/video/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		writeJSON(w, map[string]any{"job_id": "job-1", "state": "completed", "output_assets": []map[string]any{
			{"id": "a1", "filename": "out.mp4", "content_type": "video/mp4", "sha256": "abc", "size_bytes": 4},
		}})
	})
	mux.HandleFunc("/v1/video/jobs/job-1/assets/a1", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		fmt.Fprint(w, "DATA")
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		fmt.Fprint(w, "echo-ok")
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGatewayServer) hit(path string) bool {
	for _, p := range f.paths {
		if p == path {
			return true
		}
	}
	return false
}

// ResolveSelector validates against the gateway's own /v1/models list:
// listed selectors resolve, unlisted ones fail closed before any chat.
func TestGatewayClientResolveSelector(t *testing.T) {
	f := newFakeGatewayServer(t)
	client := &GatewayClient{BaseURL: f.server.URL}
	info, err := client.ResolveSelector(context.Background(), "team/coding")
	if err != nil || info.Kind != "route" || info.ResolvedModel != "team/coding" {
		t.Fatalf("selector = %+v, err = %v", info, err)
	}
	if _, err := client.ResolveSelector(context.Background(), "no/such"); err == nil {
		t.Fatal("unlisted selector resolved, want selector_not_found")
	} else {
		var ne *NodeError
		if !errors.As(err, &ne) || ne.Code != "selector_not_found" {
			t.Fatalf("unlisted selector error = %v, want selector_not_found", err)
		}
	}
	if f.hit("/v1/chat/completions") {
		t.Fatal("selector validation made a chat call")
	}
	if _, err := client.ResolveSelector(context.Background(), "  "); err == nil {
		t.Fatal("empty selector resolved, want validation_error")
	}
}

func TestGatewayClientResolveSelectorGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	if _, err := client.ResolveSelector(context.Background(), "team/coding"); err == nil {
		t.Fatal("selector resolved despite gateway 503, want backend_error")
	}
}

// ResolveSecretRef fails closed without a resolver, on an empty ref,
// and on a resolver error — and never surfaces the resolved value in
// an error.
func TestGatewayClientResolveSecretRef(t *testing.T) {
	client := &GatewayClient{}
	if _, err := client.ResolveSecretRef("x"); err == nil {
		t.Fatal("secret resolved without a resolver, want secret_error")
	}
	client.SecretResolver = func(ref string) (string, error) {
		if ref == "good" {
			return "s3cr3t-value", nil
		}
		return "", errors.New("boom")
	}
	if _, err := client.ResolveSecretRef(""); err == nil {
		t.Fatal("empty secret ref resolved, want secret_error")
	}
	if v, err := client.ResolveSecretRef("good"); err != nil || v != "s3cr3t-value" {
		t.Fatalf("secret = %q, err = %v", v, err)
	}
	if _, err := client.ResolveSecretRef("bad"); err == nil {
		t.Fatal("failing resolver accepted, want secret_error")
	} else if strings.Contains(err.Error(), "boom") {
		t.Fatalf("resolver error leaked into node error: %v", err)
	}
}

// Every CompositeBackend port with a nil component fails closed with
// backend_unavailable instead of silently succeeding.
func TestCompositeBackendNilPortsFailClosed(t *testing.T) {
	ctx := context.Background()
	b := &CompositeBackend{}
	cases := []struct {
		name string
		call func() error
	}{
		{"selector", func() error { _, err := b.ResolveSelector(ctx, "x"); return err }},
		{"chat", func() error { _, err := b.Chat(ctx, ChatRequest{}); return err }},
		{"health", func() error { _, err := b.ProviderHealth(ctx, "x"); return err }},
		{"compat", func() error { _, err := b.Compat(ctx, "x"); return err }},
		{"eval", func() error { _, err := b.RunEvalReplay(ctx, "s", "d", nil); return err }},
		{"mcp-list", func() error { _, err := b.MCPList(ctx, "s"); return err }},
		{"mcp-call", func() error { _, err := b.MCPCall(ctx, "s", "t", nil); return err }},
		{"video-submit", func() error { _, err := b.VideoSubmit(ctx, VideoSubmitRequest{}); return err }},
		{"video-status", func() error { _, err := b.VideoStatus(ctx, "j"); return err }},
		{"video-asset", func() error { _, _, err := b.VideoAsset(ctx, "j", "a"); return err }},
		{"usage", func() error { _, err := b.UsageSnapshot(ctx); return err }},
		{"budget", func() error { _, err := b.BudgetSnapshot(ctx, "k"); return err }},
		{"http", func() error { _, err := b.HTTPDo(ctx, "GET", "http://127.0.0.1/", nil, nil); return err }},
		{"secret", func() error { _, err := b.ResolveSecretRef("x"); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var ne *NodeError
			if !errors.As(err, &ne) || ne.Code != "backend_unavailable" {
				t.Fatalf("err = %v, want backend_unavailable", err)
			}
		})
	}
}

// CompositeBackend delegates each port to the subsystem that owns it:
// gateway HTTP for chat/MCP/video/egress, in-process health/compat,
// usage tracker + budget ledger snapshots, and eval replay.
func TestCompositeBackendDelegatesToOwners(t *testing.T) {
	ctx := context.Background()
	f := newFakeGatewayServer(t)
	b := &CompositeBackend{
		Gateway:      &GatewayClient{BaseURL: f.server.URL},
		HealthCompat: &DirectHealthCompatBackend{Health: &health.Manager{}, CompatStore: compat.NewStore()},
		Cost:         &DirectCostBackend{Tracker: &usage.Tracker{}, Ledger: usage.NewBudgetLedger()},
		Eval:         &ReplayEvalBackend{},
	}
	if resp, err := b.Chat(ctx, ChatRequest{Selector: "prov/model", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err != nil || resp.Text != "hello" {
		t.Fatalf("chat = %+v, err = %v", resp, err)
	}
	tools, err := b.MCPList(ctx, "srv")
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("mcp list = %+v, err = %v", tools, err)
	}
	if res, err := b.MCPCall(ctx, "srv", "echo", nil); err != nil || res["echoed"] != true {
		t.Fatalf("mcp call = %+v, err = %v", res, err)
	}
	job, err := b.VideoSubmit(ctx, VideoSubmitRequest{Prompt: "p", Mode: "text_to_video"})
	if err != nil || job.JobID != "job-1" {
		t.Fatalf("video submit = %+v, err = %v", job, err)
	}
	st, err := b.VideoStatus(ctx, "job-1")
	if err != nil || st.State != "completed" {
		t.Fatalf("video status = %+v, err = %v", st, err)
	}
	meta, data, err := b.VideoAsset(ctx, "job-1", "a1")
	if err != nil || meta.ID != "a1" || string(data) != "DATA" {
		t.Fatalf("video asset = %+v %q, err = %v", meta, data, err)
	}
	healthInfo, err := b.ProviderHealth(ctx, "prov/model")
	if err != nil || healthInfo.DeploymentID != "prov/model" {
		t.Fatalf("health = %+v, err = %v", healthInfo, err)
	}
	if _, err := b.Compat(ctx, "prov/model"); err != nil {
		t.Fatalf("compat err = %v", err)
	}
	usageInfo, err := b.UsageSnapshot(ctx)
	if err != nil {
		t.Fatalf("usage err = %v", err)
	}
	_ = usageInfo
	budgetInfo, err := b.BudgetSnapshot(ctx, "k")
	if err != nil || budgetInfo.Key != "k" || budgetInfo.Committed != 0 || budgetInfo.Reserved != 0 {
		t.Fatalf("budget = %+v, err = %v", budgetInfo, err)
	}
	httpResp, err := b.HTTPDo(ctx, "GET", f.server.URL+"/echo", nil, nil)
	if err != nil || httpResp.Status != 200 || string(httpResp.Body) != "echo-ok" {
		t.Fatalf("http = %+v, err = %v", httpResp, err)
	}
	suite, ok := eval.LookupSuite("coding")
	if !ok || len(suite.Cases) == 0 {
		t.Fatal("coding suite unavailable for eval replay delegation test")
	}
	res, err := b.RunEvalReplay(ctx, "coding", "prov/model", []eval.Outcome{{CaseID: suite.Cases[0].ID, Status: eval.OutcomeOK, Output: "42"}})
	if err != nil {
		t.Fatalf("eval replay err = %v", err)
	}
	if res.SuiteID != "coding" {
		t.Fatalf("eval suite = %q, want coding", res.SuiteID)
	}
	if res.Upstream != 0 {
		t.Fatalf("eval upstream calls = %d, want 0 (replay only)", res.Upstream)
	}
}
