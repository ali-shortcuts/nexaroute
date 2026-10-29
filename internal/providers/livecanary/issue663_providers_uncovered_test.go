package livecanary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

func issue663ScenarioRunner(t *testing.T, baseURL, id string) (*Runner, scenario) {
	t.Helper()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	opts := testOptions(baseURL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{id}
	opts.MaxCalls = 1
	if id == "reasoning" {
		// The reasoning scenario requires at least 512 output tokens.
		opts.MaxOutputTokens = 2048
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	sc, err := ResolveScenarios([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	return r, sc[0]
}

func issue663ReplyServer(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// issue663ChatReply renders the fake provider's OpenAI-shaped reply with
// the given message object and usage object (already-encoded JSON).
func issue663ChatReply(messageJSON, usageJSON string) string {
	return `{"id":"chatcmpl-canary","object":"chat.completion","model":"canary-model","choices":[{"index":0,"message":` +
		messageJSON + `,"finish_reason":"stop"}],"usage":` + usageJSON + `}`
}

func issue663TextReply(content, usage string) string {
	if usage == "" {
		usage = `{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}`
	}
	return issue663ChatReply(`{"role":"assistant","content":`+jsonString(content)+`}`, usage)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestIssue663ProvidersNewValidation(t *testing.T) {
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	// No model at all.
	opts := testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Model = config.ModelConfig{}
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "requires a model") {
		t.Fatalf("no-model New = %v, want a requires-a-model error", err)
	}
	// Model id defaults from the upstream model name and vice versa.
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Model = config.ModelConfig{Model: "m1"}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.model.ID != "m1" || r.model.Model != "m1" {
		t.Fatalf("model defaulting = %+v, want ID and Model m1", r.model)
	}
	r.Close()
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Model = config.ModelConfig{ID: "m2"}
	r, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.model.Model != "m2" {
		t.Fatalf("model defaulting = %+v, want Model m2", r.model)
	}
	r.Close()
	// Empty provider model list adopts the requested model.
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.Models = nil
	opts.Model = config.ModelConfig{ID: "m3", Model: "m3", Weight: 1}
	r, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.provider.Models) != 1 || r.provider.Models[0].ID != "m3" {
		t.Fatalf("provider models = %+v, want [m3]", r.provider.Models)
	}
	r.Close()
	// A non-http provider scheme fails provider validation.
	opts = testOptions("ftp://127.0.0.1:1", "NEXA_CANARY_KEY")
	if _, err := New(opts); err == nil {
		t.Fatal("ftp-scheme provider was accepted")
	}
	// Missing credential with no env name falls back to "api_key_env".
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.APIKeyEnv = ""
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "api_key_env") || !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("empty-env credential error = %v, want api_key_env + ErrMissingCredential", err)
	}
	// Bedrock naming a secret env whose value is unset fails before
	// any network call. (An empty secret_key_env is rejected earlier
	// by provider validation, so it cannot reach this check.)
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Provider.Type = "bedrock"
	opts.Provider.SecretKeyEnv = "NEXA_CANARY_SECRET_UNSET"
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "bedrock secret") {
		t.Fatalf("bedrock without secret = %v, want a bedrock secret error", err)
	}
	// An unknown scenario name surfaces from New.
	opts = testOptions("http://127.0.0.1:1", "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"bogus"}
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "unknown scenario") {
		t.Fatalf("unknown scenario New = %v, want an unknown-scenario error", err)
	}
	// A base URL with a query string passes config validation but
	// fails adapter construction: the egress policy rejects targets
	// carrying a raw query.
	opts = testOptions("http://127.0.0.1:1/?q=1", "NEXA_CANARY_KEY")
	if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "egress target is malformed") {
		t.Fatalf("query base URL New = %v, want an egress-target malformed error", err)
	}
}

