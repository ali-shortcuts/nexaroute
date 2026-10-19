package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// Phase 19 (issue #419), acceptance: with mode=apply on a test route, a
// coding-shaped request lands on the coding profile's pool and a long-context
// request on the long-context pool — proven with fake providers, traces
// matching. Suggest mode records without touching routing. Operator override
// wins over the client pin, which wins over the classifier.

const taskIntelCompletionBody = `{"id":"u","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

func taskIntelUpstream(t *testing.T, calls *atomic.Int64, marker string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Replace(taskIntelCompletionBody, `"ok"`, `"`+marker+`"`, 1))
	}))
}

func taskIntelProvider(id, model, baseURL string, priority int) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: "openai_compatible", BaseURL: baseURL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: model, Enabled: true, Priority: priority, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true},
		}},
	}
}

// taskIntelTestConfig builds a virtual endpoint whose route profile runs task
// intelligence in the given mode, with optional operator override.
func taskIntelTestConfig(codeURL, ctxURL, mode, override string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.HedgingEnabled = false
	cfg.Providers = []config.ProviderConfig{
		taskIntelProvider("codeprov", "code-model", codeURL, 0),
		taskIntelProvider("ctxprov", "ctx-model", ctxURL, 1),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "code-pool", Deployments: []string{"code-model"}},
		{ID: "ctx-pool", Deployments: []string{"ctx-model"}},
		{ID: "default-pool", Deployments: []string{"code-model", "ctx-model"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{{
		ID: "rp-task", CandidatePool: "default-pool",
		TaskIntelMode: mode, TaskOverride: override,
		TaskPools: map[string]string{"coding": "code-pool", "long_context": "ctx-pool"},
	}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{
		ID: "ve-task", PublicModel: "task-model", RouteProfile: "rp-task",
	}}
	return cfg
}

// taskIntelBody builds a minimal chat-completions body via encoding/json so
// fixtures never carry hand-rolled escaping bugs.
func taskIntelBody(t testing.TB, content string) string {
	t.Helper()
	m := map[string]any{"model": "task-model", "messages": []any{map[string]any{"role": "user", "content": content}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func taskIntelCodingBody(t testing.TB) string {
	t.Helper()
	return taskIntelBody(t, "Explain what this Go function does:\n```go\npackage main\n\nfunc greet(name string) string {\n\treturn \"hello \" + name\n}\n```")
}

