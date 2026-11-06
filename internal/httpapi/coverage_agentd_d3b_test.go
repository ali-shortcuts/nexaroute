// Coverage tests for Agent D3 (100% mission): httpapi providers/wiring files.
// Part B: budget_alerts, decision_wiring, evaluation_plane, evaluation_state,
// media.
//
// Every test is behavioral: it asserts an observable status, body, error, or
// state — never mere execution. Production code is never edited by this file.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

func d3BoolPtr(v bool) *bool { return &v }

// d3WithTinyRLIMITFSIZE runs fn with the process file-size limit clamped to
// the given number of bytes, so file writes past that size fail with EFBIG
// instead of succeeding. SIGXFSZ is ignored for the duration so the process
// is not killed. The limit and the signal disposition are restored afterwards.
func d3WithTinyRLIMITFSIZE(t *testing.T, limit uint64, fn func()) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("d3WithTinyRLIMITFSIZE: getrlimit: %v", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: old.Max}); err != nil {
		t.Fatalf("d3WithTinyRLIMITFSIZE: setrlimit: %v", err)
	}
	defer func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
		signal.Reset(syscall.SIGXFSZ)
	}()
	fn()
}

// --- budget_alerts.go ---

// D3: fileBudgetAlertStore.Save surfaces a write failure
// (budget_alerts.go:178-180). The tiny RLIMIT_FSIZE makes the atomic temp
// file write fail with EFBIG.
func TestD3BudgetFileStoreWriteError(t *testing.T) {
	dir := t.TempDir()
	store := &fileBudgetAlertStore{path: filepath.Join(dir, "sub", "alerts.json")}
	var err error
	d3WithTinyRLIMITFSIZE(t, 16, func() {
		_, err = store.Save(map[string]int64{"k": 1}, 0)
	})
	if err == nil {
		t.Fatal("fileBudgetAlertStore.Save: write past RLIMIT_FSIZE succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "file too large") && !strings.Contains(err.Error(), "too large") {
		t.Fatalf("fileBudgetAlertStore.Save: error %q, want a file-size-limit error", err.Error())
	}
}

// D3 (evidence for budget_alerts.go:588-599, 602-603): a redirect to a
// cross-origin URL is refused by the egress guard before the redirect-target
// scheme checks in deliverBudgetAlert are ever reached.
func TestD3BudgetWebhookCrossOriginRedirectRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/hook", http.StatusFound)
	}))
	defer upstream.Close()
	payload := BudgetAlertPayload{BudgetID: "b1", ThresholdPercent: 80, SpendUSD: 8, LimitUSD: 10, Currency: "USD", Period: "day"}
	err := deliverBudgetAlert(context.Background(), payload, upstream.URL+"/hook",
		2*time.Second, config.EgressConfig{})
	if err == nil {
		t.Fatal("deliverBudgetAlert: cross-origin redirect accepted, want an error")
	}
	if !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("deliverBudgetAlert: error %q, want 'refused cross-origin redirect'", err.Error())
	}
}

// D3 (evidence for budget_alerts.go:602-603): a webhook URL the egress
// policy rejects is refused before any request is built.
func TestD3BudgetWebhookBadURLRefused(t *testing.T) {
	payload := BudgetAlertPayload{BudgetID: "b1", ThresholdPercent: 80, SpendUSD: 8, LimitUSD: 10, Currency: "USD", Period: "day"}
	err := deliverBudgetAlert(context.Background(), payload, "http://127.0.0.1:1/\x7f",
		2*time.Second, config.EgressConfig{})
	if err == nil {
		t.Fatal("deliverBudgetAlert: malformed webhook URL accepted, want an error")
	}
	if !strings.Contains(err.Error(), "budget webhook refused") {
		t.Fatalf("deliverBudgetAlert: error %q, want 'budget webhook refused'", err.Error())
	}
}

// --- decision_wiring.go ---

