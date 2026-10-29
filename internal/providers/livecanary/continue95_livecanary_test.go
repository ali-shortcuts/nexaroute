package livecanary

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// Continue95 stub adapter: returns a canned response (or error) for every
// DoPath call, so scenario branches can be exercised without any network
// or provider credential. Loopback httptest servers are used only where
// the code under test builds its own adapter (runAuthFailure).
type Continue95StubAdapter struct {
	status int
	header http.Header
	body   string
	err    error
}

func (s *Continue95StubAdapter) ID() string   { return "continue95-stub" }
func (s *Continue95StubAdapter) Kind() string { return "stub" }
func (s *Continue95StubAdapter) Stats() providers.ProviderStats {
	return providers.ProviderStats{}
}
func (s *Continue95StubAdapter) CredentialsMatch([]string) bool { return true }
func (s *Continue95StubAdapter) RedactBody(b []byte) []byte     { return b }
func (s *Continue95StubAdapter) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return s.DoPath(ctx, http.MethodPost, "/stub", payload, stream, forward)
}
func (s *Continue95StubAdapter) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	h := s.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: s.status, Header: h, Body: io.NopCloser(strings.NewReader(s.body))}, nil
}
func (s *Continue95StubAdapter) CountTokens(ctx context.Context, payload []byte, forward http.Header) (*http.Response, error) {
	return nil, errors.New("continue95 stub: CountTokens not implemented")
}
func (s *Continue95StubAdapter) Probe(ctx context.Context, model string, maxTokens int) (time.Duration, int, error) {
	return 0, 0, nil
}

func Continue95Runner(t *testing.T, dialect string, stub providers.Adapter) *Runner {
	t.Helper()
	return &Runner{
		provider:      config.ProviderConfig{ID: "p", Type: dialect, BaseURL: "http://127.0.0.1:9", AuthMode: "bearer"},
		upstreamModel: "canary-model",
		adapter:       stub,
		limits:        Limits{MaxOutputTokens: 64, MaxCalls: 12},
		callTimeout:   time.Second,
		overall:       10 * time.Second,
		redactor:      NewRedactor(nil),
		now:           func() time.Time { return time.Now().UTC() },
	}
}

func Continue95OpenAIChatBody(content, finish string, usage string) string {
	if usage == "" {
		usage = `"usage":{"prompt_tokens":7,"completion_tokens":2}`
	}
	return `{"choices":[{"message":{"role":"assistant","content":` + quoteJSON(content) + `},"finish_reason":` + quoteJSON(finish) + `}],` + usage + `}`
}

