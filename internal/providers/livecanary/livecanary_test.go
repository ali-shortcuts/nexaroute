package livecanary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

type fakeProvider struct {
	hits          atomic.Int64
	failStream    bool
	textErrorBody string
	block         bool
	release       chan struct{}
	outputTokens  int
	// Acceptance-scenario fixtures (issue #646). requireAuth rejects any
	// bearer token other than the test credential, streamUsage appends an
	// OpenAI usage chunk to the stream, and truncateStream ends a stream
	// after one data event without a terminal event.
	requireAuth    bool
	streamUsage    bool
	truncateStream bool
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if f.requireAuth && r.Header.Get("Authorization") != "Bearer canary-test-secret-1234" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"invalid api key"}}`))
		return
	}
	if f.block {
		if f.release != nil {
			select {
			case <-f.release:
			case <-r.Context().Done():
			}
		} else {
			<-r.Context().Done()
		}
		return
	}
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"malformed request"}}`))
		return
	}
	stream, _ := req["stream"].(bool)
	if stream {
		if f.failStream {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"stream unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n")
		if f.truncateStream {
			// No terminal event and no [DONE]: the client must report a
			// failed, truncated stream, never a silent pass or a retry.
			return
		}
		if f.streamUsage {
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	if f.textErrorBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(f.textErrorBody))
		return
	}
	tools, _ := req["tools"].([]any)
	messages, _ := req["messages"].([]any)
	hasToolRole := false
	hasImage := false
	for _, raw := range messages {
		m, _ := raw.(map[string]any)
		if role, _ := m["role"].(string); role == "tool" {
			hasToolRole = true
		}
		if content, ok := m["content"].([]any); ok {
			for _, c := range content {
				if p, _ := c.(map[string]any); p != nil && p["type"] == "image_url" {
					hasImage = true
				}
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": "OK"}
	finish := "stop"
	switch {
	case hasToolRole:
		msg["content"] = "42"
	case hasImage:
		msg["content"] = "red"
	case len(tools) >= 2:
		msg["content"] = ""
		msg["tool_calls"] = []any{
			toolCall("call_a", "lookup_a"),
			toolCall("call_b", "lookup_b"),
		}
		finish = "tool_calls"
	case req["tool_choice"] != nil:
		msg["content"] = ""
		msg["tool_calls"] = []any{toolCall("call_1", "lookup")}
		finish = "tool_calls"
	case req["response_format"] != nil:
		msg["content"] = `{"status":"ok"}`
	case req["reasoning_effort"] != nil:
		msg["content"] = "4"
	}
	resp := map[string]any{
		"id":      "chatcmpl-canary",
		"object":  "chat.completion",
		"model":   "canary-model",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9},
	}
	if req["reasoning_effort"] != nil {
		resp["usage"] = map[string]any{
			"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
		}
	}
	if hasImage {
		resp["usage"] = map[string]any{
			"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13,
			"prompt_tokens_details": map[string]any{"cached_tokens": 4},
		}
	}
	if f.outputTokens > 0 {
		resp["usage"] = map[string]any{"prompt_tokens": 7, "completion_tokens": f.outputTokens, "total_tokens": 7 + f.outputTokens}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func toolCall(id, name string) map[string]any {
	return map[string]any{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": `{"q":"canary"}`},
	}
}

func testProvider(baseURL, keyEnv string) config.ProviderConfig {
	return config.ProviderConfig{
		ID: "canary", Name: "Canary", Type: "openai_compatible",
		BaseURL: baseURL, AuthMode: "bearer", APIKeyEnv: keyEnv,
		Enabled: true, MaxConcurrency: 2,
		Models: []config.ModelConfig{{ID: "m", Model: "canary-model", Enabled: true, Weight: 1}},
	}
}

func testOptions(baseURL, keyEnv string) Options {
	return Options{
		Provider:        testProvider(baseURL, keyEnv),
		Model:           config.ModelConfig{ID: "m", Model: "canary-model", InputCostPerMTok: 1, OutputCostPerMTok: 2},
		Scenarios:       []string{"core"},
		MaxCalls:        5,
		MaxOutputTokens: 64,
		MaxInputTokens:  128,
		CallTimeout:     2 * time.Second,
		OverallTimeout:  10 * time.Second,
		MaxCostUSD:      1,
	}
}

func TestCanaryRunsFixtureScenariosAndRecordsEvidence(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	runner, err := New(testOptions(srv.URL, "NEXA_CANARY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomePass {
		t.Fatalf("outcome=%s report=%s", report.Outcome, report.Summary())
	}
	if report.Limits.CallsUsed != 5 {
		t.Fatalf("calls=%d, want 5", report.Limits.CallsUsed)
	}
	if report.Provider.Dialect != "openai_compatible" || report.Provider.Model != "canary-model" {
		t.Fatalf("provider evidence = %+v", report.Provider)
	}
	for _, want := range []string{"non_stream", "streaming_sse", "tool_calls", "tool_results", "error_envelopes"} {
		if !contains(report.VerifiedDimensions, want) {
			t.Fatalf("verified dimensions %v missing %s", report.VerifiedDimensions, want)
		}
	}
	if !contains(report.NotCoveredDimensions, "retry_after") || !contains(report.NotCoveredDimensions, "idempotency") {
		t.Fatalf("report pretends to cover unbounded dimensions: %v", report.NotCoveredDimensions)
	}
	if report.Limits.EstimatedCostUSD == nil || !report.Limits.CostEstimateKnown {
		t.Fatal("configured pricing should produce a known cost estimate")
	}
	if report.GeneratedAt == "" {
		t.Fatal("report has no date")
	}
}

func TestCanaryMissingCredentialMakesNoCall(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_MISSING", "")

	_, err := New(testOptions(srv.URL, "NEXA_CANARY_MISSING"))
	if !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("err=%v, want ErrMissingCredential", err)
	}
	if fake.hits.Load() != 0 {
		t.Fatalf("missing credential still made %d upstream calls", fake.hits.Load())
	}
	if strings.Contains(err.Error(), "NEXA_CANARY_MISSING") == false {
		t.Fatalf("error should name the env var so the operator can fix it: %v", err)
	}
}

func TestCanaryPartialFailureIsReported(t *testing.T) {
	fake := &fakeProvider{failStream: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	runner, err := New(testOptions(srv.URL, "NEXA_CANARY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomePartial {
		t.Fatalf("outcome=%s, want PARTIAL\n%s", report.Outcome, report.Summary())
	}
	if !contains(report.FailedDimensions, "streaming_sse") {
		t.Fatalf("failed dimensions = %v, want streaming_sse", report.FailedDimensions)
	}
	if contains(report.VerifiedDimensions, "streaming_sse") {
		t.Fatal("failed stream scenario must not verify streaming_sse")
	}
	if contains(report.NotCoveredDimensions, "streaming_sse") {
		t.Fatal("a failed dimension was tested; it must not be listed as not covered")
	}
	status := map[string]string{}
	for _, res := range report.Scenarios {
		status[res.ID] = res.Status
	}
	if status["text"] != StatusPass || status["stream"] != StatusFail || status["tools"] != StatusPass {
		t.Fatalf("scenario statuses = %v", status)
	}
}

func TestCanaryRedactsSecretsFromErrorsAndSummary(t *testing.T) {
	const secret = "canary-test-DO-NOT-LEAK-9f3b"
	fake := &fakeProvider{textErrorBody: `{"error":{"message":"provider rejected ` + secret + `","authorization":"Bearer ` + secret + `"}}`}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", secret)

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(raw), report.Summary()} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret leaked in canary output:\n%s", out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Fatalf("expected redaction marker in output:\n%s", out)
		}
	}
}

func TestSecretRedactorCoversAuthorizationSchemeTokens(t *testing.T) {
	cases := []string{
		"Authorization: Bearer opaque-token-1234",
		"authorization=Basic dXNlcjpwYXNz",
		"Proxy-Authorization: bearer another-token",
	}
	for _, input := range cases {
		got := NewRedactor(nil).Redact(input)
		for _, leaked := range []string{"opaque-token-1234", "dXNlcjpwYXNz", "another-token"} {
			if strings.Contains(got, leaked) {
				t.Fatalf("redactor leaked %q from %q: %q", leaked, input, got)
			}
		}
	}
}

func TestSecretRedactorCoversHeaderAndProxyCredentials(t *testing.T) {
	const headerSecret = "header-token-DO-NOT-LEAK"
	const proxySecret = "proxy-password-DO-NOT-LEAK"
	provider := config.ProviderConfig{
		Headers:  map[string]string{"X-Custom-Key": headerSecret, "X-Public": "not-secret"},
		ProxyURL: "http://proxy-user:" + proxySecret + "@127.0.0.1:8080",
	}
	redacted := NewRedactor(secretsForProvider(provider)).Redact(
		"header=" + headerSecret + " proxy=" + proxySecret + " public=not-secret")
	if strings.Contains(redacted, headerSecret) || strings.Contains(redacted, proxySecret) {
		t.Fatalf("secret survived redaction: %q", redacted)
	}
	if !strings.Contains(redacted, "not-secret") {
		t.Fatalf("benign header value was over-redacted: %q", redacted)
	}
}

func TestCanaryCancellationStopsAndRecords(t *testing.T) {
	fake := &fakeProvider{block: true, release: make(chan struct{})}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	report := runner.Run(ctx)
	close(fake.release)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %s; the canary did not honor the context", elapsed)
	}
	if report.Outcome != OutcomeCanceled {
		t.Fatalf("outcome=%s, want CANCELED\n%s", report.Outcome, report.Summary())
	}
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusCanceled {
		t.Fatalf("scenario result = %+v", report.Scenarios)
	}
}

func TestCanaryObservedCostStopsRunAndReportsPartial(t *testing.T) {
	fake := &fakeProvider{outputTokens: 5}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Model.InputCostPerMTok = 0
	opts.Model.OutputCostPerMTok = 1_000_000
	opts.MaxCostUSD = 2
	opts.MaxOutputTokens = 1
	opts.Scenarios = []string{"text,stream"}
	opts.MaxCalls = 2
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomePartial {
		t.Fatalf("outcome=%s, want PARTIAL\n%s", report.Outcome, report.Summary())
	}
	if len(report.Scenarios) != 2 || report.Scenarios[0].Status != StatusPass || report.Scenarios[1].Status != StatusSkip {
		t.Fatalf("scenarios=%+v", report.Scenarios)
	}
	if !contains(report.VerifiedDimensions, "non_stream") {
		t.Fatalf("successful text evidence was discarded: %v", report.VerifiedDimensions)
	}
}

func TestCanaryPreCanceledContextMakesNoCall(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	runner, err := New(testOptions(srv.URL, "NEXA_CANARY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := runner.Run(ctx)
	if fake.hits.Load() != 0 {
		t.Fatalf("pre-canceled canary made %d calls", fake.hits.Load())
	}
	if report.Outcome != OutcomeCanceled {
		t.Fatalf("outcome=%s, want CANCELED", report.Outcome)
	}
}

func TestCanaryBudgetAndCostGuards(t *testing.T) {
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	opts := testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.MaxCalls = 2
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "selected scenarios need") {
		t.Fatalf("call budget guard error = %v", err)
	}

	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.MaxCostUSD = 0.0000001
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "exceeds --max-cost-usd") {
		t.Fatalf("cost guard error = %v", err)
	}

	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.MaxCalls = 1
	opts.Scenarios = []string{"reasoning"}
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "reasoning scenario requires") {
		t.Fatalf("reasoning token guard error = %v", err)
	}
}