func taskIntelLongCtxBody(t testing.TB) string {
	t.Helper()
	return taskIntelBody(t, strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor ", 600))
}

func taskIntelPost(t *testing.T, srv *Server, reqID, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-request-id", reqID)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// taskClassifiedEvent finds the task_classified event for a request.
func taskClassifiedEvent(t *testing.T, srv *Server, reqID string) events.Event {
	t.Helper()
	for _, ev := range srv.bus.Snapshot() {
		if ev.Kind == "task_classified" && ev.RequestID == reqID {
			return ev
		}
	}
	t.Fatalf("no task_classified event for %s", reqID)
	return events.Event{}
}

// routeResolvedEvent finds the route_resolved event for a request.
func routeResolvedEvent(t *testing.T, srv *Server, reqID string) events.Event {
	t.Helper()
	for _, ev := range srv.bus.Snapshot() {
		if ev.Kind == "route_resolved" && ev.RequestID == reqID {
			return ev
		}
	}
	t.Fatalf("no route_resolved event for %s", reqID)
	return events.Event{}
}

func TestTaskIntelApply_CodingLandsOnCodePool(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", ""))
	srv.SyncCapabilityContracts()

	rr := taskIntelPost(t, srv, "ti-apply-coding", taskIntelCodingBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "code-answer") {
		t.Fatalf("expected code-pool answer, got %s", rr.Body.String())
	}
	if codeCalls.Load() != 1 || ctxCalls.Load() != 0 {
		t.Fatalf("expected exactly one code-pool call, got code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	// Trace matches: classification recorded with the applied decision and pool.
	tev := taskClassifiedEvent(t, srv, "ti-apply-coding")
	if tev.TaskType != "coding" {
		t.Fatalf("task_type=%q want coding", tev.TaskType)
	}
	if tev.TaskIntelMode != "apply" || tev.TaskIntelOutcome != "applied" {
		t.Fatalf("mode/outcome=%q/%q want apply/applied", tev.TaskIntelMode, tev.TaskIntelOutcome)
	}
	if tev.TaskIntelPool != "code-pool" || tev.Pool != "code-pool" {
		t.Fatalf("task pool=%q event pool=%q want code-pool", tev.TaskIntelPool, tev.Pool)
	}
	rev := routeResolvedEvent(t, srv, "ti-apply-coding")
	if rev.Pool != "code-pool" {
		t.Fatalf("route_resolved pool=%q want code-pool", rev.Pool)
	}
}

func TestTaskIntelApply_LongContextLandsOnCtxPool(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", ""))
	srv.SyncCapabilityContracts()

	rr := taskIntelPost(t, srv, "ti-apply-longctx", taskIntelLongCtxBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ctx-answer") {
		t.Fatalf("expected ctx-pool answer, got %s", rr.Body.String())
	}
	if codeCalls.Load() != 0 || ctxCalls.Load() != 1 {
		t.Fatalf("expected exactly one ctx-pool call, got code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	tev := taskClassifiedEvent(t, srv, "ti-apply-longctx")
	if tev.TaskType != "long_context" || tev.TaskIntelOutcome != "applied" || tev.TaskIntelPool != "ctx-pool" {
		t.Fatalf("trace mismatch: type=%q outcome=%q pool=%q", tev.TaskType, tev.TaskIntelOutcome, tev.TaskIntelPool)
	}
}

func TestTaskIntelSuggest_RecordsWithoutRouting(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "suggest", ""))
	srv.SyncCapabilityContracts()

	rr := taskIntelPost(t, srv, "ti-suggest-coding", taskIntelCodingBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Routing untouched: default pool serves (codeprov has the higher priority).
	if codeCalls.Load() != 1 || ctxCalls.Load() != 0 {
		t.Fatalf("suggest mode must not change routing: code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	tev := taskClassifiedEvent(t, srv, "ti-suggest-coding")
	if tev.TaskIntelMode != "suggest" || tev.TaskIntelOutcome != "suggested" {
		t.Fatalf("mode/outcome=%q/%q want suggest/suggested", tev.TaskIntelMode, tev.TaskIntelOutcome)
	}
	if tev.TaskIntelPool != "code-pool" {
		t.Fatalf("suggestion should name the pool it would use: %q", tev.TaskIntelPool)
	}
	rev := routeResolvedEvent(t, srv, "ti-suggest-coding")
	if rev.Pool != "default-pool" {
		t.Fatalf("route_resolved pool=%q want default-pool (unchanged)", rev.Pool)
	}
}

func TestTaskIntelPin_BeatsClassifier(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", ""))
	srv.SyncCapabilityContracts()

	// Coding-shaped body, but the client pins long_context.
	rr := taskIntelPost(t, srv, "ti-pin", taskIntelCodingBody(t), map[string]string{TaskPinHeader: "long_context"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if codeCalls.Load() != 0 || ctxCalls.Load() != 1 {
		t.Fatalf("pin must beat the classifier: code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	tev := taskClassifiedEvent(t, srv, "ti-pin")
	if tev.TaskIntelOutcome != "pinned" || tev.TaskPin != "long_context" || tev.TaskIntelPool != "ctx-pool" {
		t.Fatalf("pin not recorded: outcome=%q pin=%q pool=%q", tev.TaskIntelOutcome, tev.TaskPin, tev.TaskIntelPool)
	}
	if tev.TaskType != "coding" {
		t.Fatalf("classifier output must still be recorded honestly: type=%q", tev.TaskType)
	}
}

func TestTaskIntelInvalidPin_Ignored(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", ""))
	srv.SyncCapabilityContracts()

	rr := taskIntelPost(t, srv, "ti-badpin", taskIntelCodingBody(t), map[string]string{TaskPinHeader: "not-a-task"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Invalid pin ignored: classifier (coding) applies.
	if codeCalls.Load() != 1 || ctxCalls.Load() != 0 {
		t.Fatalf("invalid pin must be ignored: code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	tev := taskClassifiedEvent(t, srv, "ti-badpin")
	if tev.TaskPin != "" || tev.TaskIntelOutcome != "applied" {
		t.Fatalf("invalid pin leaked: pin=%q outcome=%q", tev.TaskPin, tev.TaskIntelOutcome)
	}
}

func TestTaskIntelOverride_WinsOverPin(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	// Operator override forces long_context for the whole route profile.
	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", "long_context"))
	srv.SyncCapabilityContracts()

	// Coding-shaped body + a coding pin: the operator override still wins.
	rr := taskIntelPost(t, srv, "ti-override", taskIntelCodingBody(t), map[string]string{TaskPinHeader: "coding"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if codeCalls.Load() != 0 || ctxCalls.Load() != 1 {
		t.Fatalf("operator override must win over pin: code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	tev := taskClassifiedEvent(t, srv, "ti-override")
	if tev.TaskIntelOutcome != "overridden" || tev.TaskIntelPool != "ctx-pool" {
		t.Fatalf("override not recorded: outcome=%q pool=%q", tev.TaskIntelOutcome, tev.TaskIntelPool)
	}
}

func TestTaskIntelUnmapped_NoRoutingChange(t *testing.T) {
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", ""))
	srv.SyncCapabilityContracts()

	// "hi" classifies as simple_chat (high confidence) but no pool is mapped
	// for it: recorded as unmapped, routing unchanged.
	body := `{"model":"task-model","messages":[{"role":"user","content":"hi"}]}`
	rr := taskIntelPost(t, srv, "ti-unmapped", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if codeCalls.Load() != 1 || ctxCalls.Load() != 0 {
		t.Fatalf("unmapped task must not change routing: code=%d ctx=%d", codeCalls.Load(), ctxCalls.Load())
	}
	tev := taskClassifiedEvent(t, srv, "ti-unmapped")
	if tev.TaskIntelOutcome != "unmapped" {
		t.Fatalf("outcome=%q want unmapped", tev.TaskIntelOutcome)
	}
}

// Privacy: no prompt content may reach the trace under default settings,
// end to end.
func TestTaskIntelApply_NoPromptContentInTrace(t *testing.T) {
	var codeCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer codeUp.Close()
	ctxUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	defer ctxUp.Close()

	srv := testGateway(t, taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", ""))
	srv.SyncCapabilityContracts()

	const canary = "CANARY_PROMPT_CONTENT_9d2b"
	body := taskIntelBody(t, "the secret phrase is "+canary+" and here is code:\n```go\nfunc x() {}\n```")
	rr := taskIntelPost(t, srv, "ti-canary", body, map[string]string{"User-Agent": "claude-code/9.9"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, ev := range srv.bus.Snapshot() {
		if ev.RequestID != "ti-canary" {
			continue
		}
		evJSON, _ := json.Marshal(ev)
		if strings.Contains(string(evJSON), canary) {
			t.Fatalf("privacy violation: prompt content in %s event: %s", ev.Kind, string(evJSON))
		}
	}
	tev := taskClassifiedEvent(t, srv, "ti-canary")
	if tev.TaskClientClass != "agent" {
		t.Fatalf("client class not recorded: %q", tev.TaskClientClass)
	}
}