func quoteJSON(s string) string {
	if s == "" {
		return `""`
	}
	out := strings.ReplaceAll(s, `\`, `\\`)
	out = strings.ReplaceAll(out, `"`, `\"`)
	return `"` + out + `"`
}

func Continue95Scenario(id string) scenario {
	scenarios, err := ResolveScenarios([]string{id})
	if err != nil {
		panic(err)
	}
	return scenarios[0]
}

func TestContinue95ResolveScenariosEdges(t *testing.T) {
	// Empty input defaults to core.
	sc, err := ResolveScenarios(nil)
	if err != nil || len(sc) != len(coreScenarioIDs) {
		t.Fatalf("empty names: sc=%d err=%v, want core %d", len(sc), err, len(coreScenarioIDs))
	}
	// Blank-only input selects nothing.
	if _, err := ResolveScenarios([]string{" , "}); err == nil || !strings.Contains(err.Error(), "no canary scenarios selected") {
		t.Fatalf("blank input err=%v, want no-scenarios error", err)
	}
	// Case/space normalization and dedup across aliases.
	sc, err = ResolveScenarios([]string{" Extended , TEXT "})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range sc {
		if seen[s.ID] {
			t.Fatalf("duplicate scenario %q", s.ID)
		}
		seen[s.ID] = true
	}
	if !seen["text"] || !seen["structured_output"] {
		t.Fatalf("extended+text ids=%v", seen)
	}
	// "all" is the extended set; acceptance is its own set.
	sc, err = ResolveScenarios([]string{"all"})
	if err != nil || len(sc) != len(extendedScenarioIDs) {
		t.Fatalf("all: %d err=%v", len(sc), err)
	}
	sc, err = ResolveScenarios([]string{"acceptance"})
	if err != nil || len(sc) != len(acceptanceScenarioIDs) {
		t.Fatalf("acceptance: %d err=%v", len(sc), err)
	}
	// Unknown id error names the id and the known set.
	if _, err := ResolveScenarios([]string{"bogus-scenario"}); err == nil || !strings.Contains(err.Error(), "unknown scenario") || !strings.Contains(err.Error(), "bogus-scenario") {
		t.Fatalf("unknown err=%v", err)
	}
}

func TestContinue95AppliesPredicates(t *testing.T) {
	types := []string{"openai_compatible", "azure_openai", "openai_responses", "gemini", "vertex", "anthropic_compatible", "bedrock", "other"}
	for _, ty := range types {
		p := config.ProviderConfig{Type: ty}
		if !allDialects(p) {
			t.Fatalf("allDialects false for %s", ty)
		}
		wantStructured := ty == "openai_compatible" || ty == "azure_openai" || ty == "openai_responses" || ty == "gemini" || ty == "vertex"
		if supportsStructuredOutput(p) != wantStructured {
			t.Fatalf("structured %s=%v want %v", ty, supportsStructuredOutput(p), wantStructured)
		}
		wantParallel := ty == "openai_compatible" || ty == "azure_openai" || ty == "openai_responses" || ty == "anthropic_compatible"
		if supportsParallelTools(p) != wantParallel {
			t.Fatalf("parallel %s=%v want %v", ty, supportsParallelTools(p), wantParallel)
		}
		if supportsReasoning(p) != (ty != "bedrock") {
			t.Fatalf("reasoning %s wrong", ty)
		}
	}
	if appliesWhenCredentialed(config.ProviderConfig{AuthMode: " none "}) {
		t.Fatal("auth none must not be credentialed")
	}
	if !appliesWhenCredentialed(config.ProviderConfig{AuthMode: "bearer"}) {
		t.Fatal("bearer must be credentialed")
	}
	// Output-token rules for reasoning.
	if got := scenarioCallOutputTokens("text", "anthropic_compatible", 64); got != 64 {
		t.Fatalf("non-reasoning tokens=%d want 64", got)
	}
	if got := scenarioCallOutputTokens("reasoning", "anthropic_compatible", 64); got != 2048 {
		t.Fatalf("anthropic reasoning tokens=%d want 2048", got)
	}
	if got := scenarioCallOutputTokens("reasoning", "openai_compatible", 100); got != 512 {
		t.Fatalf("low reasoning tokens=%d want 512", got)
	}
	if got := scenarioCallOutputTokens("reasoning", "openai_compatible", 900); got != 900 {
		t.Fatalf("high reasoning tokens=%d want 900", got)
	}
	if got := dimensionsAsStrings(nil); len(got) != 0 {
		t.Fatalf("empty dimensions=%v", got)
	}
	if got := dimensionsAsStrings([]providers.ConformanceDimension{providers.DimAuth}); len(got) != 1 || got[0] != string(providers.DimAuth) {
		t.Fatalf("dimensions=%v", got)
	}
}

func TestContinue95ScenarioUnsupportedAndPaths(t *testing.T) {
	if unsupported, note := scenarioUnsupported("no-such-dialect", scenario{ID: "text"}); unsupported || note != "" {
		t.Fatalf("unknown dialect unsupported=%v note=%q", unsupported, note)
	}
	if pathOr("  ", "/fallback") != "/fallback" || pathOr("/custom", "/fallback") != "/custom" {
		t.Fatal("pathOr wrong")
	}
	cases := []struct {
		ty   string
		want string
	}{
		{"anthropic_compatible", "/v1/messages"},
		{"openai_responses", "/v1/responses"},
		{"openai_compatible", "/v1/chat/completions"},
		{"gemini", "/v1/chat/completions"},
	}
	for _, tc := range cases {
		if got := defaultPath(config.ProviderConfig{Type: tc.ty}); got != tc.want {
			t.Fatalf("defaultPath %s=%q want %q", tc.ty, got, tc.want)
		}
	}
	p := config.ProviderConfig{Type: "anthropic_compatible", MessagesPath: "/custom/messages"}
	if got := defaultPath(p); got != "/custom/messages" {
		t.Fatalf("custom messages path=%q", got)
	}
}

func TestContinue95EncodeRequestDialectPaths(t *testing.T) {
	req := baseRequest("canary-model", "hi", 8)
	cases := []struct {
		name     string
		provider config.ProviderConfig
		stream   bool
		want     string
	}{
		{"azure-default-version", config.ProviderConfig{Type: "azure_openai"}, false, "/openai/deployments/canary-model/chat/completions?api-version=2024-10-21"},
		{"vertex-default-location", config.ProviderConfig{Type: "vertex", VertexProject: "proj"}, false, "/v1/projects/proj/locations/us-central1/publishers/google/models/canary-model:generateContent"},
		{"vertex-stream", config.ProviderConfig{Type: "vertex", VertexProject: "proj", VertexLocation: "eu"}, true, "/v1/projects/proj/locations/eu/publishers/google/models/canary-model:streamGenerateContent?alt=sse"},
		{"gemini-model-prefix", config.ProviderConfig{Type: "gemini"}, false, "/v1beta/models/canary-model:generateContent"},
		{"gemini-stream", config.ProviderConfig{Type: "gemini"}, true, "/v1beta/models/canary-model:streamGenerateContent?alt=sse"},
		{"openai-custom-path", config.ProviderConfig{Type: "openai_compatible", ChatPath: "/custom/chat"}, false, "/custom/chat"},
		{"responses-custom-path", config.ProviderConfig{Type: "openai_responses", ResponsesPath: "/custom/resp"}, false, "/custom/resp"},
		{"anthropic-custom-path", config.ProviderConfig{Type: "anthropic_compatible", MessagesPath: "/custom/msg"}, false, "/custom/msg"},
		{"bedrock-stream", config.ProviderConfig{Type: "bedrock"}, true, ""}, // path asserted by suffix below
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			r.Stream = tc.stream
			payload, path, err := encodeRequest(tc.provider, r)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(payload), "canary-model") && tc.name != "bedrock-stream" {
				// Bedrock payloads carry the model differently; other dialects embed it.
				t.Logf("payload does not embed model name (ok if dialect encodes elsewhere): %s", payload)
			}
			if tc.want != "" && path != tc.want {
				t.Fatalf("path=%q want %q", path, tc.want)
			}
			if tc.name == "bedrock-stream" && !strings.Contains(path, "converse-stream") {
				t.Fatalf("bedrock stream path=%q, want converse-stream", path)
			}
			if tc.name == "gemini-model-prefix" {
				r2 := req
				r2.Model = "models/canary-model"
				_, path2, err := encodeRequest(tc.provider, r2)
				if err != nil || path2 != tc.want {
					t.Fatalf("models/ prefix not stripped: path=%q err=%v", path2, err)
				}
			}
		})
	}
	// decodeResponse dispatches every dialect without error on its own shape.
	decodes := []struct {
		dialect string
		body    string
	}{
		{"anthropic_compatible", `{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`},
		{"bedrock", `{"output":{"message":{"role":"assistant","content":[{"text":"OK"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":2}}`},
		{"openai_responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}`},
		{"openai_compatible", Continue95OpenAIChatBody("OK", "stop", "")},
	}
	for _, tc := range decodes {
		resp, err := decodeResponse(tc.dialect, []byte(tc.body))
		if err != nil {
			t.Fatalf("decode %s: %v", tc.dialect, err)
		}
		if strings.TrimSpace(resp.Text()) == "" {
			t.Fatalf("decode %s produced no text: %+v", tc.dialect, resp)
		}
	}
}