func TestCanarySkipsManifestUnsupportedScenario(t *testing.T) {
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	opts := testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.Type = "gemini"
	opts.Provider.BaseURL = "http://127.0.0.1:1"
	opts.Provider.AuthMode = "none"
	opts.Provider.APIKeyEnv = ""
	opts.Scenarios = []string{"parallel_tools"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomeNoEvidence {
		t.Fatalf("outcome=%s, want NO-EVIDENCE\n%s", report.Outcome, report.Summary())
	}
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusSkip {
		t.Fatalf("scenario = %+v", report.Scenarios)
	}
	if !strings.Contains(report.Scenarios[0].Note, "UNSUPPORTED") {
		t.Fatalf("skip note = %q", report.Scenarios[0].Note)
	}
}

func TestCanarySkipsScenarioNotApplicableToDialect(t *testing.T) {
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	opts := testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.Type = "anthropic_compatible"
	opts.Provider.BaseURL = "http://127.0.0.1:1"
	opts.Provider.AuthMode = "none"
	opts.Provider.APIKeyEnv = ""
	opts.Scenarios = []string{"structured_output"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomeNoEvidence {
		t.Fatalf("outcome=%s, want NO-EVIDENCE\n%s", report.Outcome, report.Summary())
	}
	if report.Scenarios[0].Status != StatusSkip || !strings.Contains(report.Scenarios[0].Note, "not applicable") {
		t.Fatalf("scenario=%+v", report.Scenarios[0])
	}
}

func TestEncodeRequestPathsAndPayloads(t *testing.T) {
	cases := []struct {
		name     string
		provider config.ProviderConfig
		wantPath string
	}{
		{"anthropic", config.ProviderConfig{Type: "anthropic_compatible"}, "/v1/messages"},
		{"openai", config.ProviderConfig{Type: "openai_compatible"}, "/v1/chat/completions"},
		{"responses", config.ProviderConfig{Type: "openai_responses"}, "/v1/responses"},
		{"azure", config.ProviderConfig{Type: "azure_openai", APIVersion: "2024-10-21"}, "/openai/deployments/canary-model/chat/completions?api-version=2024-10-21"},
		{"gemini", config.ProviderConfig{Type: "gemini"}, "/v1beta/models/canary-model:generateContent"},
		{"gemini-stream", config.ProviderConfig{Type: "gemini"}, "/v1beta/models/canary-model:streamGenerateContent?alt=sse"},
		{"vertex", config.ProviderConfig{Type: "vertex", VertexProject: "p", VertexLocation: "us-central1"}, "/v1/projects/p/locations/us-central1/publishers/google/models/canary-model:generateContent"},
		{"bedrock", config.ProviderConfig{Type: "bedrock"}, "/model/canary-model/converse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.provider.ApplyDefaults()
			req := baseRequest("canary-model", "hi", 8)
			req.Stream = strings.Contains(tc.name, "stream")
			payload, path, err := encodeRequest(tc.provider, req)
			if err != nil {
				t.Fatal(err)
			}
			if path != tc.wantPath {
				t.Fatalf("path=%q, want %q", path, tc.wantPath)
			}
			if !json.Valid(payload) {
				t.Fatalf("payload is not valid JSON: %s", payload)
			}
		})
	}
}

func TestResolveScenariosRejectsUnknown(t *testing.T) {
	if _, err := ResolveScenarios([]string{"not-a-scenario"}); err == nil || !strings.Contains(err.Error(), "unknown scenario") {
		t.Fatalf("err=%v", err)
	}
	sc, err := ResolveScenarios([]string{"core,text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc) != 5 {
		t.Fatalf("core+duplicate text resolved to %d scenarios", len(sc))
	}
}

func TestManifestStillHasNoLiveClaims(t *testing.T) {
	for _, entry := range providers.Manifest() {
		if entry.Status == providers.StateLiveVerified {
			t.Fatalf("dialect %s claims LIVE-VERIFIED without reviewed live evidence", entry.Dialect)
		}
		for dim, claim := range entry.Dimensions {
			if claim.State == providers.StateLiveVerified {
				t.Fatalf("dialect %s dimension %s claims LIVE-VERIFIED without reviewed live evidence", entry.Dialect, dim)
			}
		}
	}
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}