// D3: a hybrid decision event whose chain trace has an empty ChainID records
// an empty DecisionChainID (decision_wiring.go:298-299).
func TestD3DecisionHybridEmptyChainIDEvent(t *testing.T) {
	s := testGateway(t, config.Default())
	s.emitDecisionEvent("d3-dec",
		decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "a/m", ProviderID: "a"},
		decision.DecisionTrace{Mode: "hybrid", ChainTrace: &decision.ChainTrace{
			ChainID: "", Outcome: decision.ChainOutcomeSelected,
		}},
		nil, 2, false)
	events := s.bus.Snapshot()
	if len(events) == 0 {
		t.Fatal("emitDecisionEvent: no event recorded")
	}
	ev := events[len(events)-1]
	if ev.Kind != "decision_ok" {
		t.Fatalf("emitDecisionEvent: kind %q, want \"decision_ok\"", ev.Kind)
	}
	if ev.DecisionChainID != "" {
		t.Fatalf("emitDecisionEvent: DecisionChainID %q, want empty", ev.DecisionChainID)
	}
	if ev.DecisionSelectedProvider != "a" {
		t.Fatalf("emitDecisionEvent: DecisionSelectedProvider %q, want \"a\"", ev.DecisionSelectedProvider)
	}
}

// D3: with no orchestrator, applyDecisionPlane returns the candidates
// unchanged (decision_wiring.go:349-350).
func TestD3DecisionOrchestratorNil(t *testing.T) {
	s := testGateway(t, config.Default())
	s.decisionOrchestrator = nil
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "a/m"}},
		{Deployment: router.Deployment{ID: "b/m"}},
	}
	got := s.applyDecisionPlane(context.Background(), candidates, taskIntelligence{}, nil, "d3-req", router.Requirement{})
	if len(got) != 2 || got[0].Deployment.ID != "a/m" || got[1].Deployment.ID != "b/m" {
		t.Fatalf("applyDecisionPlane (orch nil): got %v, want [a/m b/m] in order", got)
	}
}

// d3HybridConfig returns a config with hybrid decision mode and no chain, so
// Decide fails open with the cloned candidate order.
func d3HybridConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = ""
	cfg.Decision.MaxProviderCalls = 0
	return cfg
}

// D3: in hybrid mode, RequireNoTraining drops the training candidate and the
// single survivor is returned early (decision_wiring.go:361-367).
func TestD3DecisionRequireNoTrainingFilter(t *testing.T) {
	s := testGateway(t, d3HybridConfig(t))
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "a/m", TrainsOnData: "yes"}},
		{Deployment: router.Deployment{ID: "b/m", TrainsOnData: "no"}},
	}
	got := s.applyDecisionPlane(context.Background(), candidates, taskIntelligence{}, nil, "d3-req",
		router.Requirement{RequireNoTraining: true})
	if len(got) != 1 || got[0].Deployment.ID != "b/m" {
		t.Fatalf("applyDecisionPlane (no-training): got %v, want only [b/m]", got)
	}
}

// D3: in hybrid mode with MaxProviderCalls==0 and an unknown chain ID, Decide
// fails open and the candidate order is preserved (decision_wiring.go:413-414).
func TestD3DecisionHybridBudgetZeroFailsOpen(t *testing.T) {
	s := testGateway(t, d3HybridConfig(t))
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "a/m", TrainsOnData: "no"}},
		{Deployment: router.Deployment{ID: "b/m", TrainsOnData: "no"}},
	}
	got := s.applyDecisionPlane(context.Background(), candidates, taskIntelligence{}, nil, "d3-req",
		router.Requirement{RequireNoTraining: true})
	if len(got) != 2 || got[0].Deployment.ID != "a/m" || got[1].Deployment.ID != "b/m" {
		t.Fatalf("applyDecisionPlane (hybrid budget 0): got %v, want [a/m b/m] in order", got)
	}
	if len(s.bus.Snapshot()) == 0 {
		t.Fatal("applyDecisionPlane (hybrid budget 0): no decision event recorded")
	}
}

// D3: taskDimensionFor maps long_context and structured_output to their
// scorecard dimensions (decision_wiring.go:504, 506).
func TestD3TaskDimensionForLongContextAndStructured(t *testing.T) {
	if got := taskDimensionFor("long_context"); got != scorecards.DimLongContext {
		t.Fatalf("taskDimensionFor(long_context): got %q, want %q", got, scorecards.DimLongContext)
	}
	if got := taskDimensionFor("structured_output"); got != scorecards.DimStructuredOutput {
		t.Fatalf("taskDimensionFor(structured_output): got %q, want %q", got, scorecards.DimStructuredOutput)
	}
}