func TestIssue663ProvidersRunPaths(t *testing.T) {
	// Run with a nil context falls back to Background.
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	runner, err := New(testOptions(srv.URL, "NEXA_CANARY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(nil)
	if report.Outcome != OutcomePass || len(report.Scenarios) != 5 {
		t.Fatalf("nil-context run = outcome %s, %d scenarios, want PASS/5", report.Outcome, len(report.Scenarios))
	}
	// Call budget already exhausted skips without a call.
	r := &Runner{
		provider:    testProvider("http://127.0.0.1:1", "NEXA_CANARY_KEY"),
		model:       config.ModelConfig{ID: "m", Model: "canary-model"},
		scenarios:   []scenario{mustScenario(t, "text")},
		limits:      Limits{MaxCalls: 0},
		callTimeout: time.Second,
		overall:     10 * time.Second,
		now:         func() time.Time { return time.Now().UTC() },
	}
	report = r.Run(context.Background())
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusSkip || !strings.Contains(report.Scenarios[0].Note, "call budget exhausted") {
		t.Fatalf("budget run = %+v, want a call-budget-exhausted skip", report.Scenarios)
	}
	// A call timeout (overall context alive) rewrites the failure.
	// The release channel is closed after Run returns so the blocked
	// fake handler can exit; otherwise the deferred server Close
	// waits forever on the still-open handler (the adapter returns
	// on the call deadline without closing the in-flight request).
	fake = &fakeProvider{block: true, release: make(chan struct{})}
	srv = httptest.NewServer(fake)
	defer srv.Close()
	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 1
	opts.CallTimeout = time.Second
	opts.OverallTimeout = 10 * time.Second
	runner, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report = runner.Run(context.Background())
	close(fake.release)
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusFail || !strings.Contains(report.Scenarios[0].Error, "call timed out after 1s") {
		t.Fatalf("call-timeout run = %+v, want a call-timed-out failure", report.Scenarios)
	}
	// A 401 on a normal scenario stops the remaining scenarios.
	fake = &fakeProvider{requireAuth: true}
	srv = httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "wrong-secret")
	opts = testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"text,stream"}
	opts.MaxCalls = 2
	runner, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report = runner.Run(context.Background())
	if len(report.Scenarios) != 2 || report.Scenarios[0].Status != StatusFail || report.Scenarios[0].HTTPStatus != http.StatusUnauthorized ||
		report.Scenarios[1].Status != StatusSkip || !strings.Contains(report.Scenarios[1].Note, "rejected the credential") {
		t.Fatalf("auth-rejection run = %+v, want FAIL 401 then a rejection skip", report.Scenarios)
	}
	// Unknown scenario ids fail from the dispatcher.
	if res := (&Runner{}).runScenario(context.Background(), scenario{ID: "bogus"}); res.Status != StatusFail || res.Error != "unknown scenario" {
		t.Fatalf("unknown scenario dispatch = %+v, want FAIL/unknown scenario", res)
	}
}

func mustScenario(t *testing.T, id string) scenario {
	t.Helper()
	sc, err := ResolveScenarios([]string{id})
	if err != nil || len(sc) != 1 {
		t.Fatalf("ResolveScenarios(%q) = %v %v", id, sc, err)
	}
	return sc[0]
}

func TestIssue663ProvidersRedactHelpers(t *testing.T) {
	var nilRunner *Runner
	if got := nilRunner.redact("plain"); got != "plain" {
		t.Fatalf("nil runner redact = %q, want plain", got)
	}
	if got := (&Runner{}).redact("plain"); got != "plain" {
		t.Fatalf("nil redactor redact = %q, want plain", got)
	}
	r := &Runner{redactor: NewRedactor(nil)}
	long := strings.Repeat("x", 400) + "\r\n" + strings.Repeat("y", 400)
	got := r.redactSnippet([]byte(long))
	if !strings.HasSuffix(got, "...") || len(got) != maxErrorSnippet+3 || strings.Contains(got, "\r") || strings.Contains(got, "\n") {
		t.Fatalf("redactSnippet = len %d suffix %q, want %d chars ending ... without CR/LF", len(got), got[len(got)-3:], maxErrorSnippet+3)
	}
	red := NewRedactor([]string{"ab", "abcd", "abcd"})
	if got := red.Redact("xxabcdxx"); got != "xx[REDACTED]xx" {
		t.Fatalf("redactor dedup/short-skip = %q, want xx[REDACTED]xx", got)
	}
	if got := red.Redact("ab"); got != "ab" {
		t.Fatalf("short secret was redacted: %q", got)
	}
	if got := getenv(""); got != "" {
		t.Fatalf("getenv(\"\") = %q, want empty", got)
	}
	t.Setenv("ISSUE663_CANARY_ENV", "v123")
	if got := getenv("ISSUE663_CANARY_ENV"); got != "v123" {
		t.Fatalf("getenv set var = %q, want v123", got)
	}
	// Summary branches: unknown cost estimate and a noted scenario.
	rep := Report{
		Provider:  ProviderEvidence{ID: "p", Dialect: "openai_compatible", Model: "m"},
		Limits:    Limits{MaxCostUSD: 1},
		Scenarios: []ScenarioResult{{ID: "text", Status: StatusSkip, Note: "why"}},
	}
	summary := rep.Summary()
	if !strings.Contains(summary, "estimated cost=unknown") || !strings.Contains(summary, "note=why") {
		t.Fatalf("summary = %q, want unknown-cost and note branches", summary)
	}
}