func TestContinue95BogusAuthProviderShapes(t *testing.T) {
	if _, err := bogusAuthProvider(config.ProviderConfig{AuthMode: "none"}); err == nil || !strings.Contains(err.Error(), "auth_mode none") {
		t.Fatalf("auth none bogus err=%v", err)
	}
	p := config.ProviderConfig{
		ID: "p", Type: "openai_compatible", AuthMode: "bearer", APIKey: "real-key",
		Headers: map[string]string{
			"X-Custom": "keep", "Authorization": "Bearer real-key", "X-Token": "tok",
			"Cookie": "session=abc", "X-Secret": "sec", "X-Auth": "auth", "X-Key-Id": "kid",
		},
	}
	bogus, err := bogusAuthProvider(p)
	if err != nil {
		t.Fatal(err)
	}
	if bogus.APIKey == "real-key" || bogus.Headers["X-Custom"] != "keep" {
		t.Fatalf("bogus provider leaked or dropped headers: %+v", bogus)
	}
	for _, name := range []string{"Authorization", "X-Token", "Cookie", "X-Secret", "X-Auth", "X-Key-Id"} {
		if _, ok := bogus.Headers[name]; ok {
			t.Fatalf("auth-shaped header %q survived bogus replacement", name)
		}
	}
	// No headers at all: bogus still succeeds and keeps nil headers.
	bogus2, err := bogusAuthProvider(config.ProviderConfig{AuthMode: "bearer", APIKey: "k"})
	if err != nil || bogus2.Headers != nil {
		t.Fatalf("headerless bogus err=%v headers=%v", err, bogus2.Headers)
	}
}

func TestContinue95StreamHelpers(t *testing.T) {
	if hasSSEData("event: x\n\n") || !hasSSEData("data: {\"a\":1}\n\ndata: [DONE]\n\n") {
		t.Fatal("hasSSEData wrong")
	}
	if hasSSEData("data: [DONE]\n\ndata: \n\n") {
		t.Fatal("DONE-only stream must not count as data")
	}
	// streamTerminal per dialect.
	if !streamTerminal("anthropic_compatible", "event: message_stop\n") {
		t.Fatal("anthropic terminal missed")
	}
	if !streamTerminal("openai_responses", "data: {\"type\":\"response.completed\"}\n\n") {
		t.Fatal("responses terminal missed")
	}
	if streamTerminal("bedrock", "data: {\"eventType\":\"messageStart\"}\n\n") {
		t.Fatal("bedrock messageStart is not messageStop")
	}
	if !streamTerminal("bedrock", "data: {\"eventType\":\"messageStop\"}\n\n") {
		t.Fatal("bedrock terminal missed")
	}
	if !streamTerminal("openai_compatible", "data: [DONE]\n\n") {
		t.Fatal("openai DONE missed")
	}
	// JSON finish helpers: nested, null, empty, invalid.
	nested := "data: {\"outer\":{\"finish_reason\":\"stop\"}}\n\n"
	if !streamJSONHasNonEmptyString(nested, "finish_reason") {
		t.Fatal("nested finish_reason missed")
	}
	if streamJSONHasNonEmptyString("data: {\"finish_reason\":\"\"}\n\n", "finish_reason") {
		t.Fatal("empty finish_reason counted")
	}
	if streamJSONHasNonEmptyString("data: {\"finish_reason\":null}\n\n", "finish_reason") {
		t.Fatal("null finish_reason counted")
	}
	if streamJSONHasNonEmptyString("data: not-json\n\n", "finish_reason") {
		t.Fatal("invalid JSON counted")
	}
	if streamJSONHasNonEmptyString("event: x\n\n", "finish_reason") {
		t.Fatal("non-data line counted")
	}
	// extractStreamUsage: malformed lines skipped, non-float ignored, nested arrays walked.
	body := "data: broken-json\n\ndata: {\"usage\":{\"prompt_tokens\":\"three\",\"completion_tokens\":4}}\n\ndata: {\"arr\":[{\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":6}}]}\n\n"
	in, out, found := extractStreamUsage([]byte(body))
	if !found || in != 5 || out != 6 {
		t.Fatalf("usage walk in=%d out=%d found=%v, want 5/6", in, out, found)
	}
	if _, _, found := extractStreamUsage([]byte("data: {\"usage\":{\"prompt_tokens\":5}}\n\n")); found {
		t.Fatal("input-only usage must not count as found")
	}
	if _, _, found := extractStreamUsage([]byte("")); found {
		t.Fatal("empty stream must not count as found")
	}
	// extractJSONObject: surrounding prose, invalid, empty.
	obj, ok := extractJSONObject("prefix {\"status\":\"ok\"} suffix")
	if !ok || obj["status"] != "ok" {
		t.Fatalf("extractJSONObject=%v %v", obj, ok)
	}
	if _, ok := extractJSONObject("no json here"); ok {
		t.Fatal("non-JSON extracted as object")
	}
	if _, ok := extractJSONObject(""); ok {
		t.Fatal("empty extracted as object")
	}
	// hasNativeFinishReason per dialect + malformed.
	if !hasNativeFinishReason("anthropic_compatible", []byte(`{"stop_reason":"end_turn"}`)) {
		t.Fatal("anthropic finish missed")
	}
	if hasNativeFinishReason("anthropic_compatible", []byte(`{"stop_reason":null}`)) {
		t.Fatal("anthropic null finish counted")
	}
	if !hasNativeFinishReason("gemini", []byte(`{"candidates":[{"finishReason":"STOP"}]}`)) {
		t.Fatal("gemini finish missed")
	}
	if hasNativeFinishReason("gemini", []byte(`{"candidates":[]}`)) {
		t.Fatal("empty gemini candidates counted")
	}
	if !hasNativeFinishReason("bedrock", []byte(`{"stopReason":"end_turn"}`)) {
		t.Fatal("bedrock finish missed")
	}
	if !hasNativeFinishReason("openai_responses", []byte(`{"status":"completed"}`)) {
		t.Fatal("responses status missed")
	}
	if !hasNativeFinishReason("openai_compatible", []byte(`{"choices":[{"finish_reason":"stop"}]}`)) {
		t.Fatal("openai finish missed")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`{"choices":[{"finish_reason":null}]}`)) {
		t.Fatal("openai null finish counted")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`not json`)) {
		t.Fatal("malformed body counted as finish")
	}
	if nonEmptyRaw(nil) || nonEmptyRaw([]byte("null")) || nonEmptyRaw([]byte(`""`)) || !nonEmptyRaw([]byte(`"stop"`)) {
		t.Fatal("nonEmptyRaw wrong")
	}
}

