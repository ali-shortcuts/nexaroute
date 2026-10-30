package httpapi

// Wave-2 coverage for the compatibility engine (issue #663,
// continuing PR #730). Targets the merged-tree profile's uncovered
// blocks in internal/httpapi/compat_engine.go via direct unit tests
// and one loopback dispatch, never via fabricated capability data.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func wave2CompatServer(t *testing.T) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, ContextWindow: 200000}},
	}}
	return testGateway(t, cfg)
}

func wave2CompatDeployment() router.Deployment {
	return router.Deployment{ID: "g/m", ProviderID: "g", ProviderType: "openai_compatible", Model: "upstream-model"}
}

func TestWave2CompatDialectFallback(t *testing.T) {
	s := wave2CompatServer(t)
	d := s.dialectFor("ghost", "openai_compatible", "http://127.0.0.1:9")
	if d.Name == "" {
		t.Fatal("dialect fallback must still identify a dialect")
	}
}

func TestWave2CompatSyncSkipsMissingProvider(t *testing.T) {
	s := wave2CompatServer(t)
	// Simulate the post-removal state: the router still lists the
	// deployment, but the snapshot config no longer has its provider.
	s.runtimeMu.Lock()
	s.cfg.Providers = nil
	s.runtimeMu.Unlock()
	s.SyncCapabilityContracts()
	if got := len(s.capStore.Snapshot()); got != 0 {
		t.Fatalf("contracts=%d want 0 (all dropped as stale)", got)
	}
}

func TestWave2CompatSyncLearnsNumeric(t *testing.T) {
	s := wave2CompatServer(t)
	s.SyncCapabilityContracts()
	if got := s.capStore.Get("g/m"); got.InvalidationKey == "" {
		t.Fatal("sync must seed a contract for g/m (ContextWindow 200000)")
	}
}

func TestWave2CompatLearnFromSuccessUnknownProvider(t *testing.T) {
	s := wave2CompatServer(t)
	// Must return without touching the store or panicking.
	s.learnFromSuccess("g/m", "ghost", wave2CompatDeployment(), []byte(`{}`), nil)
}

func TestWave2CompatLearnFromSuccessSkipsAndLearns(t *testing.T) {
	s := wave2CompatServer(t)
	s.capStore.LearnUnsupported("g/m", compat.CapTools, compat.SourceRuntime, "test", "")
	s.capStore.LearnUnsupported("g/m", compat.CapTemperature, compat.SourceRuntime, "test", "")
	canReq := &canonical.Request{Messages: []canonical.Message{{
		Role:  "user",
		Parts: []canonical.Part{{Type: canonical.PartImage}},
	}}}
	s.learnFromSuccess("g/m", "g", wave2CompatDeployment(),
		[]byte(`{"tools":[{"type":"function"}],"generationConfig":{"temperature":0.5},"toolConfig":{}}`),
		canReq)
	got := s.capStore.Get("g/m").Capabilities
	if got.Get(compat.CapTools) != compat.Unsupported {
		t.Fatal("tools must stay Unsupported (skip rule), not be re-learned")
	}
	if got.Get(compat.CapTemperature) != compat.Unsupported {
		t.Fatal("temperature must stay Unsupported (skip rule), not be re-learned")
	}
	if got.Get(compat.CapToolChoiceAuto) != compat.Supported {
		t.Fatal("tool_choice_auto must be learned from toolConfig")
	}
	if got.Get(compat.CapVision) != compat.Supported {
		t.Fatal("vision must be learned from the image part")
	}
}

type wave2ErrReader struct{}

func (wave2ErrReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (wave2ErrReader) Close() error               { return nil }

func TestWave2CompatMaybeRepairClamps(t *testing.T) {
	s := wave2CompatServer(t)
	for _, maxRepairs := range []int{-1, 5} {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`ok`))}
		_, _, out := s.maybeRepairUpstream(context.Background(), "req-1",
			hedgeAttemptBundle{c: router.Scored{Deployment: wave2CompatDeployment()}},
			[]byte(`{}`), resp, false, nil, maxRepairs, compat.RequirementProfile{})
		if out.Repaired || out.GaveUp {
			t.Fatalf("maxRepairs=%d: 200 response must not repair, out=%+v", maxRepairs, out)
		}
	}
}

func TestWave2CompatMaybeRepairReadError(t *testing.T) {
	s := wave2CompatServer(t)
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(wave2ErrReader{})}
	outResp, payload, out := s.maybeRepairUpstream(context.Background(), "req-1",
		hedgeAttemptBundle{c: router.Scored{Deployment: wave2CompatDeployment()}},
		[]byte(`{}`), resp, false, nil, 2, compat.RequirementProfile{})
	if outResp == nil || out.Repaired {
		t.Fatalf("read error must restore the response without repair, out=%+v", out)
	}
	if string(payload) != `{}` {
		t.Fatalf("payload=%s want unchanged", payload)
	}
}

func TestWave2CompatMaybeRepairDoPathRetry(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true}},
	}}
	s := testGateway(t, cfg)
	adapter, ok := s.reg.Get("g")
	if !ok {
		t.Fatal("adapter g missing")
	}
	resp400 := &http.Response{
		StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"temperature is not supported"}}`)),
	}
	outResp, _, out := s.maybeRepairUpstream(context.Background(), "req-1",
		hedgeAttemptBundle{c: router.Scored{Deployment: wave2CompatDeployment()}, a: adapter, path: "/custom/path"},
		[]byte(`{"temperature":0.5}`), resp400, false, nil, 2, compat.RequirementProfile{})
	if outResp == nil || outResp.StatusCode != http.StatusOK {
		t.Fatalf("repair retry status=%v want 200", outResp)
	}
	if !out.Repaired {
		t.Fatalf("out=%+v want Repaired", out)
	}
	if len(paths) != 1 || paths[0] != "/custom/path" {
		t.Fatalf("retry paths=%v want [/custom/path]", paths)
	}
}

func TestWave2CompatDoUpstreamRepairUnrepairable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported"}}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true}},
	}}
	s := testGateway(t, cfg)
	adapter, ok := s.reg.Get("g")
	if !ok {
		t.Fatal("adapter g missing")
	}
	// A non-JSON payload cannot be repaired: Repair fails on unmarshal,
	// so the capability failure is learned and the error returned
	// untouched (the semantics-critical branch).
	resp, payload, out, err := s.doUpstreamWithRepair(context.Background(), "req-1", adapter,
		wave2CompatDeployment(), []byte(`not json`), "", false, nil,
		compat.DialectProfile{}, compat.RequirementProfile{}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%v want 400 untouched", resp)
	}
	if string(payload) != `not json` || out.Repaired {
		t.Fatalf("payload=%s out=%+v want untouched, unrepaired", payload, out)
	}
	got := s.capStore.Get("g/m")
	if got.Capabilities.Get(compat.CapTemperature) != compat.Unsupported {
		t.Fatal("temperature must be learned Unsupported from the unrepairable failure")
	}
	if got.LastCompatibilityIssue == "" {
		t.Fatal("compatibility issue must be recorded")
	}
}

func TestWave2CompatFinishAttemptUnknownProvider(t *testing.T) {
	s := &Server{cfg: config.Default()}
	bundle := hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "ghost/m", ProviderID: "ghost"}}}
	if _, ok := s.finishCanonicalAttempt(bundle, canonical.Request{}, "openai_chat"); ok {
		t.Fatal("unknown provider must fail the canonical attempt")
	}
}

func wave2CompatCanReq() canonical.Request {
	return canonical.Request{
		Model: "m",
		Messages: []canonical.Message{{
			Role:  "user",
			Parts: []canonical.Part{{Type: canonical.PartText, Text: "hi"}},
		}},
	}
}

func TestWave2CompatFinishAttemptVertexDefaultLocation(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "v", Type: "vertex", BaseURL: "http://127.0.0.1:9", VertexProject: "test-proj", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true}},
	}}
	s := &Server{cfg: cfg}
	bundle := hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "v/m", ProviderID: "v", ProviderType: "vertex", Model: "gemini-2.0-flash"}}}
	out, ok := s.finishCanonicalAttempt(bundle, wave2CompatCanReq(), "gemini")
	if !ok {
		t.Fatal("vertex attempt must build")
	}
	if !strings.Contains(out.path, "locations/us-central1") || !strings.Contains(out.path, "test-proj") {
		t.Fatalf("path=%q want us-central1 default + test-proj", out.path)
	}
}

func TestWave2CompatFinishAttemptResponsesDefaultPath(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Type: "openai_responses", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true}},
	}}
	s := &Server{cfg: cfg}
	bundle := hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "g/m", ProviderID: "g", ProviderType: "openai_responses", Model: "upstream-model"}}}
	out, ok := s.finishCanonicalAttempt(bundle, wave2CompatCanReq(), "openai_responses")
	if !ok || out.path != "/v1/responses" {
		t.Fatalf("ok=%v path=%q want true /v1/responses", ok, out.path)
	}
}

func TestWave2CompatFinishAttemptAzureDefaultVersion(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "z", Type: "azure_openai", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true}},
	}}
	s := &Server{cfg: cfg}
	bundle := hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "z/m", ProviderID: "z", ProviderType: "azure_openai", Model: "upstream-model"}}}
	out, ok := s.finishCanonicalAttempt(bundle, wave2CompatCanReq(), "openai_chat")
	if !ok || !strings.Contains(out.path, "api-version=2024-10-21") {
		t.Fatalf("ok=%v path=%q want true + default api-version", ok, out.path)
	}
}

func TestWave2CompatSanitizeDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.SanitizeEnabled = false
	s := &Server{cfg: cfg}
	if got := s.sanitizeOutgoingPayload(hedgeAttemptBundle{}, []byte(`orig`), router.Requirement{}); string(got) != `orig` {
		t.Fatalf("disabled sanitizer must return the payload untouched, got %s", got)
	}
}

func TestWave2CompatAdapterTransportRedact(t *testing.T) {
	s := wave2CompatServer(t)
	adapter, ok := s.reg.Get("g")
	if !ok {
		t.Fatal("adapter g missing")
	}
	tr := adapterTransport{a: adapter}
	if got := tr.RedactBody([]byte(`hello`)); string(got) != `hello` {
		t.Fatalf("redact=%s want hello", got)
	}
}