func TestIssue663ProvidersSecretsForProviderEnv(t *testing.T) {
	old := getenv
	getenv = func(name string) string {
		if name == "MY_SECRET_ENV" {
			return "env-secret-123"
		}
		return old(name)
	}
	defer func() { getenv = old }()
	p := config.ProviderConfig{SecretKeyEnv: "MY_SECRET_ENV"}
	secrets := secretsForProvider(p)
	found := false
	for _, s := range secrets {
		if s == "env-secret-123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("secretsForProvider = %v, want the env secret", secrets)
	}
	if got := NewRedactor(secrets).Redact("env-secret-123"); got != "[REDACTED]" {
		t.Fatalf("env secret redaction = %q", got)
	}
}

func TestIssue663ProvidersResolveAndCatalog(t *testing.T) {
	sc, err := ResolveScenarios(nil)
	if err != nil || len(sc) != 5 || sc[0].ID != "text" {
		t.Fatalf("empty ResolveScenarios = %v %v, want the 5 core scenarios", sc, err)
	}
	sc, err = ResolveScenarios([]string{"text,,stream"})
	if err != nil || len(sc) != 2 || sc[0].ID != "text" || sc[1].ID != "stream" {
		t.Fatalf("empty-part ResolveScenarios = %v %v, want text,stream", sc, err)
	}
	if _, err := ResolveScenarios([]string{","}); err == nil || !strings.Contains(err.Error(), "no canary scenarios") {
		t.Fatalf("no-selection ResolveScenarios = %v, want a no-scenarios error", err)
	}
	if unsupported, note := scenarioUnsupported("bogus-dialect", mustScenario(t, "text")); unsupported || note != "" {
		t.Fatalf("unknown dialect unsupported = %v %q, want false/empty", unsupported, note)
	}
	if got := scenarioCallOutputTokens("reasoning", "anthropic_compatible", 64); got != 2048 {
		t.Fatalf("anthropic reasoning tokens = %d, want 2048", got)
	}
	if got := scenarioCallOutputTokens("reasoning", "openai_compatible", 64); got != 512 {
		t.Fatalf("small-limit reasoning tokens = %d, want 512", got)
	}
	if _, err := bogusAuthProvider(config.ProviderConfig{AuthMode: "none"}); err == nil {
		t.Fatal("bogusAuthProvider accepted auth_mode none")
	}
	if got := pathOr("", "/fallback"); got != "/fallback" {
		t.Fatalf("pathOr empty = %q, want /fallback", got)
	}
	// decodeResponse across dialects.
	decodeCases := []struct {
		dialect string
		body    string
	}{
		{"anthropic_compatible", `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`},
		{"gemini", `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`},
		{"bedrock", `{"output":{"message":{"content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":2}}`},
		{"openai_responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`},
	}
	for _, tc := range decodeCases {
		resp, err := decodeResponse(tc.dialect, []byte(tc.body))
		if err != nil || resp.Text() != "hi" {
			t.Fatalf("decodeResponse(%s) = %+v %v, want text hi", tc.dialect, resp, err)
		}
	}
	if hasNativeFinishReason("gemini", []byte(`{"candidates":[{"finishReason":""}]}`)) {
		t.Fatal("empty gemini finishReason counted as native")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`{"choices":[{"finish_reason":null}]}`)) {
		t.Fatal("null openai finish_reason counted as native")
	}
	// streamJSONHasNonEmptyString: early return inside nested walks,
	// empty/[DONE] lines, and undecodable lines.
	if !streamJSONHasNonEmptyString("data: [{\"finishReason\":\"STOP\"},{\"other\":1}]\n\n", "finishReason") {
		t.Fatal("nested finishReason was not found")
	}
	if streamJSONHasNonEmptyString("data: [DONE]\n\n", "finishReason") {
		t.Fatal("[DONE] line counted as a finish reason")
	}
	if streamJSONHasNonEmptyString("data: {bad\n\n", "finishReason") {
		t.Fatal("undecodable line counted as a finish reason")
	}
	// extractStreamUsage: non-float usage values and undecodable lines.
	if _, _, found := extractStreamUsage([]byte("data: {\"usage\":{\"prompt_tokens\":\"seven\",\"completion_tokens\":2}}\n\n")); found {
		t.Fatal("string usage tokens counted as found")
	}
	if _, _, found := extractStreamUsage([]byte("data: {bad\n\n")); found {
		t.Fatal("undecodable usage line counted as found")
	}
}