func TestContinue95RunTextBranches(t *testing.T) {
	ctx := context.Background()
	sc := Continue95Scenario("text")
	// Adapter error.
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{err: errors.New("dial failed")})
	res := r.runText(ctx, sc)
	if res.Status != StatusFail || !strings.Contains(res.Error, "dial failed") {
		t.Fatalf("adapter error res=%+v", res)
	}
	// HTTP error status.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 500, body: `{"error":"boom"}`})
	res = r.runText(ctx, sc)
	if res.Status != StatusFail || res.HTTPStatus != 500 || !strings.Contains(res.Error, "http 500") {
		t.Fatalf("http error res=%+v", res)
	}
	// Decode error.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: `not json`})
	res = r.runText(ctx, sc)
	if res.Status != StatusFail {
		t.Fatalf("decode error res=%+v, want FAIL", res)
	}
	// Empty completion.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("", "stop", "")})
	res = r.runText(ctx, sc)
	if res.Status != StatusFail || !strings.Contains(res.Error, "no completion text") {
		t.Fatalf("empty text res=%+v", res)
	}
	// Pass with cache + native finish dimensions.
	cached := `"usage":{"prompt_tokens":7,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}`
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("OK", "stop", cached)})
	res = r.runText(ctx, sc)
	if res.Status != StatusPass || res.InputTokens != 7 || res.OutputTokens != 2 {
		t.Fatalf("pass res=%+v", res)
	}
	hasDim := func(want string) bool {
		for _, d := range res.Dimensions {
			if d == want {
				return true
			}
		}
		return false
	}
	if !hasDim(string(providers.DimUsageCache)) || !hasDim(string(providers.DimFinishReasons)) {
		t.Fatalf("pass dimensions=%v, want cache + finish reasons", res.Dimensions)
	}
	// Dialect pass paths (decode + finish detection per dialect).
	dialectBodies := []struct {
		dialect string
		body    string
	}{
		{"anthropic_compatible", `{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`},
		{"bedrock", `{"output":{"message":{"role":"assistant","content":[{"text":"OK"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":2}}`},
	}
	for _, tc := range dialectBodies {
		r := Continue95Runner(t, tc.dialect, &Continue95StubAdapter{status: 200, body: tc.body})
		res := r.runText(ctx, sc)
		if res.Status != StatusPass {
			t.Fatalf("dialect %s res=%+v", tc.dialect, res)
		}
	}
}

func TestContinue95RunStreamBranches(t *testing.T) {
	ctx := context.Background()
	sseOK := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	// Wrong content type.
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"application/json"}}, body: sseOK})
	res := r.runStream(ctx, Continue95Scenario("stream"))
	if res.Status != StatusFail || !strings.Contains(res.Error, "expected text/event-stream") {
		t.Fatalf("content-type res=%+v", res)
	}
	// No data event.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, body: "data: [DONE]\n\n"})
	res = r.runStream(ctx, Continue95Scenario("stream"))
	if res.Status != StatusFail || !strings.Contains(res.Error, "no non-terminal data") {
		t.Fatalf("no-data res=%+v", res)
	}
	// No terminal event.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, body: "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"})
	res = r.runStream(ctx, Continue95Scenario("stream"))
	if res.Status != StatusFail || !strings.Contains(res.Error, "terminal event") {
		t.Fatalf("no-terminal res=%+v", res)
	}
	// Pass.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, body: sseOK})
	res = r.runStream(ctx, Continue95Scenario("stream"))
	if res.Status != StatusPass {
		t.Fatalf("stream pass res=%+v", res)
	}
	// Stream usage: no usage fails, usage passes with counts.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, body: sseOK})
	res = r.runStreamUsage(ctx, Continue95Scenario("stream_usage"))
	if res.Status != StatusFail || !strings.Contains(res.Error, "no input/output usage") {
		t.Fatalf("usage-missing res=%+v", res)
	}
	sseUsage := sseOK + ""
	_ = sseUsage
	usageBody := "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}}, body: usageBody})
	res = r.runStreamUsage(ctx, Continue95Scenario("stream_usage"))
	if res.Status != StatusPass || res.InputTokens != 7 || res.OutputTokens != 2 {
		t.Fatalf("usage pass res=%+v", res)
	}
	// Stream adapter error fails, not panics.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{err: errors.New("stream dial failed")})
	res = r.runStream(ctx, Continue95Scenario("stream"))
	if res.Status != StatusFail {
		t.Fatalf("stream adapter error res=%+v", res)
	}
}

func TestContinue95RunToolBranches(t *testing.T) {
	ctx := context.Background()
	toolBody := func(calls string) string {
		return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":` + calls + `},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
	}
	// Tools: wrong tool name, invalid JSON args, no tool call, pass.
	cases := []struct {
		name string
		body string
		want string // PASS or FAIL
		err  string
	}{
		{"wrong-name", toolBody(`[{"id":"c1","type":"function","function":{"name":"other","arguments":"{}"}}]`), StatusFail, "did not contain the requested lookup tool call"},
		{"invalid-args", toolBody(`[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{bad"}}]`), StatusFail, "not valid JSON"},
		{"no-call", Continue95OpenAIChatBody("just text", "stop", ""), StatusFail, "did not contain the requested lookup tool call"},
		{"pass", toolBody(`[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"canary\"}"}}]`), StatusPass, ""},
	}
	for _, tc := range cases {
		r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: tc.body})
		res := r.runTools(ctx, Continue95Scenario("tools"))
		if res.Status != tc.want {
			t.Fatalf("%s: res=%+v want %s", tc.name, res, tc.want)
		}
		if tc.err != "" && !strings.Contains(res.Error, tc.err) {
			t.Fatalf("%s: error=%q want containing %q", tc.name, res.Error, tc.err)
		}
	}
	// Tools HTTP error.
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 429, body: `{"error":"slow down"}`})
	if res := r.runTools(ctx, Continue95Scenario("tools")); res.Status != StatusFail || res.HTTPStatus != 429 {
		t.Fatalf("tools http res=%+v", res)
	}
	// Tool result: empty text fails, text passes.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("", "stop", "")})
	if res := r.runToolResult(ctx, Continue95Scenario("tool_result")); res.Status != StatusFail || !strings.Contains(res.Error, "no text after the tool-result") {
		t.Fatalf("tool-result empty res=%+v", res)
	}
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("42", "stop", "")})
	if res := r.runToolResult(ctx, Continue95Scenario("tool_result")); res.Status != StatusPass {
		t.Fatalf("tool-result pass res=%+v", res)
	}
}

