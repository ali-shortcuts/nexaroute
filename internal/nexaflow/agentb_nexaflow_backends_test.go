package nexaflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Agent B (100% coverage mission): HTTP backend, MCP/provider/trigger/
// video nodes, and studio-port gaps. Tests only; no production code
// is touched here.

// agentBExecErr runs one node through the registry and returns its
// error (nil on success). Unlike runNode it never fails the test on
// an error, so error branches can be asserted.
func agentBExecErr(t *testing.T, id string, principal authz.Identity, input, config map[string]any, backend Backend, policy bool) error {
	t.Helper()
	reg := DefaultRegistry()
	spec, err := reg.Lookup(id, SupportedVersion)
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	_, err = reg.Execute(context.Background(), spec, principal, input, config, backend, policy)
	return err
}

func agentBRequireNodeError(t *testing.T, err error, code, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %q node error, got nil", code)
	}
	var nerr *NodeError
	if !errors.As(err, &nerr) {
		t.Fatalf("error %v is not a *NodeError, want code %q", err, code)
	}
	if nerr.Code != code {
		t.Fatalf("error code = %q, want %q (err: %v)", nerr.Code, code, err)
	}
	if substr != "" && !strings.Contains(err.Error(), substr) {
		t.Fatalf("error = %v, want it to contain %q", err, substr)
	}
}

// agentBNodeErrBackend is a FakeBackend with per-method injected errors.
type agentBNodeErrBackend struct {
	*FakeBackend
	mcpListErr     error
	mcpCallErr     error
	healthErr      error
	compatErr      error
	videoSubmitErr error
	videoStatusErr error
}

func (b *agentBNodeErrBackend) MCPList(ctx context.Context, serverID string) ([]MCPTool, error) {
	if b.mcpListErr != nil {
		return nil, b.mcpListErr
	}
	return b.FakeBackend.MCPList(ctx, serverID)
}

func (b *agentBNodeErrBackend) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	if b.mcpCallErr != nil {
		return nil, b.mcpCallErr
	}
	return b.FakeBackend.MCPCall(ctx, serverID, tool, args)
}

func (b *agentBNodeErrBackend) ProviderHealth(ctx context.Context, id string) (ProviderHealth, error) {
	if b.healthErr != nil {
		return ProviderHealth{}, b.healthErr
	}
	return b.FakeBackend.ProviderHealth(ctx, id)
}

func (b *agentBNodeErrBackend) Compat(ctx context.Context, id string) (CompatInfo, error) {
	if b.compatErr != nil {
		return CompatInfo{}, b.compatErr
	}
	return b.FakeBackend.Compat(ctx, id)
}

func (b *agentBNodeErrBackend) VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	if b.videoSubmitErr != nil {
		return VideoJobInfo{}, b.videoSubmitErr
	}
	return b.FakeBackend.VideoSubmit(ctx, req)
}

func (b *agentBNodeErrBackend) VideoStatus(ctx context.Context, jobID string) (VideoJobInfo, error) {
	if b.videoStatusErr != nil {
		return VideoJobInfo{}, b.videoStatusErr
	}
	return b.FakeBackend.VideoStatus(ctx, jobID)
}

// ---- http_backend.go ----

func TestAgentBHTTPValidateLoopbackPort(t *testing.T) {
	// url.Parse rejects a non-numeric port before the range check,
	// so only out-of-range numeric ports reach it.
	for _, raw := range []string{
		"http://127.0.0.1:99999/",
		"http://127.0.0.1:0/",
	} {
		err := validateLoopbackBaseURL(raw)
		if err == nil {
			t.Fatalf("validateLoopbackBaseURL(%q) succeeded, want a port error", raw)
		}
		if !strings.Contains(err.Error(), "port must be between 1 and 65535") {
			t.Fatalf("validateLoopbackBaseURL(%q) = %v", raw, err)
		}
	}
}

func TestAgentBHTTPDoJSONRequestBuildError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	client := &GatewayClient{BaseURL: srv.URL}
	_, err := client.doJSON(context.Background(), "BAD\nMETHOD", "/x", nil, nil)
	agentBRequireNodeError(t, err, "backend_error", "gateway request failed")
}

func TestAgentBHTTPVideoAssetAuthTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/job-9/assets/"):
			fmt.Fprint(w, "asset-bytes")
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/"):
			fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// The resolver succeeds for the status call inside VideoAsset
	// and fails for the asset call itself: the only way the second
	// authToken check can fail after the first one passed is a
	// resolver that fails between the two calls.
	calls := 0
	resolver := func(ref string) (string, error) {
		calls++
		if calls == 1 {
			return "tok-1", nil
		}
		return "", errors.New("agentb: resolver failed on second call")
	}
	client := &GatewayClient{BaseURL: srv.URL, AuthTokenRef: "tok", SecretResolver: resolver}
	_, _, err := client.VideoAsset(context.Background(), "job-9", "a1")
	agentBRequireNodeError(t, err, "secret_error", "auth token resolution failed")
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (status + asset)", calls)
	}
}

// agentBCancelOnCloseBody cancels ctx the first time the wrapped response
// body is closed. doJSON closes the body only after fully reading and
// decoding it, so the cancel lands strictly between VideoAsset's status
// call and its asset-request construction.
type agentBCancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *agentBCancelOnCloseBody) Close() error {
	b.once.Do(b.cancel)
	return b.ReadCloser.Close()
}

// agentBCancelOnBodyCloseTransport wraps every response body so its Close
// cancels ctx (see agentBCancelOnCloseBody).
type agentBCancelOnBodyCloseTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (t *agentBCancelOnBodyCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &agentBCancelOnCloseBody{ReadCloser: resp.Body, cancel: t.cancel}
	return resp, nil
}

func TestAgentBHTTPVideoAssetCanceledContext(t *testing.T) {
	// Behavior coverage (NOT a regression test for commit d24d27cd):
	// a context canceled between VideoAsset's status call and its
	// asset request must fail closed with backend_error, never panic
	// and never attempt the download.
	//
	// Correction to supervisor guidance 6691513651: the canceled-
	// context panic premise behind it is wrong. Go 1.27's
	// http.NewRequestWithContext errors only on an invalid method, a
	// nil context, or a URL parse failure — it never returns
	// ctx.Err() — so on a canceled context it returns a usable
	// request and the failure surfaces at client.Do instead. This
	// test passes identically with and without d24d27cd (verified by
	// running it against the pre-fix file); the restored
	// validateLoopbackBaseURL / NewRequestWithContext error checks are
	// defense-in-depth on branches unreachable once the status call
	// succeeded (constant GET, already-validated URL, escaped path
	// segments). The test pins the fail-closed behavior, not the fix.
	// The cancel is driven by the status response body closing, which
	// doJSON does only after the body is fully read and decoded — so
	// the context is live for the whole status call and dead before
	// the asset request is built. No sleep, no race.
	assetHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/job-9/assets/"):
			assetHits++
			fmt.Fprint(w, "asset-bytes")
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/"):
			fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &GatewayClient{
		BaseURL: srv.URL,
		HTTPClient: &http.Client{
			Transport: &agentBCancelOnBodyCloseTransport{base: http.DefaultTransport, cancel: cancel},
		},
	}
	_, _, err := client.VideoAsset(ctx, "job-9", "a1")
	// The failure surfaces at client.Do (canceled context), not at
	// request construction: NewRequestWithContext never returns
	// ctx.Err(). Either backend_error wording is the fail-closed
	// outcome; the pinned facts are the code and that no download
	// was attempted.
	agentBRequireNodeError(t, err, "backend_error", "")
	if assetHits != 0 {
		t.Fatalf("asset endpoint hit %d times, want 0 (request must fail before any download)", assetHits)
	}
}

// ---- nodes_mcp.go ----

func TestAgentBMCPListRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.mcp.list", ownerPrincipal(),
		map[string]any{"server_id": "tools"}, nil, nil, false)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBMCPListRequiresServerID(t *testing.T) {
	// Direct call: the registry rejects a missing required field
	// before the node runs, so the node's own check is exercised
	// here with no server_id in the input at all.
	_, err := runMCPList(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: map[string]any{}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "server_id is required")
}