func TestIssue663ProvidersEncodeRequestBranches(t *testing.T) {
	// Anthropic request encoding is infallible: EncodeAnthropicRequest
	// (canonical/anthropic.go) contains no error return, so the error
	// branch after it in encodeRequest is dead and deliberately left
	// untested. The "unsupported Anthropic message role" error lives
	// in the response decoder, not the request encoder.
	// Gemini: tool-call arguments ride into the payload as a raw JSON
	// message, so invalid arguments fail the final marshal. (Tool
	// *definitions* are sanitized first and cannot trigger this.)
	badCallReq := canonical.Request{Model: "m",
		Messages: []canonical.Message{{Role: canonical.RoleAssistant, Parts: []canonical.Part{
			{Type: canonical.PartToolCall, ToolCall: &canonical.ToolCall{Name: "f", Arguments: "{bad"}},
		}}},
	}
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "gemini"}, badCallReq); err == nil {
		t.Fatal("gemini bad tool-call arguments were accepted")
	}
	// Bedrock: unsupported image media type.
	imgReq := canonical.Request{Model: "m",
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{
			{Type: canonical.PartImage, Image: &canonical.Image{MediaType: "image/bogus", Data: "x"}},
			{Type: canonical.PartText, Text: "hi"},
		}}},
	}
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "bedrock"}, imgReq); err == nil || !strings.Contains(err.Error(), "unsupported image media type") {
		t.Fatalf("bedrock bad image = %v, want an unsupported-media-type error", err)
	}
	// OpenAI default: no messages after translation.
	if _, _, err := encodeRequest(config.ProviderConfig{Type: "openai_compatible"},
		canonical.Request{Model: "m"}); err == nil {
		t.Fatal("openai empty-message request was accepted")
	}
	// OpenAI default: tool-definition parameters are sanitized
	// (unmarshal-or-drop) and the only other raw message is a
	// hardcoded valid literal, so the marshal error return in
	// encodeRequest is dead and deliberately left untested.
	// Vertex: default location and stream path.
	_, path, err := encodeRequest(config.ProviderConfig{Type: "vertex", VertexProject: "p"},
		canonical.Request{Model: "m", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{{Type: canonical.PartText, Text: "hi"}}}}})
	if err != nil || !strings.Contains(path, "us-central1") {
		t.Fatalf("vertex default location path = %q %v, want us-central1", path, err)
	}
	streamReq := canonical.Request{Model: "m", Stream: true,
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.Part{{Type: canonical.PartText, Text: "hi"}}}}}
	_, path, err = encodeRequest(config.ProviderConfig{Type: "vertex", VertexProject: "p", VertexLocation: "eu"}, streamReq)
	if err != nil || !strings.HasSuffix(path, ":streamGenerateContent?alt=sse") {
		t.Fatalf("vertex stream path = %q %v, want a streamGenerateContent suffix", path, err)
	}
	// Azure: default API version.
	_, path, err = encodeRequest(config.ProviderConfig{Type: "azure_openai"}, streamReq)
	if err != nil || !strings.Contains(path, "api-version=2024-10-21") {
		t.Fatalf("azure default version path = %q %v, want 2024-10-21", path, err)
	}
	// defaultPath via runErrorEnvelope for anthropic and responses.
	for _, dialect := range []string{"anthropic_compatible", "openai_responses"} {
		fake := &fakeProvider{}
		srv := httptest.NewServer(fake)
		t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
		opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
		opts.Provider.Type = dialect
		opts.Provider.AuthMode = "none"
		opts.Provider.APIKeyEnv = ""
		opts.Scenarios = []string{"error_envelope"}
		opts.MaxCalls = 1
		runner, err := New(opts)
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		report := runner.Run(context.Background())
		runner.Close()
		srv.Close()
		if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusPass {
			t.Fatalf("error_envelope via %s = %+v, want PASS", dialect, report.Scenarios)
		}
	}
	// doCanonical surfaces encode failures (empty messages fail the
	// OpenAI encoder before any network call).
	r, _ := issue663ScenarioRunner(t, "http://127.0.0.1:1", "text")
	if _, err := r.doCanonical(context.Background(), canonical.Request{Model: "canary-model"}, false); err == nil {
		t.Fatal("doCanonical with no messages was accepted")
	}
	// doRawWith negative latency clamps to zero via a backward clock.
	nowCalls := []time.Time{time.Unix(2000, 0).UTC(), time.Unix(1000, 0).UTC()}
	nowIdx := 0
	r2, _ := issue663ScenarioRunner(t, "http://127.0.0.1:1", "text")
	r2.now = func() time.Time {
		t := nowCalls[nowIdx]
		if nowIdx < len(nowCalls)-1 {
			nowIdx++
		}
		return t
	}
	// The clamp is exercised through the stub adapter below, whose
	// body arrives even though the backward clock makes the end
	// timestamp earlier than the start timestamp.
	stubRunner := &Runner{provider: testProvider("http://127.0.0.1:1", "NEXA_CANARY_KEY"),
		now: r2.now, redactor: NewRedactor(nil)}
	raw, err := stubRunner.doRawWith(&issue663StubAdapter{body: []byte(`{}`)}, context.Background(), []byte(`{}`), false, "/x")
	if err != nil || raw.latency != 0 {
		t.Fatalf("backward-clock doRawWith = %+v %v, want latency 0", raw, err)
	}
	// doRawWith read and oversize failures.
	if _, err := stubRunner.doRawWith(&issue663StubAdapter{readErr: errors.New("boom")}, context.Background(), []byte(`{}`), false, "/x"); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("read-error doRawWith = %v, want a could-not-be-read error", err)
	}
	big := bytes.Repeat([]byte("x"), int(maxResponseBytes)+1)
	if _, err := stubRunner.doRawWith(&issue663StubAdapter{body: big}, context.Background(), []byte(`{}`), false, "/x"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize doRawWith = %v, want an exceeds error", err)
	}
}