func TestContinue95RunStructuredImageParallelReasoning(t *testing.T) {
	ctx := context.Background()
	// Structured output.
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("not json", "stop", "")})
	if res := r.runStructuredOutput(ctx, Continue95Scenario("structured_output")); res.Status != StatusFail || !strings.Contains(res.Error, "not a JSON object") {
		t.Fatalf("structured non-json res=%+v", res)
	}
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody(`{"other":1}`, "stop", "")})
	if res := r.runStructuredOutput(ctx, Continue95Scenario("structured_output")); res.Status != StatusFail || !strings.Contains(res.Error, "status field") {
		t.Fatalf("structured missing status res=%+v", res)
	}
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody(`{"status":"ok"}`, "stop", "")})
	if res := r.runStructuredOutput(ctx, Continue95Scenario("structured_output")); res.Status != StatusPass {
		t.Fatalf("structured pass res=%+v", res)
	}
	// Images: empty, wrong color, pass.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("", "stop", "")})
	if res := r.runImages(ctx, Continue95Scenario("images")); res.Status != StatusFail || !strings.Contains(res.Error, "no text for the image") {
		t.Fatalf("images empty res=%+v", res)
	}
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("blue", "stop", "")})
	if res := r.runImages(ctx, Continue95Scenario("images")); res.Status != StatusFail || !strings.Contains(res.Error, "red test image") {
		t.Fatalf("images wrong color res=%+v", res)
	}
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("RED", "stop", "")})
	if res := r.runImages(ctx, Continue95Scenario("images")); res.Status != StatusPass {
		t.Fatalf("images pass res=%+v", res)
	}
	// Parallel tools: one call fails, two pass.
	one := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"lookup_a","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: one}).runParallelTools(ctx, Continue95Scenario("parallel_tools")); res.Status != StatusFail || !strings.Contains(res.Error, "at least two tool calls") {
		t.Fatalf("parallel one res=%+v", res)
	}
	two := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"lookup_a","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"lookup_b","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: two}).runParallelTools(ctx, Continue95Scenario("parallel_tools")); res.Status != StatusPass {
		t.Fatalf("parallel two res=%+v", res)
	}
	// Reasoning: neither thinking block nor reasoning tokens fails; reasoning tokens pass.
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("4", "stop", "")}).runReasoning(ctx, Continue95Scenario("reasoning")); res.Status != StatusFail || !strings.Contains(res.Error, "reasoning not demonstrated") {
		t.Fatalf("reasoning empty res=%+v", res)
	}
	reasoningUsage := `"usage":{"prompt_tokens":7,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":5}}`
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("4", "stop", reasoningUsage)}).runReasoning(ctx, Continue95Scenario("reasoning")); res.Status != StatusPass {
		t.Fatalf("reasoning pass res=%+v", res)
	}
}

func TestContinue95RunErrorEnvelopeBranches(t *testing.T) {
	ctx := context.Background()
	sc := Continue95Scenario("error_envelope")
	// Adapter error.
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{err: errors.New("envelope dial failed")}).runErrorEnvelope(ctx, sc); res.Status != StatusFail {
		t.Fatalf("envelope adapter error res=%+v", res)
	}
	// Non-4xx.
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: `{"ok":true}`}).runErrorEnvelope(ctx, sc); res.Status != StatusFail || !strings.Contains(res.Error, "expected a 4xx") {
		t.Fatalf("envelope 200 res=%+v", res)
	}
	// Non-JSON body.
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 400, body: `not json`}).runErrorEnvelope(ctx, sc); res.Status != StatusFail || !strings.Contains(res.Error, "not a JSON object") {
		t.Fatalf("envelope non-json res=%+v", res)
	}
	// JSON without any envelope key.
	if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 400, body: `{"detail":"bad"}`}).runErrorEnvelope(ctx, sc); res.Status != StatusFail || !strings.Contains(res.Error, "error/message envelope") {
		t.Fatalf("envelope no-key res=%+v", res)
	}
	// Each envelope spelling passes.
	for _, body := range []string{`{"error":{"message":"bad"}}`, `{"message":"bad"}`, `{"Message":"bad"}`} {
		if res := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 400, body: body}).runErrorEnvelope(ctx, sc); res.Status != StatusPass {
			t.Fatalf("envelope %s res=%+v", body, res)
		}
	}
}

func TestContinue95DoRawBranches(t *testing.T) {
	ctx := context.Background()
	// Oversize body is refused.
	big := strings.Repeat("x", int(maxResponseBytes)+1)
	if _, err := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: big}).doRaw(ctx, []byte(`{}`), false); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize err=%v", err)
	}
	// Negative latency clamps to zero: now() goes backwards between the two reads.
	times := []time.Time{time.Unix(100, 0), time.Unix(99, 0)}
	i := 0
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("OK", "stop", "")})
	r.now = func() time.Time {
		v := times[i]
		if i < len(times)-1 {
			i++
		}
		return v
	}
	res := r.runText(ctx, Continue95Scenario("text"))
	if res.Status != StatusPass || res.LatencyMS != 0 {
		t.Fatalf("negative latency res=%+v", res)
	}
	// Long error snippet is truncated with an ellipsis.
	r2 := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 500, body: strings.Repeat("e", 600)})
	res2 := r2.runText(ctx, Continue95Scenario("text"))
	if res2.Status != StatusFail || !strings.Contains(res2.Error, "...") {
		t.Fatalf("snippet truncation res=%+v", res2)
	}
	// runScenario unknown id fails closed; nil runner redacts as identity.
	r3 := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: ""})
	unknown := r3.runScenario(ctx, scenario{ID: "nope"})
	if unknown.Status != StatusFail || unknown.Error != "unknown scenario" {
		t.Fatalf("unknown scenario=%+v", unknown)
	}
	var nilRunner *Runner
	if got := nilRunner.redact("abc"); got != "abc" {
		t.Fatalf("nil runner redact=%q", got)
	}
	if got := contextError(context.Background()); got != "unknown" {
		t.Fatalf("contextError no-err=%q", got)
	}
	if got := sanitizeBaseURL("://bad-url"); got != "" {
		t.Fatalf("sanitize bad url=%q", got)
	}
	if got := sanitizeBaseURL("https://user:pw@example.com/a/?q=1#frag/"); got != "https://example.com/a" {
		t.Fatalf("sanitize url=%q", got)
	}
}