// D3: taskQualityEvidence reports no evidence when the evaluation plane has
// no registry (decision_wiring.go:537-538).
func TestD3TaskQualityEvidenceNilRegistry(t *testing.T) {
	srv := &Server{evaluation: &evaluationPlane{}}
	_, _, ok := srv.taskQualityEvidence("coding", "d1")
	if ok {
		t.Fatal("taskQualityEvidence: ok=true with a nil registry, want false")
	}
}

// --- evaluation_plane.go ---

// d3UnixSocketPath creates a listening unix socket and returns its path. The
// socket file exists for Stat but cannot be read as a regular file.
func d3UnixSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("d3UnixSocketPath: listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

// D3: loadImport surfaces a read failure on a path that exists but is not a
// regular file (evaluation_plane.go:274-275).
func TestD3EvalLoadImportReadError(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{}, nil)
	err := plane.loadImport(d3UnixSocketPath(t))
	if err == nil {
		t.Fatal("loadImport: unreadable path accepted, want an error")
	}
	// loadImport returns the ReadFile error unwrapped: opening a unix socket
	// fails with ENXIO ("no such device or address").
	if !strings.Contains(err.Error(), "no such device or address") {
		t.Fatalf("loadImport: error %q, want ENXIO 'no such device or address'", err.Error())
	}
}

// D3: loadImport refuses an import that would exceed the registry bound
// (evaluation_plane.go:299-300).
func TestD3EvalLoadImportRegistryBound(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{MaxScorecards: 1}, nil)
	card, err := scorecards.FromOperatorConfig("d1/m", "p", "m", "scorecards.quality",
		[]scorecards.OperatorDeclaration{{Dimension: scorecards.DimCoding, Score: 0.9}})
	if err != nil {
		t.Fatalf("FromOperatorConfig: %v", err)
	}
	if _, err := plane.Registry().Upsert(card); err != nil {
		t.Fatalf("Registry().Upsert: %v", err)
	}
	doc, err := json.Marshal(map[string]any{"scorecards": []any{map[string]any{
		"deployment_id": "d2/m",
		"version":       1,
		"generated_at":  time.Now().UTC().Format(time.RFC3339),
		"values": map[string]any{"coding": map[string]any{
			"score": 0.9, "provenance": "operator_config", "sample_count": 0,
			"confidence": 0.8, "source": "d3",
		}},
	}}})
	if err != nil {
		t.Fatalf("marshal import doc: %v", err)
	}
	path := filepath.Join(t.TempDir(), "import.json")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatalf("write import doc: %v", err)
	}
	err = plane.loadImport(path)
	if err == nil {
		t.Fatal("loadImport: import exceeding the 1-deployment bound accepted, want an error")
	}
	if !strings.Contains(err.Error(), "would exceed the 1-deployment registry bound") {
		t.Fatalf("loadImport: error %q, want 'would exceed the 1-deployment registry bound'", err.Error())
	}
}

// --- evaluation_state.go ---

// D3: loadState surfaces a read failure on a path that exists but is not a
// regular file (evaluation_state.go:57-58).
func TestD3EvalLoadStateReadError(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{}, nil)
	err := plane.loadState(d3UnixSocketPath(t))
	if err == nil {
		t.Fatal("loadState: unreadable path accepted, want an error")
	}
	// loadState returns the ReadFile error unwrapped: opening a unix socket
	// fails with ENXIO ("no such device or address").
	if !strings.Contains(err.Error(), "no such device or address") {
		t.Fatalf("loadState: error %q, want ENXIO 'no such device or address'", err.Error())
	}
}