// issue663StubAdapter implements providers.Adapter with a scripted body.
type issue663StubAdapter struct {
	body    []byte
	readErr error
}

func (a *issue663StubAdapter) ID() string                     { return "stub" }
func (a *issue663StubAdapter) Kind() string                   { return "stub" }
func (a *issue663StubAdapter) Stats() providers.ProviderStats { return providers.ProviderStats{} }
func (a *issue663StubAdapter) CredentialsMatch([]string) bool { return false }
func (a *issue663StubAdapter) RedactBody(b []byte) []byte     { return b }
func (a *issue663StubAdapter) Do(context.Context, []byte, bool, http.Header) (*http.Response, error) {
	return nil, nil
}
func (a *issue663StubAdapter) DoPath(_ context.Context, _, _ string, _ []byte, _ bool, _ http.Header) (*http.Response, error) {
	var rc io.ReadCloser
	if a.readErr != nil {
		rc = &issue663ErrReader{err: a.readErr}
	} else {
		rc = io.NopCloser(bytes.NewReader(a.body))
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: rc}, nil
}
func (a *issue663StubAdapter) CountTokens(context.Context, []byte, http.Header) (*http.Response, error) {
	return nil, nil
}
func (a *issue663StubAdapter) Probe(context.Context, string, int) (time.Duration, int, error) {
	return 0, 0, nil
}

