package nexaflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// FakeBackend is a deterministic test double. It is NOT a production code
// path: it never touches a real provider, gateway, or credential store.
// Eval replay goes through the real eval.Runner (replay is deterministic
// and side-effect free), which is exactly what the eval node adapts.
type FakeBackend struct {
	Selectors map[string]RouteInfo
	ChatText  string
	Health    map[string]ProviderHealth
	CompatMap map[string]CompatInfo
	MCPTools  map[string][]MCPTool
	MCPResult map[string]any
	VideoJobs map[string]VideoJobInfo
	Usage     UsageInfo
	Budgets   map[string]BudgetInfo
	Secrets   map[string]string
	HTTPResp  HTTPResponse
	HTTPErr   error
	ChatCalls int
	MCPCalls  []string
}

func newFakeBackend() *FakeBackend {
	return &FakeBackend{
		Selectors: map[string]RouteInfo{
			"team/coding": {Kind: "alias", ResolvedModel: "vendor/model", DeploymentIDs: []string{"prov/model"}},
		},
		ChatText: "fake reply",
		Health: map[string]ProviderHealth{
			"prov/model": {DeploymentID: "prov/model", Status: "healthy", Successes: 10, Failures: 1},
		},
		CompatMap: map[string]CompatInfo{
			"prov/model": {DeploymentID: "prov/model", Support: map[string]string{"text": "supported", "tools": "supported", "vision": "unsupported"}, ScorecardStatus: "CLAUDE_CODE_READY", Protocol: "openai_chat", ContextWindow: 128000},
		},
		MCPTools: map[string][]MCPTool{
			"tools": {{Name: "search_files", Description: "search"}, {Name: "read_file", Description: "read"}},
		},
		MCPResult: map[string]any{"content": "ok"},
		VideoJobs: map[string]VideoJobInfo{
			"job-1": {JobID: "job-1", State: "completed", Terminal: true, Assets: []VideoAssetInfo{{ID: "a1", Filename: "out.mp4", ContentType: "video/mp4", SHA256: "abc", SizeBytes: 5}}},
		},
		Usage:   UsageInfo{TotalPromptTokens: 100, TotalCompletionTokens: 50, TotalRequests: 3, TotalEstimatedCostUSD: 0.01, TotalCostBasis: "recorded", TotalUnpricedTokens: 0},
		Budgets: map[string]BudgetInfo{"team": {Key: "team", Committed: 4.0, Reserved: 1.0}},
		Secrets: map[string]string{"webhook-secret": "s3cret", "api-token": "tok-123"},
	}
}

func (f *FakeBackend) ResolveSelector(_ context.Context, selector string) (RouteInfo, error) {
	if info, ok := f.Selectors[selector]; ok {
		return info, nil
	}
	return RouteInfo{}, fmt.Errorf("selector %q not found", selector)
}

func (f *FakeBackend) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	f.ChatCalls++
	return ChatResponse{Model: "vendor/model", Text: f.ChatText, PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, ResolvedRoute: req.Selector}, nil
}

func (f *FakeBackend) ProviderHealth(_ context.Context, id string) (ProviderHealth, error) {
	if h, ok := f.Health[id]; ok {
		return h, nil
	}
	return ProviderHealth{DeploymentID: id, Status: "unknown"}, nil
}

func (f *FakeBackend) Compat(_ context.Context, id string) (CompatInfo, error) {
	if c, ok := f.CompatMap[id]; ok {
		return c, nil
	}
	return CompatInfo{DeploymentID: id, Support: map[string]string{}}, nil
}

func (f *FakeBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	return (&ReplayEvalBackend{}).RunEvalReplay(ctx, suiteID, deploymentID, outcomes)
}

func (f *FakeBackend) MCPList(_ context.Context, serverID string) ([]MCPTool, error) {
	return f.MCPTools[serverID], nil
}

func (f *FakeBackend) MCPCall(_ context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	f.MCPCalls = append(f.MCPCalls, serverID+"__"+tool)
	return f.MCPResult, nil
}

func (f *FakeBackend) VideoSubmit(_ context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	_ = req
	return VideoJobInfo{JobID: "job-1", State: "queued"}, nil
}

func (f *FakeBackend) VideoStatus(_ context.Context, jobID string) (VideoJobInfo, error) {
	if j, ok := f.VideoJobs[jobID]; ok {
		return j, nil
	}
	return VideoJobInfo{}, fmt.Errorf("job %q not found", jobID)
}