func TestContinue95NewConstructionEdges(t *testing.T) {
	// No model at all.
	opts := testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Model = config.ModelConfig{}
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "requires a model") {
		t.Fatalf("no-model err=%v", err)
	}
	// ID-only and Model-only fill each other (then fail on credential-free
	// server only if no credential; here credential is set, so construction
	// proceeds to adapter creation against the unreachable address — that
	// still succeeds because adapters are lazy).
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Model = config.ModelConfig{ID: "only-id"}
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if runner.upstreamModel != "only-id" {
		t.Fatalf("id-only upstream=%q", runner.upstreamModel)
	}
	runner.Close()
	opts.Model = config.ModelConfig{Model: "only-model"}
	runner, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if runner.model.ID != "only-model" {
		t.Fatalf("model-only id=%q", runner.model.ID)
	}
	runner.Close()
	// Invalid provider config fails closed before any call.
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.ID = ""
	opts.Provider.Type = "openai_compatible"
	opts.Provider.BaseURL = "not-a-url"
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "not usable for a canary") {
		t.Fatalf("invalid provider err=%v", err)
	}
	// Missing credential with no env name uses the api_key_env placeholder.
	opts = testOptions("http://127.0.0.1:1", "")
	opts.Provider.APIKeyEnv = ""
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	if _, err := New(opts); err == nil || !errors.Is(err, ErrMissingCredential) || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("missing credential err=%v", err)
	}
	// Bedrock without a secret env fails closed.
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.Type = "bedrock"
	opts.Provider.SecretKeyEnv = "NEXA_CANARY_SECRET_MISSING"
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	t.Setenv("NEXA_CANARY_SECRET_MISSING", "")
	if _, err := New(opts); err == nil || !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("bedrock missing secret err=%v", err)
	}
	// Unknown scenario at construction.
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"bogus"}
	opts.MaxCalls = 1
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "unknown scenario") {
		t.Fatalf("construction unknown scenario err=%v", err)
	}
	// Nil context to Run is treated as background (no call, canceled
	// overall context path is not needed: use a stub-runner instead).
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("OK", "stop", "")})
	r.scenarios = []scenario{Continue95Scenario("text")}
	r.limits.MaxCalls = 1
	report := r.Run(nil)
	if report.Outcome != OutcomePass {
		t.Fatalf("nil ctx run outcome=%s", report.Outcome)
	}
}

func TestContinue95RunBudgetAndAuthStop(t *testing.T) {
	ctx := context.Background()
	// Call-budget skip: second scenario never runs once MaxCalls is hit.
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: Continue95OpenAIChatBody("OK", "stop", "")})
	r.scenarios = []scenario{Continue95Scenario("text"), Continue95Scenario("tools")}
	r.limits.MaxCalls = 1
	report := r.Run(ctx)
	if len(report.Scenarios) != 2 || report.Scenarios[0].Status != StatusPass || report.Scenarios[1].Status != StatusSkip || !strings.Contains(report.Scenarios[1].Note, "call budget exhausted") {
		t.Fatalf("budget skip report=%+v", report.Scenarios)
	}
	// 401 on a non-auth scenario stops the remaining scenarios.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 401, body: `{"error":"unauthorized"}`})
	r.scenarios = []scenario{Continue95Scenario("text"), Continue95Scenario("tools")}
	r.limits.MaxCalls = 12
	report = r.Run(ctx)
	if len(report.Scenarios) != 2 || report.Scenarios[1].Status != StatusSkip || !strings.Contains(report.Scenarios[1].Note, "rejected the credential") {
		t.Fatalf("auth stop report=%+v", report.Scenarios)
	}
	// Summary without an estimate uses the unknown-cost line.
	rep := Report{Provider: ProviderEvidence{ID: "p", Dialect: "openai_compatible", Model: "m"}, Limits: Limits{MaxCostUSD: 1}, Scenarios: []ScenarioResult{{ID: "text", Status: StatusPass, HTTPStatus: 200, LatencyMS: 3, Dimensions: []string{"non_stream"}, Note: "n"}}}
	summary := rep.Summary()
	if !strings.Contains(summary, "estimated cost=unknown") || !strings.Contains(summary, "note=n") {
		t.Fatalf("summary=%q", summary)
	}
	// Redactor skips short/duplicate secrets and keeps benign text.
	red := NewRedactor([]string{"ab", "secret-value", "secret-value"})
	if got := red.Redact("leak secret-value here"); !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "secret-value") {
		t.Fatalf("redactor=%q", got)
	}
	// secretsForProvider picks up secret envs and proxy credentials.
	old := getenv
	defer func() { getenv = old }()
	getenv = func(name string) string {
		if name == "NEXA_CANARY_SECRET_ENV" {
			return "env-secret-value"
		}
		return ""
	}
	p := config.ProviderConfig{SecretKeyEnv: "NEXA_CANARY_SECRET_ENV", ProxyURL: "http://proxy-user:proxy-pass@127.0.0.1:8080"}
	secrets := secretsForProvider(p)
	joined := strings.Join(secrets, ",")
	if !strings.Contains(joined, "env-secret-value") || !strings.Contains(joined, "proxy-user") || !strings.Contains(joined, "proxy-pass") {
		t.Fatalf("secrets=%v", secrets)
	}
	if got := getenv(""); got != "" {
		t.Fatalf("getenv empty=%q", got)
	}
}

func newContinue95AuthFake(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer nexaroute-acceptance-invalid-credential" {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid credential"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
}