// d3FatScorecard builds a scorecard near the per-card size limit: a value
// for every dimension with a 256-byte source, plus 16 notes of 256 bytes.
func d3FatScorecard(i int) scorecards.Scorecard {
	values := make(map[scorecards.Dimension]scorecards.Value, len(scorecards.AllDimensions))
	for _, d := range scorecards.AllDimensions {
		values[d] = scorecards.Value{
			Score:       0.9,
			Confidence:  0.8,
			SampleCount: 0,
			Provenance:  scorecards.ProvenanceOperatorConfig,
			Source:      strings.Repeat("s", scorecards.MaxStringBytes),
		}
	}
	notes := make([]string, 0, 16)
	for n := 0; n < 16; n++ {
		notes = append(notes, strings.Repeat("n", scorecards.MaxStringBytes))
	}
	return scorecards.Scorecard{
		DeploymentID: fmt.Sprintf("d%04d/m", i),
		Version:      1,
		GeneratedAt:  time.Now().UTC(),
		Values:       values,
		Notes:        notes,
	}
}

// D3: persist refuses to write a state document larger than the safe size
// and leaves no state file behind (evaluation_state.go:142-143).
func TestD3EvalPersistOversize(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	plane := newEvaluationPlane(config.EvaluationConfig{StatePath: statePath}, nil)
	for i := 0; i < 900; i++ {
		if _, err := plane.Registry().Upsert(d3FatScorecard(i)); err != nil {
			t.Fatalf("Registry().Upsert(%d): %v", i, err)
		}
	}
	err := plane.persist()
	if err == nil {
		t.Fatal("persist: oversized state accepted, want an error")
	}
	if !strings.Contains(err.Error(), "evaluation state exceeds safe size") {
		t.Fatalf("persist: error %q, want 'evaluation state exceeds safe size'", err.Error())
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("persist: state file exists after a refused write (stat err %v)", statErr)
	}
}

// D3: persist surfaces a write failure on the atomic temp file
// (evaluation_state.go:162-164). The tiny RLIMIT_FSIZE makes the write fail
// with EFBIG.
func TestD3EvalPersistWriteError(t *testing.T) {
	dir := t.TempDir()
	plane := newEvaluationPlane(config.EvaluationConfig{StatePath: filepath.Join(dir, "state.json")}, nil)
	if err := plane.Store().Save(eval.Result{
		RunID: "r1", SuiteID: "s", SuiteVersion: "1", DeploymentID: "d/m", Score: 0.5,
	}); err != nil {
		t.Fatalf("Store().Save: %v", err)
	}
	var err error
	d3WithTinyRLIMITFSIZE(t, 64, func() {
		err = plane.persist()
	})
	if err == nil {
		t.Fatal("persist: write past RLIMIT_FSIZE succeeded, want an error")
	}
	// persist returns the temp-file write error unwrapped.
	if !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("persist: error %q, want 'file too large'", err.Error())
	}
}

// --- media.go ---