type issue663ErrReader struct{ err error }

func (r *issue663ErrReader) Read([]byte) (int, error) { return 0, r.err }
func (r *issue663ErrReader) Close() error             { return nil }

// issue663LadderCase drives one runX failure/success path against a
// scripted server or a fake provider flag.
type issue663LadderCase struct {
	name     string
	id       string
	fake     *fakeProvider
	status   int
	ctype    string
	body     string
	unreach  bool
	wantErr  string
	wantPass bool
	wantDim  providers.ConformanceDimension
}

func issue663RunScenario(t *testing.T, r *Runner, sc scenario) ScenarioResult {
	t.Helper()
	ctx := context.Background()
	switch sc.ID {
	case "text":
		return r.runText(ctx, sc)
	case "stream":
		return r.runStream(ctx, sc)
	case "stream_usage":
		return r.runStreamUsage(ctx, sc)
	case "auth_failure":
		return r.runAuthFailure(ctx, sc)
	case "tools":
		return r.runTools(ctx, sc)
	case "tool_result":
		return r.runToolResult(ctx, sc)
	case "structured_output":
		return r.runStructuredOutput(ctx, sc)
	case "images":
		return r.runImages(ctx, sc)
	case "parallel_tools":
		return r.runParallelTools(ctx, sc)
	case "reasoning":
		return r.runReasoning(ctx, sc)
	case "error_envelope":
		return r.runErrorEnvelope(ctx, sc)
	default:
		t.Fatalf("no runner for %q", sc.ID)
		return ScenarioResult{}
	}
}