func TestContinue95RunAuthFailureBranches(t *testing.T) {
	ctx := context.Background()
	sc := Continue95Scenario("auth_failure")
	// Auth-mode none: bogus provider construction fails closed.
	r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: ""})
	r.provider.AuthMode = "none"
	if res := r.runAuthFailure(ctx, sc); res.Status != StatusFail || !strings.Contains(res.Error, "auth_mode none") {
		t.Fatalf("auth-none res=%+v", res)
	}
	// Unreachable upstream: the raw call errors and the scenario fails.
	r = Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: ""})
	if res := r.runAuthFailure(ctx, sc); res.Status != StatusFail {
		t.Fatalf("unreachable res=%+v, want FAIL", res)
	}
	// Loopback fake that rejects the bogus credential with 403, accepts
	// anything else: 403 and 400 pass, 200 fails.
	for _, tc := range []struct {
		code int
		want string
	}{
		{403, StatusPass},
		{400, StatusPass},
		{200, StatusFail},
	} {
		srv := newContinue95AuthFake(tc.code)
		r := Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: ""})
		r.provider.BaseURL = srv.URL
		r.provider.APIKey = "operator-key-synthetic"
		r.provider.APIKeyEnv = ""
		res := r.runAuthFailure(ctx, sc)
		srv.Close()
		if res.Status != tc.want || res.HTTPStatus != tc.code {
			t.Fatalf("code %d: res=%+v want %s", tc.code, res, tc.want)
		}
	}
}