// d3MediaProvider returns a config with one openai_compatible provider whose
// fake upstream answers every path with the given status and body.
func d3MediaProvider(t *testing.T, status int, body string) config.Config {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID:       "d3-media",
		Type:     "openai_compatible",
		BaseURL:  upstream.URL,
		APIKey:   "test-key-1",
		AuthMode: "bearer",
		Enabled:  true,
		Models: []config.ModelConfig{{
			ID:      "d3-media-model",
			Model:   "shared-model",
			Aliases: []string{"media-model"},
			Enabled: true,
			Weight:  1,
			Capabilities: config.Capabilities{
				Streaming: true,
				Tools:     true,
			},
		}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	return cfg
}

// D3: azureMediaPath rejects an unknown media kind (media.go:151).
func TestD3MediaAzureUnknownKind(t *testing.T) {
	path, ok := azureMediaPath(config.ProviderConfig{}, mediaKind("bogus"), "dep")
	if ok || path != "" {
		t.Fatalf("azureMediaPath(bogus): got (%q, %v), want (\"\", false)", path, ok)
	}
}

// D3: a request for a disabled virtual endpoint returns 404
// (media.go:192-193).
func TestD3MediaDisabledVirtualEndpoint(t *testing.T) {
	cfg := d3MediaProvider(t, http.StatusOK,
		`{"data":[{"embedding":[0.1],"index":0}],"model":"shared-model","object":"list","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "all", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "all"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{
		ID: "ve1", PublicModel: "ve-model", RouteProfile: "rp1", Enabled: d3BoolPtr(false),
	}}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings",
		strings.NewReader(`{"model":"ve-model","input":"hi"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled VE embeddings: got status %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `virtual endpoint`) ||
		!strings.Contains(rr.Body.String(), `is disabled`) {
		t.Fatalf("disabled VE embeddings: body %q does not contain the disabled diagnostic", rr.Body.String())
	}
}

// d3VirtualEndpointConfig wires a virtual endpoint "ve-model" over pool "all"
// with the given fallback setting.
func d3VirtualEndpointConfig(t *testing.T, cfg *config.Config, fallback bool) {
	t.Helper()
	cfg.Routing.FallbackOnUnknownModel = fallback
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "all", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "all"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{
		ID: "ve1", PublicModel: "ve-model", RouteProfile: "rp1", Enabled: d3BoolPtr(true),
	}}
}

// D3: with FallbackOnUnknownModel=false, a media request through a virtual
// endpoint has every candidate skipped because the public model is never
// matched (media.go:229). The chat control through the same endpoint
// succeeds, proving the divergence: the chat handlers blank the model for
// virtual routes, the media handler does not.
func TestD3MediaVirtualEndpointCandidateSkipped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/chat/completions"):
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1],"index":0}],"model":"shared-model","object":"list","usage":{"prompt_tokens":1,"total_tokens":1}}`))
		}
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID:       "d3-media",
		Type:     "openai_compatible",
		BaseURL:  upstream.URL,
		APIKey:   "test-key-1",
		AuthMode: "bearer",
		Enabled:  true,
		Models: []config.ModelConfig{{
			ID:      "d3-media-model",
			Model:   "shared-model",
			Enabled: true,
			Weight:  1,
			Capabilities: config.Capabilities{
				Streaming: true,
				Tools:     true,
			},
		}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	d3VirtualEndpointConfig(t, &cfg, false)
	s := testGateway(t, cfg)

	// Media path: every candidate is skipped -> 503.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings",
		strings.NewReader(`{"model":"ve-model","input":"hi"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("media VE (fallback off): got status %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("media VE (fallback off): body %q does not contain 'no compatible healthy deployment'", rr.Body.String())
	}

	// Chat control through the same virtual endpoint succeeds.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"ve-model","messages":[{"role":"user","content":"hi"}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("chat VE control (fallback off): got status %d, want 200; body %q", rr.Code, rr.Body.String())
	}
}