func TestIssue663ProvidersScenarioLadders(t *testing.T) {
	plainUsage := `{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}`
	cachedUsage := `{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}`
	otherCall := `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"other","arguments":"{\"q\":\"canary\"}"}}]}`
	badArgsCall := `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"not-json"}}]}`
	oneCall := `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup_a","arguments":"{\"q\":\"canary\"}"}}]}`
	cases := []issue663LadderCase{
		{name: "text unreachable", id: "text", unreach: true, wantErr: ""},
		{name: "text 500", id: "text", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "text decode", id: "text", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "text empty", id: "text", status: 200, ctype: "application/json", body: issue663TextReply("", ""), wantErr: "no completion text"},
		{name: "text cache dims", id: "text", status: 200, ctype: "application/json", body: issue663TextReply("OK", cachedUsage), wantPass: true, wantDim: providers.DimUsageCache},
		{name: "stream unreachable", id: "stream", unreach: true},
		{name: "stream 500", id: "stream", fake: &fakeProvider{failStream: true}, wantErr: "http 500"},
		{name: "stream non-sse", id: "stream", status: 200, ctype: "application/json", body: `{"ok":true}`, wantErr: "expected text/event-stream"},
		{name: "stream no data", id: "stream", status: 200, ctype: "text/event-stream", body: "data: [DONE]\n\n", wantErr: "no non-terminal data event"},
		{name: "stream no terminal", id: "stream", fake: &fakeProvider{truncateStream: true}, wantErr: "terminal event"},
		{name: "stream_usage unreachable", id: "stream_usage", unreach: true},
		{name: "stream_usage 500", id: "stream_usage", fake: &fakeProvider{failStream: true}, wantErr: "http 500"},
		{name: "stream_usage non-sse", id: "stream_usage", status: 200, ctype: "application/json", body: `{"ok":true}`, wantErr: "expected text/event-stream"},
		{name: "stream_usage no data", id: "stream_usage", status: 200, ctype: "text/event-stream", body: "data: [DONE]\n\n", wantErr: "no non-terminal data event"},
		{name: "stream_usage no terminal", id: "stream_usage", fake: &fakeProvider{truncateStream: true}, wantErr: "terminal event"},
		{name: "tools unreachable", id: "tools", unreach: true},
		{name: "tools 500", id: "tools", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "tools decode", id: "tools", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "tools bad args", id: "tools", status: 200, ctype: "application/json", body: issue663ChatReply(badArgsCall, plainUsage), wantErr: "not valid JSON"},
		{name: "tools other name", id: "tools", status: 200, ctype: "application/json", body: issue663ChatReply(otherCall, plainUsage), wantErr: "did not contain"},
		{name: "tools no call", id: "tools", status: 200, ctype: "application/json", body: issue663TextReply("OK", ""), wantErr: "did not contain"},
		{name: "tool_result unreachable", id: "tool_result", unreach: true},
		{name: "tool_result 500", id: "tool_result", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "tool_result decode", id: "tool_result", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "tool_result empty", id: "tool_result", status: 200, ctype: "application/json", body: issue663TextReply("", ""), wantErr: "no text after the tool-result turn"},
		{name: "structured unreachable", id: "structured_output", unreach: true},
		{name: "structured 500", id: "structured_output", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "structured decode", id: "structured_output", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "structured not object", id: "structured_output", status: 200, ctype: "application/json", body: issue663TextReply("just text", ""), wantErr: "not a JSON object"},
		{name: "structured no status", id: "structured_output", status: 200, ctype: "application/json", body: issue663TextReply(`{"other":1}`, ""), wantErr: "did not contain the requested status field"},
		{name: "images unreachable", id: "images", unreach: true},
		{name: "images 500", id: "images", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "images decode", id: "images", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "images empty", id: "images", status: 200, ctype: "application/json", body: issue663TextReply("", ""), wantErr: "no text for the image"},
		{name: "images no red", id: "images", status: 200, ctype: "application/json", body: issue663TextReply("blue", ""), wantErr: "did not reference the red"},
		{name: "parallel unreachable", id: "parallel_tools", unreach: true},
		{name: "parallel 500", id: "parallel_tools", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "parallel decode", id: "parallel_tools", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "parallel one call", id: "parallel_tools", status: 200, ctype: "application/json", body: issue663ChatReply(oneCall, plainUsage), wantErr: "at least two tool calls"},
		{name: "reasoning unreachable", id: "reasoning", unreach: true},
		{name: "reasoning 500", id: "reasoning", fake: &fakeProvider{textErrorBody: "boom"}, wantErr: "http 500"},
		{name: "reasoning decode", id: "reasoning", status: 200, ctype: "application/json", body: "not-json", wantErr: "invalid"},
		{name: "reasoning no evidence", id: "reasoning", status: 200, ctype: "application/json", body: issue663TextReply("4", ""), wantErr: "neither a thinking block nor reasoning-token usage"},
		{name: "error_envelope unreachable", id: "error_envelope", unreach: true},
		{name: "error_envelope 200", id: "error_envelope", status: 200, ctype: "application/json", body: `{"ok":true}`, wantErr: "expected a 4xx"},
		{name: "error_envelope non-json", id: "error_envelope", status: 400, ctype: "application/json", body: "not-json", wantErr: "not a JSON object"},
		{name: "error_envelope no envelope", id: "error_envelope", status: 400, ctype: "application/json", body: `{"status":"bad"}`, wantErr: "did not contain an error/message envelope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var baseURL string
			if tc.unreach {
				baseURL = "http://127.0.0.1:1"
			} else if tc.fake != nil {
				srv := httptest.NewServer(tc.fake)
				t.Cleanup(srv.Close)
				baseURL = srv.URL
			} else {
				srv := issue663ReplyServer(t, tc.status, tc.ctype, tc.body)
				baseURL = srv.URL
			}
			r, sc := issue663ScenarioRunner(t, baseURL, tc.id)
			res := issue663RunScenario(t, r, sc)
			if tc.wantPass {
				if res.Status != StatusPass {
					t.Fatalf("%s = %+v, want PASS", tc.name, res)
				}
				if tc.wantDim != "" && !contains(res.Dimensions, string(tc.wantDim)) {
					t.Fatalf("%s dimensions = %v, want %s", tc.name, res.Dimensions, tc.wantDim)
				}
				return
			}
			if res.Status != StatusFail {
				t.Fatalf("%s = %+v, want FAIL", tc.name, res)
			}
			if tc.wantErr != "" && !strings.Contains(res.Error, tc.wantErr) {
				t.Fatalf("%s error = %q, want substring %q", tc.name, res.Error, tc.wantErr)
			}
		})
	}
}