func TestAgentBMCPListBackendError(t *testing.T) {
	backend := &agentBNodeErrBackend{FakeBackend: newFakeBackend(), mcpListErr: errors.New("agentb: list boom")}
	err := agentBExecErr(t, "nexaroute.mcp.list", ownerPrincipal(),
		map[string]any{"server_id": "tools"}, nil, backend, false)
	if err == nil || !strings.Contains(err.Error(), "list boom") {
		t.Fatalf("MCPList backend error = %v", err)
	}
}

func TestAgentBMCPCallRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "search_files"}, map[string]any{"allowlist": []any{"tools__search_files"}}, nil, false)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBMCPCallRequiresServerID(t *testing.T) {
	_, err := runMCPCall(context.Background(), &RunContext{Principal: ownerPrincipal(),
		Input:  map[string]any{"tool": "search_files"},
		Config: map[string]any{"allowlist": []any{"tools__search_files"}}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "server_id is required")
}

func TestAgentBMCPCallRequiresTool(t *testing.T) {
	_, err := runMCPCall(context.Background(), &RunContext{Principal: ownerPrincipal(),
		Input:  map[string]any{"server_id": "tools"},
		Config: map[string]any{"allowlist": []any{"tools__search_files"}}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "input tool is required")
}

func TestAgentBMCPCallRequiresAllowlist(t *testing.T) {
	input := map[string]any{"server_id": "tools", "tool": "search_files"}
	_, err := runMCPCall(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: input, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "allowlist is required")
	_, err = runMCPCall(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: input,
		Config: map[string]any{"allowlist": []any{}}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "allowlist is required")
}

func TestAgentBMCPCallArgumentsMustBeObject(t *testing.T) {
	_, err := runMCPCall(context.Background(), &RunContext{Principal: ownerPrincipal(),
		Input:  map[string]any{"server_id": "tools", "tool": "search_files", "arguments": "not-an-object"},
		Config: map[string]any{"allowlist": []any{"tools__search_files"}}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "arguments must be an object")
}

func TestAgentBMCPCallArgumentsTooLarge(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "search_files",
			"arguments": map[string]any{"big": strings.Repeat("x", 70000)}},
		map[string]any{"allowlist": []any{"tools__search_files"}}, newFakeBackend(), false)
	agentBRequireNodeError(t, err, "validation_error", "exceed 64 KiB")
}

func TestAgentBMCPCallListError(t *testing.T) {
	backend := &agentBNodeErrBackend{FakeBackend: newFakeBackend(), mcpListErr: errors.New("agentb: list boom")}
	err := agentBExecErr(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "search_files"},
		map[string]any{"allowlist": []any{"tools__search_files"}}, backend, false)
	if err == nil || !strings.Contains(err.Error(), "list boom") {
		t.Fatalf("MCPCall list error = %v", err)
	}
}

func TestAgentBMCPCallBackendError(t *testing.T) {
	backend := &agentBNodeErrBackend{FakeBackend: newFakeBackend(), mcpCallErr: errors.New("agentb: call boom")}
	err := agentBExecErr(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "search_files"},
		map[string]any{"allowlist": []any{"tools__search_files"}}, backend, false)
	if err == nil || !strings.Contains(err.Error(), "call boom") {
		t.Fatalf("MCPCall backend error = %v", err)
	}
}

// ---- nodes_provider.go ----

func TestAgentBProviderHealthRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.provider.health", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model"}, nil, nil, false)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBProviderHealthRequiresDeployment(t *testing.T) {
	_, err := runProviderHealth(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: map[string]any{}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "deployment_id is required")
}

func TestAgentBProviderHealthBackendError(t *testing.T) {
	backend := &agentBNodeErrBackend{FakeBackend: newFakeBackend(), healthErr: errors.New("agentb: health boom")}
	err := agentBExecErr(t, "nexaroute.provider.health", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model"}, nil, backend, false)
	if err == nil || !strings.Contains(err.Error(), "health boom") {
		t.Fatalf("ProviderHealth backend error = %v", err)
	}
}

func TestAgentBProviderCompatRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.provider.compat", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model"}, nil, nil, false)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBProviderCompatRequiresDeployment(t *testing.T) {
	_, err := runProviderCompat(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: map[string]any{}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "deployment_id is required")
}

func TestAgentBProviderCompatBadRequiredCapabilities(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.provider.compat", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model", "required_capabilities": []any{float64(1)}},
		nil, newFakeBackend(), false)
	agentBRequireNodeError(t, err, "validation_error", "required_capabilities must be an array of strings")
}

func TestAgentBProviderCompatBackendError(t *testing.T) {
	backend := &agentBNodeErrBackend{FakeBackend: newFakeBackend(), compatErr: errors.New("agentb: compat boom")}
	err := agentBExecErr(t, "nexaroute.provider.compat", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model"}, nil, backend, false)
	if err == nil || !strings.Contains(err.Error(), "compat boom") {
		t.Fatalf("Compat backend error = %v", err)
	}
}

// ---- nodes_trigger.go ----

func TestAgentBHTTPRequestBackendError(t *testing.T) {
	backend := newFakeBackend()
	backend.HTTPErr = errors.New("agentb: http boom")
	err := agentBExecErr(t, "nexaroute.http.request", ownerPrincipal(),
		map[string]any{"url": "http://127.0.0.1:9/echo"}, nil, backend, true)
	if err == nil || !strings.Contains(err.Error(), "http boom") {
		t.Fatalf("HTTPDo backend error = %v", err)
	}
}

// ---- nodes_video.go ----

func TestAgentBVideoSubmitRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "a cat", "duration_seconds": float64(5)}, nil, nil, true)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBVideoSubmitValidatesPrompt(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "", "duration_seconds": float64(5)}, nil, newFakeBackend(), true)
	agentBRequireNodeError(t, err, "validation_error", "prompt must be non-empty")
	err = agentBExecErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": strings.Repeat("x", 4001), "duration_seconds": float64(5)}, nil, newFakeBackend(), true)
	agentBRequireNodeError(t, err, "validation_error", "prompt must be non-empty")
}

func TestAgentBVideoSubmitValidatesMode(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "a cat", "mode": "sideways", "duration_seconds": float64(5)}, nil, newFakeBackend(), true)
	agentBRequireNodeError(t, err, "validation_error", "unknown video mode")
}

func TestAgentBVideoSubmitValidatesAspect(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "a cat", "mode": "text_to_video", "duration_seconds": float64(5), "aspect_ratio": "21:9"}, nil, newFakeBackend(), true)
	agentBRequireNodeError(t, err, "validation_error", "unknown aspect_ratio")
}

func TestAgentBVideoSubmitBackendError(t *testing.T) {
	backend := &agentBNodeErrBackend{FakeBackend: newFakeBackend(), videoSubmitErr: errors.New("agentb: submit boom")}
	err := agentBExecErr(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "a cat", "duration_seconds": float64(5)}, nil, backend, true)
	if err == nil || !strings.Contains(err.Error(), "submit boom") {
		t.Fatalf("VideoSubmit backend error = %v", err)
	}
}

func TestAgentBVideoStatusRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.status", ownerPrincipal(),
		map[string]any{"job_id": "job-1"}, nil, nil, false)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBVideoStatusRequiresJobID(t *testing.T) {
	_, err := runVideoStatus(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: map[string]any{}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "job_id is required")
}

func TestAgentBVideoStatusBackendError(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.status", ownerPrincipal(),
		map[string]any{"job_id": "no-such-job"}, nil, newFakeBackend(), false)
	if err == nil || !strings.Contains(err.Error(), "no-such-job") {
		t.Fatalf("VideoStatus backend error = %v", err)
	}
}

func TestAgentBVideoOutputRequiresBackend(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.output", ownerPrincipal(),
		map[string]any{"job_id": "job-1"}, nil, nil, false)
	agentBRequireNodeError(t, err, "backend_unavailable", "")
}

func TestAgentBVideoOutputRequiresJobID(t *testing.T) {
	_, err := runVideoOutput(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: map[string]any{}, Backend: newFakeBackend()})
	agentBRequireNodeError(t, err, "validation_error", "job_id is required")
}

func TestAgentBVideoOutputStatusError(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.output", ownerPrincipal(),
		map[string]any{"job_id": "no-such-job"}, nil, newFakeBackend(), false)
	if err == nil || !strings.Contains(err.Error(), "no-such-job") {
		t.Fatalf("VideoOutput status error = %v", err)
	}
}

func TestAgentBVideoOutputAssetError(t *testing.T) {
	err := agentBExecErr(t, "nexaroute.video.output", ownerPrincipal(),
		map[string]any{"job_id": "job-1", "asset_id": "a2"}, nil, newFakeBackend(), false)
	if err == nil || !strings.Contains(err.Error(), "a2") {
		t.Fatalf("VideoOutput asset error = %v", err)
	}
}

// ---- studio_port.go ----

// agentBCustomRegistry builds a registry from the given specs
// (copies are taken so DefaultRegistry's specs are never mutated).
func agentBCustomRegistry(t *testing.T, specs ...*NodeSpec) *Registry {
	t.Helper()
	reg, err := NewRegistry(specs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func agentBDefaultSpec(t *testing.T, id string) *NodeSpec {
	t.Helper()
	spec, err := DefaultRegistry().Lookup(id, SupportedVersion)
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	cp := *spec
	return &cp
}

func agentBStudioService(t *testing.T, reg *Registry, store Store) *Service {
	t.Helper()
	eng, err := NewEngine(EngineOptions{Store: store, Registry: reg, WorkerID: "agentb-studio"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, eng, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestAgentBStudioInstantiateLookupError(t *testing.T) {
	// Registry without the manual trigger: the first lookup in the
	// has-trigger scan fails.
	reg := agentBCustomRegistry(t,
		agentBDefaultSpec(t, "nexaroute.provider.health"),
		agentBDefaultSpec(t, "nexaroute.provider.compat"),
		agentBDefaultSpec(t, "nexaroute.control.condition"),
		agentBDefaultSpec(t, "nexaroute.control.branch"),
	)
	svc := agentBStudioService(t, reg, NewMemoryStore())
	_, err := svc.InstantiateTemplate(context.Background(), "model-health-alert", map[string]string{"deployment_id": "d1"}, true)
	if err == nil {
		t.Fatal("InstantiateTemplate with a registry missing the trigger node succeeded")
	}
	if !strings.Contains(err.Error(), "nexaroute.trigger.manual") {
		t.Fatalf("InstantiateTemplate error = %v, want it to name the missing trigger node", err)
	}
}

func TestAgentBStudioInstantiateTriggerLookupError(t *testing.T) {
	// Registry with the trigger (so the has-trigger scan breaks
	// early) but without the condition node: the second scan, which
	// looks every node up, fails on it.
	reg := agentBCustomRegistry(t,
		agentBDefaultSpec(t, "nexaroute.trigger.manual"),
		agentBDefaultSpec(t, "nexaroute.provider.health"),
		agentBDefaultSpec(t, "nexaroute.provider.compat"),
		agentBDefaultSpec(t, "nexaroute.control.branch"),
	)
	svc := agentBStudioService(t, reg, NewMemoryStore())
	_, err := svc.InstantiateTemplate(context.Background(), "model-health-alert", map[string]string{"deployment_id": "d1"}, true)
	if err == nil {
		t.Fatal("InstantiateTemplate with a registry missing the condition node succeeded")
	}
	if !strings.Contains(err.Error(), "nexaroute.control.condition") {
		t.Fatalf("InstantiateTemplate error = %v, want it to name the missing condition node", err)
	}
}

func TestAgentBStudioInstantiateUnknownTriggerKind(t *testing.T) {
	// Registry that miscategorizes the health node as a trigger:
	// the trigger-declaration scan finds a trigger-category node
	// with no v1 trigger kind and fails closed.
	health := agentBDefaultSpec(t, "nexaroute.provider.health")
	health.Category = "trigger"
	reg := agentBCustomRegistry(t,
		agentBDefaultSpec(t, "nexaroute.trigger.manual"),
		health,
		agentBDefaultSpec(t, "nexaroute.provider.compat"),
		agentBDefaultSpec(t, "nexaroute.control.condition"),
		agentBDefaultSpec(t, "nexaroute.control.branch"),
	)
	svc := agentBStudioService(t, reg, NewMemoryStore())
	_, err := svc.InstantiateTemplate(context.Background(), "model-health-alert", map[string]string{"deployment_id": "d1"}, true)
	if err == nil {
		t.Fatal("InstantiateTemplate with an unmapped trigger node succeeded")
	}
	if !strings.Contains(err.Error(), "has no v1 trigger kind") {
		t.Fatalf("InstantiateTemplate error = %v", err)
	}
}

func TestAgentBStudioInstantiateSaveDraftError(t *testing.T) {
	wrapped := &agentBStore{Store: NewMemoryStore(), saveDraftErr: errors.New("agentb: save boom")}
	svc := agentBStudioService(t, DefaultRegistry(), wrapped)
	_, err := svc.InstantiateTemplate(context.Background(), "model-health-alert", map[string]string{"deployment_id": "d1"}, true)
	if err == nil {
		t.Fatal("InstantiateTemplate with a failing store succeeded")
	}
	if !strings.Contains(err.Error(), "save boom") {
		t.Fatalf("InstantiateTemplate error = %v", err)
	}
}

func TestAgentBStudioSecretRefsSkips(t *testing.T) {
	reg := DefaultRegistry()
	def := &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "secrets-skips", Name: "secrets-skips",
		Nodes: []DefinitionNode{
			// Array-typed config field: skipped (not a secretRef).
			{ID: "mcp", NodeID: "nexaroute.mcp.call", Version: SupportedVersion,
				Config: map[string]any{"allowlist": []any{"tools__search_files"}}},
			// Non-string secretRef value: skipped.
			{ID: "hook", NodeID: "nexaroute.trigger.webhook", Version: SupportedVersion,
				Config: map[string]any{"secret_ref": 123}},
			// Empty secretRef value: skipped.
			{ID: "hook2", NodeID: "nexaroute.trigger.webhook", Version: SupportedVersion,
				Config: map[string]any{"secret_ref": ""}},
			// Unknown node spec: skipped.
			{ID: "ghost", NodeID: "test.missing", Version: SupportedVersion},
			// Real secretRef: kept.
			{ID: "call", NodeID: "nexaroute.http.request", Version: SupportedVersion,
				Config: map[string]any{"auth_secret_ref": "env:API_TOKEN"}},
		},
		Triggers: []DefinitionTrigger{
			// Trigger with no matching node: skipped.
			{Name: "nope", Kind: TriggerWebhook, Config: map[string]any{"secret_ref": "env:GHOST_REF"}},
			// Trigger whose node has an unknown spec: skipped.
			{Name: "ghost", Kind: TriggerWebhook, Config: map[string]any{"secret_ref": "env:GHOST_REF"}},
			// Trigger secretRef on a real node: kept (deduped
			// against the node's own config when identical).
			{Name: "hook", Kind: TriggerWebhook, Config: map[string]any{"secret_ref": "env:WEBHOOK_SECRET"}},
		},
	}
	refs := DefinitionSecretRefs(def, reg)
	if len(refs) != 2 || refs[0] != "env:API_TOKEN" || refs[1] != "env:WEBHOOK_SECRET" {
		t.Fatalf("secret refs = %v, want [env:API_TOKEN env:WEBHOOK_SECRET]", refs)
	}
}

func TestAgentBHTTPRequestRequiresBackend(t *testing.T) {
	// nodes_trigger.go 317-318: runHTTPRequest's requireBackend check
	// fires when no backend is wired at all.
	_, err := runHTTPRequest(context.Background(), &RunContext{Principal: ownerPrincipal(),
		Input: map[string]any{"url": "http://127.0.0.1:9/x"}})
	agentBRequireNodeError(t, err, "backend_unavailable", "no backend is wired for this node")
}