// D3: a speech response larger than 64 MiB is rejected (media.go:290-291).
func TestD3MediaSpeechResponseTooLarge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1<<20)
		for i := 0; i < 65; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		if _, err := w.Write([]byte{0}); err != nil {
			return
		}
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID:       "d3-media",
		Type:     "openai_compatible",
		BaseURL:  upstream.URL,
		APIKey:   "test-key-1",
		AuthMode: "bearer",
		Enabled:  true,
		Models: []config.ModelConfig{{
			ID:      "d3-media-model",
			Model:   "shared-model",
			Aliases: []string{"media-model"},
			Enabled: true,
			Weight:  1,
			Capabilities: config.Capabilities{
				Streaming: true,
				Tools:     true,
			},
		}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/audio/speech",
		strings.NewReader(`{"model":"media-model","input":"hi","voice":"alloy"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("speech oversize: got status %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "upstream media response exceeds") {
		t.Fatalf("speech oversize: body %q does not contain 'upstream media response exceeds'", rr.Body.String())
	}
}

// D3: an upstream 500 is passed through with the upstream body
// (media.go:351-352).
func TestD3MediaUpstreamErrorPassthrough(t *testing.T) {
	cfg := d3MediaProvider(t, http.StatusInternalServerError, `{"error":"boom"}`)
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings",
		strings.NewReader(`{"model":"media-model","input":"hi"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("media upstream 500: got status %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "boom") {
		t.Fatalf("media upstream 500: body %q does not contain the upstream error", rr.Body.String())
	}
}

// D3: rewriteMultipartModel rejects a multipart body with no model field
// (media.go:448-449).
func TestD3MediaRewriteMultipartNoModel(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("audio")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	_, _, err = rewriteMultipartModel(buf.Bytes(), mw.FormDataContentType(), "dep-model")
	if err == nil {
		t.Fatal("rewriteMultipartModel: body without a model field accepted, want an error")
	}
	if !strings.Contains(err.Error(), "model form field is required") {
		t.Fatalf("rewriteMultipartModel: error %q, want 'model form field is required'", err.Error())
	}
}

// D3: a transcriptions request whose model part carries a filename is
// rejected: mediaRequestModel accepts the part, but rewriteMultipartModel
// only replaces filename-less model parts (media.go:448-449).
func TestD3MediaTranscriptionsModelPartWithFilename(t *testing.T) {
	cfg := d3MediaProvider(t, http.StatusOK, `{"text":"hello"}`)
	s := testGateway(t, cfg)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	modelPart, err := mw.CreateFormFile("model", "m.txt")
	if err != nil {
		t.Fatalf("CreateFormFile(model): %v", err)
	}
	if _, err := modelPart.Write([]byte("media-model")); err != nil {
		t.Fatalf("write model part: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatalf("CreateFormFile(file): %v", err)
	}
	if _, err := fw.Write([]byte("audio")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("transcriptions model-with-filename: got status %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "model form field is required") {
		t.Fatalf("transcriptions model-with-filename: body %q does not contain 'model form field is required'", rr.Body.String())
	}
}

// D3: rewriteMultipartModel surfaces a copy error from a truncated file part
// (media.go:440-441). The body ends mid-part with no closing boundary.
func TestD3MediaRewriteMultipartTruncated(t *testing.T) {
	raw := "--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nmedia-model\r\n" +
		"--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\nPARTIAL"
	_, _, err := rewriteMultipartModel([]byte(raw), "multipart/form-data; boundary=B", "dep-model")
	if err == nil {
		t.Fatal("rewriteMultipartModel: truncated multipart accepted, want an error")
	}
	// rewriteMultipartModel returns the io.Copy error unwrapped.
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("rewriteMultipartModel: error %q, want 'unexpected EOF'", err.Error())
	}
}

// --- round 2: lines the full-package profile showed the round-1 tests
// reached only via neighbouring paths ---

// D3: in hybrid mode with MaxProviderCalls==0, the budget block takes the
// zero branch (decision_wiring.go:413-414). ApplyDefaults clamps
// MaxProviderCalls to >= 1 (config.go:1794-1802), so the zero value is only
// reachable when the server's decision config bypasses ApplyDefaults; the
// test sets it directly on the constructed server, which is exactly the
// state the defensive branch guards.
func TestD3DecisionHybridMaxCallsZero(t *testing.T) {
	s := testGateway(t, d3HybridConfig(t))
	s.runtimeMu.Lock()
	s.cfg.Decision.MaxProviderCalls = 0
	s.runtimeMu.Unlock()
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "a/m", TrainsOnData: "no"}},
		{Deployment: router.Deployment{ID: "b/m", TrainsOnData: "no"}},
	}
	got := s.applyDecisionPlane(context.Background(), candidates, taskIntelligence{}, nil, "d3-req",
		router.Requirement{RequireNoTraining: true})
	if len(got) != 2 || got[0].Deployment.ID != "a/m" || got[1].Deployment.ID != "b/m" {
		t.Fatalf("applyDecisionPlane (hybrid maxCalls 0): got %v, want [a/m b/m] in order", got)
	}
	if len(s.bus.Snapshot()) == 0 {
		t.Fatal("applyDecisionPlane (hybrid maxCalls 0): no decision event recorded")
	}
}