func TestIssue663ProvidersHelperEdges(t *testing.T) {
	// sanitizeBaseURL fails closed on an unparseable URL.
	if got := sanitizeBaseURL("http://127.0.0.1:1/%zz"); got != "" {
		t.Fatalf("sanitizeBaseURL bad escape = %q, want empty", got)
	}
	if got := sanitizeBaseURL("http://127.0.0.1:1/api/"); got != "http://127.0.0.1:1/api" {
		t.Fatalf("sanitizeBaseURL trailing slash = %q, want the slash trimmed", got)
	}
	// contextError reports "unknown" for a live context (both Run call
	// sites only invoke it after an error is known, so this branch is
	// exercised directly).
	if got := contextError(context.Background()); got != "unknown" {
		t.Fatalf("contextError live context = %q, want unknown", got)
	}
	// Summary renders dimensions and latency for a passing scenario.
	// LatencyMS is set explicitly: real runs can complete in under a
	// millisecond, which made this branch timing-dependent.
	rep := Report{
		Provider:  ProviderEvidence{ID: "p", Dialect: "openai_compatible", Model: "m"},
		Scenarios: []ScenarioResult{{ID: "text", Status: StatusPass, Dimensions: []string{"non_stream"}, LatencyMS: 5}},
	}
	if got := rep.Summary(); !strings.Contains(got, "dims=non_stream") || !strings.Contains(got, " 5ms") {
		t.Fatalf("summary with dims = %q, want dims=non_stream and 5ms", got)
	}
	// hasNativeFinishReason falls through when the dialect's key is absent.
	if hasNativeFinishReason("gemini", []byte(`{"other":1}`)) {
		t.Fatal("gemini body without candidates counted as native")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`{"other":1}`)) {
		t.Fatal("openai body without choices counted as native")
	}
}

func TestIssue663ProvidersReasoningThinkingBlock(t *testing.T) {
	// A thinking block (not reasoning tokens) satisfies runReasoning.
	body := `{"content":[{"type":"thinking","thinking":"2+2 is 4","signature":"sig"},{"type":"text","text":"4"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	srv := issue663ReplyServer(t, 200, "application/json", body)
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Provider.Type = "anthropic_compatible"
	opts.Provider.AuthMode = "none"
	opts.Provider.APIKeyEnv = ""
	opts.Scenarios = []string{"reasoning"}
	opts.MaxCalls = 1
	opts.MaxOutputTokens = 2048
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	res := runner.runReasoning(context.Background(), mustScenario(t, "reasoning"))
	if res.Status != StatusPass {
		t.Fatalf("reasoning with thinking block = %+v, want PASS", res)
	}
}

func TestIssue663ProvidersAuthFailureDirect(t *testing.T) {
	// Auth-mode none fails before any adapter is built.
	r := &Runner{provider: testProvider("http://127.0.0.1:1", "NEXA_CANARY_KEY"),
		redactor: NewRedactor(nil), now: func() time.Time { return time.Now().UTC() }}
	r.provider.AuthMode = "none"
	res := r.runAuthFailure(context.Background(), mustScenario(t, "auth_failure"))
	if res.Status != StatusFail || !strings.Contains(res.Error, "auth_failure does not apply to a provider with auth_mode none") {
		t.Fatalf("auth-none runAuthFailure = %+v, want a no-API-key failure", res)
	}
	// A bedrock provider naming an unset secret env passes validation
	// (the name is present) but fails adapter construction, where the
	// empty secret value is rejected.
	r.provider.Type = "bedrock"
	r.provider.AuthMode = "bearer"
	r.provider.SecretKeyEnv = "NEXA_CANARY_SECRET_UNSET"
	res = r.runAuthFailure(context.Background(), mustScenario(t, "auth_failure"))
	if res.Status != StatusFail || !strings.Contains(res.Error, "bedrock secret") {
		t.Fatalf("bedrock runAuthFailure = %+v, want a bedrock secret failure", res)
	}
	// An unreachable base URL fails the raw call.
	r.provider.Type = "openai_compatible"
	res = r.runAuthFailure(context.Background(), mustScenario(t, "auth_failure"))
	if res.Status != StatusFail {
		t.Fatalf("unreachable runAuthFailure = %+v, want FAIL", res)
	}
}