func (f *FakeBackend) VideoAsset(_ context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error) {
	job, err := f.VideoStatus(context.Background(), jobID)
	if err != nil {
		return VideoAssetInfo{}, nil, err
	}
	for _, a := range job.Assets {
		if a.ID == assetID {
			return a, []byte("bytes"), nil
		}
	}
	return VideoAssetInfo{}, nil, fmt.Errorf("asset %q not found", assetID)
}

func (f *FakeBackend) UsageSnapshot(_ context.Context) (UsageInfo, error) { return f.Usage, nil }

func (f *FakeBackend) BudgetSnapshot(_ context.Context, key string) (BudgetInfo, error) {
	if b, ok := f.Budgets[key]; ok {
		return b, nil
	}
	return BudgetInfo{Key: key}, nil
}

func (f *FakeBackend) HTTPDo(_ context.Context, method, url string, headers map[string]string, body []byte) (HTTPResponse, error) {
	_ = method
	_ = url
	_ = headers
	_ = body
	if f.HTTPErr != nil {
		return HTTPResponse{}, f.HTTPErr
	}
	if f.HTTPResp.Status != 0 {
		return f.HTTPResp, nil
	}
	return HTTPResponse{Status: 200, Body: []byte("hello")}, nil
}

func (f *FakeBackend) ResolveSecretRef(ref string) (string, error) {
	if v, ok := f.Secrets[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no secret resolver for ref %q", ref)
}

var _ Backend = (*FakeBackend)(nil)

func ownerPrincipal() authz.Identity {
	return authz.Identity{Subject: "owner-1", Roles: []authz.Role{authz.RoleOwner}}
}

func viewerPrincipal() authz.Identity {
	return authz.Identity{Subject: "viewer-1", Roles: []authz.Role{authz.RoleViewer}}
}

func runNode(t *testing.T, id string, principal authz.Identity, input, config map[string]any, backend Backend, policy bool) map[string]any {
	t.Helper()
	reg := DefaultRegistry()
	spec, err := reg.Lookup(id, "v1")
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	out, err := reg.Execute(context.Background(), spec, principal, input, config, backend, policy)
	if err != nil {
		t.Fatalf("execute %s: %v", id, err)
	}
	return out
}

func TestGatewayClientChatMCPVideo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"model":"vendor/model","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
		case r.URL.Path == "/mcp":
			var req struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			if req.Method == "tools/list" {
				fmt.Fprint(w, `{"result":{"tools":[{"name":"search_files","description":"search","inputSchema":{}}]}}`)
			} else {
				fmt.Fprint(w, `{"result":{"content":"called"}}`)
			}
		case r.URL.Path == "/v1/video/jobs" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"job_id":"job-9","state":"queued"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/job-9/assets/"):
			fmt.Fprint(w, "asset-bytes")
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/"):
			fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	ctx := context.Background()
	chat, err := client.Chat(ctx, ChatRequest{Selector: "team/coding", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, MaxTokens: 10})
	if err != nil || chat.Text != "hello" || chat.TotalTokens != 7 {
		t.Fatalf("chat = %+v, err = %v", chat, err)
	}
	tools, err := client.MCPList(ctx, "tools")
	if err != nil || len(tools) != 1 || tools[0].Name != "search_files" {
		t.Fatalf("mcp list = %+v, err = %v", tools, err)
	}
	if _, err := client.MCPCall(ctx, "tools", "search_files", map[string]any{}); err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	job, err := client.VideoSubmit(ctx, VideoSubmitRequest{Prompt: "a cat", Mode: "text_to_video", DurationSeconds: 5, AspectRatio: "16:9"})
	if err != nil || job.JobID != "job-9" {
		t.Fatalf("video submit = %+v, err = %v", job, err)
	}
	status, err := client.VideoStatus(ctx, "job-9")
	if err != nil || !status.Terminal || len(status.Assets) != 1 {
		t.Fatalf("video status = %+v, err = %v", status, err)
	}
	meta, data, err := client.VideoAsset(ctx, "job-9", "a1")
	if err != nil || string(data) != "asset-bytes" || meta.Filename != "out.mp4" {
		t.Fatalf("video asset = %+v %q, err = %v", meta, data, err)
	}
}