// D3: loadImport surfaces an Upsert failure for a card the capacity
// pre-check cannot see (evaluation_plane.go:299-300). The pre-check counts
// only deployment-wide cards, so a task-scoped card for an already-known
// deployment passes it — but Upsert keys by (task, deployment), finds a new
// key, and fails against the full registry.
func TestD3EvalLoadImportUpsertFull(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{MaxScorecards: 1}, nil)
	card, err := scorecards.FromOperatorConfig("d1/m", "p", "m", "scorecards.quality",
		[]scorecards.OperatorDeclaration{{Dimension: scorecards.DimCoding, Score: 0.9}})
	if err != nil {
		t.Fatalf("FromOperatorConfig: %v", err)
	}
	if _, err := plane.Registry().Upsert(card); err != nil {
		t.Fatalf("Registry().Upsert: %v", err)
	}
	doc, err := json.Marshal(map[string]any{"scorecards": []any{map[string]any{
		"deployment_id": "d1/m",
		"task_type":     "coding",
		"version":       1,
		"generated_at":  time.Now().UTC().Format(time.RFC3339),
		"values": map[string]any{"coding": map[string]any{
			"score": 0.9, "provenance": "operator_config", "sample_count": 0,
			"confidence": 0.8, "source": "d3",
		}},
	}}})
	if err != nil {
		t.Fatalf("marshal import doc: %v", err)
	}
	path := filepath.Join(t.TempDir(), "import.json")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatalf("write import doc: %v", err)
	}
	err = plane.loadImport(path)
	if err == nil {
		t.Fatal("loadImport: task-scoped card into a full registry accepted, want an error")
	}
	if !strings.Contains(err.Error(), `scorecard "d1/m": scorecard registry is full (1 deployments)`) {
		t.Fatalf("loadImport: error %q, want the wrapped registry-full error", err.Error())
	}
}

// D3: a request for a virtual endpoint that does not allow the media
// protocol returns 400 (media.go:192-193).
func TestD3MediaVirtualEndpointProtocolMismatch(t *testing.T) {
	cfg := d3MediaProvider(t, http.StatusOK,
		`{"data":[{"embedding":[0.1],"index":0}],"model":"shared-model","object":"list","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "all", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "all"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{
		ID: "ve1", PublicModel: "ve-model", RouteProfile: "rp1",
		Enabled: d3BoolPtr(true), Protocols: []string{"anthropic"},
	}}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings",
		strings.NewReader(`{"model":"ve-model","input":"hi"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("VE protocol mismatch: got status %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not allowed for virtual endpoint") {
		t.Fatalf("VE protocol mismatch: body %q does not contain 'not allowed for virtual endpoint'", rr.Body.String())
	}
}

// D3: a candidate whose provider config is missing from the live config is
// skipped (media.go:229). The router still holds the deployment (built at
// server construction), but the config snapshot no longer has the provider —
// the divergence the guard exists for, simulated by dropping the provider
// from the server config and resetting the cached snapshot.
func TestD3MediaProviderConfigMissing(t *testing.T) {
	cfg := d3MediaProvider(t, http.StatusOK,
		`{"data":[{"embedding":[0.1],"index":0}],"model":"shared-model","object":"list","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	s := testGateway(t, cfg)
	s.runtimeMu.Lock()
	s.cfg.Providers = nil
	s.runtimeMu.Unlock()
	s.cfgSnap.Store(nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings",
		strings.NewReader(`{"model":"media-model","input":"hi"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("media provider-config missing: got status %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("media provider-config missing: body %q does not contain 'no compatible healthy deployment'", rr.Body.String())
	}
}

// D3: a rerank request through an Azure provider is refused with a 400:
// Azure OpenAI has no rerank equivalent, so the candidate is eligible but
// unsupported (media.go:329-335). This also documents why the
// lastErr=="" tail at media.go:351-352 is unreachable: an unsupported
// candidate always lands here (400) before the tail, and any attempted
// candidate sets lastErr.
func TestD3MediaAzureRerankRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unsupported media kind: upstream called at %s, want no upstream attempt", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID:       "d3-media",
		Type:     "azure_openai",
		BaseURL:  upstream.URL,
		APIKey:   "test-key-1",
		AuthMode: "api-key",
		Enabled:  true,
		Models: []config.ModelConfig{{
			ID:      "d3-media-model",
			Model:   "shared-model",
			Aliases: []string{"media-model"},
			Enabled: true,
			Weight:  1,
			Capabilities: config.Capabilities{
				Streaming: true,
				Tools:     true,
			},
		}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/rerank",
		strings.NewReader(`{"model":"media-model","query":"q","documents":["d1"]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("media azure rerank: got status %d, want 400; body %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "media endpoints are supported only for openai_compatible and azure_openai providers") {
		t.Fatalf("media azure rerank: body %q does not contain the unsupported-kind refusal", rr.Body.String())
	}
}