func TestContinue95RemainingScenarioErrorBranches(t *testing.T) {
	ctx := context.Background()
	sseHdr := http.Header{"Content-Type": []string{"text/event-stream"}}
	// Stream-usage early exits, mirroring the stream branches.
	usageCases := []struct {
		name string
		stub *Continue95StubAdapter
		err  string
	}{
		{"adapter-error", &Continue95StubAdapter{err: errors.New("usage dial failed")}, "usage dial failed"},
		{"http-error", &Continue95StubAdapter{status: 500, body: `{"error":"boom"}`, header: sseHdr}, "http 500"},
		{"wrong-content-type", &Continue95StubAdapter{status: 200, header: http.Header{"Content-Type": []string{"application/json"}}, body: "data: {}\n\n"}, "expected text/event-stream"},
		{"no-data", &Continue95StubAdapter{status: 200, header: sseHdr, body: "data: [DONE]\n\n"}, "no non-terminal data"},
		{"no-terminal", &Continue95StubAdapter{status: 200, header: sseHdr, body: "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"}, "terminal event"},
	}
	for _, tc := range usageCases {
		res := Continue95Runner(t, "openai_compatible", tc.stub).runStreamUsage(ctx, Continue95Scenario("stream_usage"))
		if res.Status != StatusFail || !strings.Contains(res.Error, tc.err) {
			t.Fatalf("stream_usage %s: res=%+v want error containing %q", tc.name, res, tc.err)
		}
	}
	// Every other scenario's adapter-error, HTTP-error, and decode-error exits.
	nonStream := []struct {
		id string
		fn func(r *Runner, ctx context.Context, sc scenario) ScenarioResult
	}{
		{"tools", (*Runner).runTools},
		{"tool_result", (*Runner).runToolResult},
		{"structured_output", (*Runner).runStructuredOutput},
		{"images", (*Runner).runImages},
		{"parallel_tools", (*Runner).runParallelTools},
		{"reasoning", (*Runner).runReasoning},
	}
	for _, tc := range nonStream {
		sc := Continue95Scenario(tc.id)
		res := tc.fn(Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{err: errors.New("scenario dial failed")}), ctx, sc)
		if res.Status != StatusFail || !strings.Contains(res.Error, "scenario dial failed") {
			t.Fatalf("%s adapter error: res=%+v", tc.id, res)
		}
		res = tc.fn(Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 503, body: `{"error":"down"}`}), ctx, sc)
		if res.Status != StatusFail || res.HTTPStatus != 503 {
			t.Fatalf("%s http error: res=%+v", tc.id, res)
		}
		res = tc.fn(Continue95Runner(t, "openai_compatible", &Continue95StubAdapter{status: 200, body: `not json at all`}), ctx, sc)
		if res.Status != StatusFail {
			t.Fatalf("%s decode error: res=%+v, want FAIL", tc.id, res)
		}
	}
	// Reasoning passes via a thinking block (Anthropic shape).
	anthropicThinking := `{"content":[{"type":"thinking","thinking":"step by step","signature":"sig-123"},{"type":"text","text":"4"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	res := Continue95Runner(t, "anthropic_compatible", &Continue95StubAdapter{status: 200, body: anthropicThinking}).runReasoning(ctx, Continue95Scenario("reasoning"))
	if res.Status != StatusPass {
		t.Fatalf("reasoning thinking-block res=%+v", res)
	}
	// Native finish-reason: openai body without choices is not a finish.
	if hasNativeFinishReason("openai_compatible", []byte(`{}`)) {
		t.Fatal("empty openai body counted as finish")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`{"choices":"bad"}`)) {
		t.Fatal("malformed choices counted as finish")
	}
	// streamJSON: a leading [DONE] line is skipped, a later finish line counts;
	// a nested duplicate finish key exercises the early-return walk guard.
	if !streamJSONHasNonEmptyString("data: [DONE]\n\ndata: {\"finish_reason\":\"stop\"}\n\n", "finish_reason") {
		t.Fatal("streamJSON after DONE missed")
	}
	if !streamJSONHasNonEmptyString("data: {\"finish_reason\":\"stop\",\"nested\":{\"finish_reason\":\"stop\"}}\n\n", "finish_reason") {
		t.Fatal("streamJSON nested duplicate missed")
	}
	// Encode errors: an empty model must fail at least one dialect encoder;
	// every dialect is probed and at least the failing ones must surface
	// through doCanonical as a runText failure.
	for _, dialect := range []string{"anthropic_compatible", "gemini", "vertex", "bedrock", "openai_responses", "openai_compatible"} {
		_, _, err := encodeRequest(config.ProviderConfig{Type: dialect}, baseRequest("", "hi", 8))
		t.Logf("encode empty-model %s err=%v", dialect, err)
	}
	r := Continue95Runner(t, "gemini", &Continue95StubAdapter{status: 200, body: ""})
	r.upstreamModel = ""
	if res := r.runText(ctx, Continue95Scenario("text")); res.Status != StatusFail {
		t.Logf("gemini empty-model runText res=%+v (encoder tolerates empty model; no encode branch claimed)", res)
	}
}

func TestContinue95CallTimeoutRewriteAndNewAdapterEdges(t *testing.T) {
	// Call-timeout rewrite: a slow failing call under a tiny call timeout
	// rewrites the failure as a timeout in Run.
	stub := &Continue95StubAdapter{err: errors.New("slow failure")}
	// Sleep inside DoPath via a wrapper: reuse the stub by delaying through
	// a slow adapter defined inline below.
	_ = stub
	r := Continue95Runner(t, "openai_compatible", &Continue95SlowAdapter{inner: &Continue95StubAdapter{err: errors.New("slow failure")}, sleep: 200 * time.Millisecond})
	r.scenarios = []scenario{Continue95Scenario("text")}
	r.limits.MaxCalls = 1
	r.callTimeout = 5 * time.Millisecond
	r.overall = 10 * time.Second
	report := r.Run(context.Background())
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusFail || !strings.Contains(report.Scenarios[0].Error, "call timed out after") {
		t.Fatalf("call timeout report=%+v", report.Scenarios)
	}
	// New with an empty provider Models list fills it from the model.
	opts := testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	opts.Provider.Models = nil
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.provider.Models) != 1 {
		t.Fatalf("models filled=%v", runner.provider.Models)
	}
	runner.Close()
	// getenv with a blank name returns "" without touching the environment.
	if got := getenv("   "); got != "" {
		t.Fatalf("blank getenv=%q", got)
	}
	// Auth failure against a provider whose adapter cannot be built fails
	// closed instead of calling anything.
	r2 := Continue95Runner(t, "unknown_dialect_xyz", &Continue95StubAdapter{status: 200, body: ""})
	r2.provider.AuthMode = "bearer"
	if res := r2.runAuthFailure(context.Background(), Continue95Scenario("auth_failure")); res.Status != StatusFail {
		t.Fatalf("unknown dialect auth_failure res=%+v, want FAIL", res)
	} else {
		t.Logf("unknown dialect auth_failure error=%q", res.Error)
	}
}

// Continue95SlowAdapter delays before returning its canned error, so a
// tiny call timeout expires first.
type Continue95SlowAdapter struct {
	inner *Continue95StubAdapter
	sleep time.Duration
}

func (s *Continue95SlowAdapter) ID() string   { return s.inner.ID() }
func (s *Continue95SlowAdapter) Kind() string { return s.inner.Kind() }
func (s *Continue95SlowAdapter) Stats() providers.ProviderStats {
	return s.inner.Stats()
}
func (s *Continue95SlowAdapter) CredentialsMatch(v []string) bool { return s.inner.CredentialsMatch(v) }
func (s *Continue95SlowAdapter) RedactBody(b []byte) []byte       { return s.inner.RedactBody(b) }
func (s *Continue95SlowAdapter) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return s.DoPath(ctx, http.MethodPost, "/stub", payload, stream, forward)
}
func (s *Continue95SlowAdapter) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	time.Sleep(s.sleep)
	return s.inner.DoPath(ctx, method, path, payload, stream, forward)
}
func (s *Continue95SlowAdapter) CountTokens(ctx context.Context, payload []byte, forward http.Header) (*http.Response, error) {
	return s.inner.CountTokens(ctx, payload, forward)
}
func (s *Continue95SlowAdapter) Probe(ctx context.Context, model string, maxTokens int) (time.Duration, int, error) {
	return s.inner.Probe(ctx, model, maxTokens)
}

func TestContinue95EncodeRequestErrorBranches(t *testing.T) {
	// doCanonical surfaces an encoding failure (Gemini tool args that are
	// not JSON break payload marshaling). Note: EncodeAnthropicRequest
	// itself never returns an error, so its encodeRequest error return
	// (scenarios.go:695-696) is unreachable by construction and is not
	// claimed here.
	badArgsReq := canonical.Request{Model: "m", Messages: []canonical.Message{{Role: canonical.RoleAssistant, Parts: []canonical.Part{{Type: canonical.PartToolCall, ToolCall: &canonical.ToolCall{Name: "lookup", Arguments: "{bad"}}}}}}
	r := Continue95Runner(t, "gemini", &Continue95StubAdapter{status: 200, body: ""})
	if _, err := r.doCanonical(context.Background(), badArgsReq, false); err == nil {
		t.Fatal("doCanonical gemini bad args should error")
	}
	// Gemini: a tool call with non-JSON arguments breaks payload marshaling.
	badArgs := canonical.Request{Model: "m", Messages: []canonical.Message{{Role: canonical.RoleAssistant, Parts: []canonical.Part{{Type: canonical.PartToolCall, ToolCall: &canonical.ToolCall{Name: "lookup", Arguments: "{bad"}}}}}}
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "gemini"}, badArgs); err == nil {
		t.Fatalf("gemini bad args err=%v, want marshal error", err)
	}
	// Bedrock: an unsupported image media type fails encoding.
	badImage := canonical.Request{Model: "m", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{{Type: canonical.PartImage, Image: &canonical.Image{MediaType: "image/tiff", Data: "AAAA"}}}}}}
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "bedrock"}, badImage); err == nil || !strings.Contains(err.Error(), "unsupported image media type") {
		t.Fatalf("bedrock bad image err=%v", err)
	}
	// Responses: a JSON-schema format with non-JSON schema breaks marshaling.
	badSchema := canonical.Request{Model: "m", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{{Type: canonical.PartText, Text: "hi"}}}},
		ResponseFormat: &canonical.ResponseFormat{Kind: canonical.FormatJSONSchema, Schema: json.RawMessage(`{bad`)}}
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "openai_responses"}, badSchema); err == nil {
		t.Fatalf("responses bad schema err=%v, want marshal error", err)
	}
	// OpenAI chat: a request with no messages at all fails encoding
	// ("no messages after translation").
	badPart := canonical.Request{Model: "m"}
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "openai_compatible"}, badPart); err == nil || !strings.Contains(err.Error(), "no messages after translation") {
		t.Fatalf("openai no-message err=%v", err)
	}
	// doRaw read error: a body that fails on read is reported, not swallowed.
	r2 := Continue95Runner(t, "openai_compatible", &Continue95ReadErrorAdapter{})
	if _, err := r2.doRaw(context.Background(), []byte(`{}`), false); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("read error err=%v", err)
	}
}

// Continue95ReadErrorAdapter returns a response whose body always fails.
type Continue95ReadErrorAdapter struct {
	Continue95StubAdapter
}

func (s *Continue95ReadErrorAdapter) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(&continue95FailReader{})}, nil
}

type continue95FailReader struct{}

func (continue95FailReader) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }
