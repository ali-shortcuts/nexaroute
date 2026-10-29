package livecanary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestResolveAcceptanceScenarioSet(t *testing.T) {
	scenarios, err := ResolveScenarios([]string{"acceptance"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sc := range scenarios {
		ids = append(ids, sc.ID)
	}
	want := []string{"text", "stream", "stream_usage", "tools", "tool_result", "auth_failure", "error_envelope"}
	if len(ids) != len(want) {
		t.Fatalf("acceptance ids=%v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("acceptance ids=%v, want %v", ids, want)
		}
	}
}

func TestAcceptanceSetPassesAgainstFixture(t *testing.T) {
	fake := &fakeProvider{requireAuth: true, streamUsage: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"acceptance"}
	opts.MaxCalls = 7
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomePass {
		t.Fatalf("outcome=%s\n%s", report.Outcome, report.Summary())
	}
	if report.Limits.CallsUsed != 7 {
		t.Fatalf("calls=%d, want 7 (one per acceptance scenario)", report.Limits.CallsUsed)
	}
	byID := map[string]ScenarioResult{}
	for _, r := range report.Scenarios {
		byID[r.ID] = r
	}
	if byID["auth_failure"].Status != StatusPass || byID["auth_failure"].HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("auth_failure=%+v, want PASS on 401", byID["auth_failure"])
	}
	if strings.Contains(report.Summary(), "canary-test-secret-1234") || strings.Contains(report.Summary(), "nexaroute-acceptance-invalid-credential") {
		t.Fatal("report summary leaked a credential value")
	}
	if byID["stream_usage"].Status != StatusPass || byID["stream_usage"].InputTokens != 7 || byID["stream_usage"].OutputTokens != 2 {
		t.Fatalf("stream_usage=%+v, want PASS with input=7 output=2", byID["stream_usage"])
	}
}

func TestAuthFailureFailsWhenBogusCredentialIsAccepted(t *testing.T) {
	// Without requireAuth the fixture accepts everything, so a bogus
	// credential returning 200 must FAIL the scenario: the scenario has
	// to discriminate, not merely record a call.
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"auth_failure"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomeFailed {
		t.Fatalf("outcome=%s, want FAILED\n%s", report.Outcome, report.Summary())
	}
	if len(report.Scenarios) != 1 || report.Scenarios[0].HTTPStatus != http.StatusOK {
		t.Fatalf("scenario=%+v, want a 200 failure record", report.Scenarios)
	}
}

func TestStreamUsageFailsWithoutUsage(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"stream_usage"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomeFailed || len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusFail {
		t.Fatalf("outcome=%s scenarios=%+v, want one FAIL", report.Outcome, report.Scenarios)
	}
}

func TestTruncatedStreamFailsWithoutRetry(t *testing.T) {
	fake := &fakeProvider{truncateStream: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"stream"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomeFailed {
		t.Fatalf("outcome=%s, want FAILED for a truncated stream\n%s", report.Outcome, report.Summary())
	}
	if fake.hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1: a truncated stream must not be retried as failover", fake.hits.Load())
	}
}

func TestAuthFailureSkippedForAuthNone(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	provider := testProvider(srv.URL, "")
	provider.AuthMode = "none"
	provider.APIKeyEnv = ""
	opts := Options{
		Provider:        provider,
		Model:           config.ModelConfig{ID: "m", Model: "canary-model"},
		Scenarios:       []string{"auth_failure"},
		MaxCalls:        1,
		MaxOutputTokens: 64,
		MaxInputTokens:  128,
		CallTimeout:     testOptions(srv.URL, "").CallTimeout,
		OverallTimeout:  testOptions(srv.URL, "").OverallTimeout,
		MaxCostUSD:      1,
	}
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusSkip {
		t.Fatalf("scenarios=%+v, want one SKIP for auth_mode none", report.Scenarios)
	}
	if fake.hits.Load() != 0 {
		t.Fatalf("hits=%d, want 0: a skipped auth scenario must make no call", fake.hits.Load())
	}
}

func TestBogusAuthProviderNeverCarriesRealCredential(t *testing.T) {
	provider := config.ProviderConfig{
		ID: "p", Type: "openai_compatible", BaseURL: "https://example.invalid/v1",
		AuthMode: "bearer", APIKeyEnv: "NEXA_CANARY_KEY",
		Credentials: []config.CredentialConfig{{Name: "pool", APIKey: "pool-real-key", Enabled: true}},
		Headers:     map[string]string{"X-Custom": "keep-me", "X-Api-Key": "header-real-key"},
	}
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	bogus, err := bogusAuthProvider(provider)
	if err != nil {
		t.Fatal(err)
	}
	if bogus.APIKeyEnv != "" || bogus.Credentials != nil {
		t.Fatalf("bogus provider still references real credential sources: %+v", bogus)
	}
	for _, cred := range bogus.ResolvedCredentials() {
		if cred == "canary-test-secret-1234" || cred == "pool-real-key" {
			t.Fatalf("bogus provider resolved a real credential: %q", cred)
		}
	}
	if bogus.Headers["X-Custom"] != "keep-me" {
		t.Fatalf("non-auth headers must survive: %+v", bogus.Headers)
	}
	if _, ok := bogus.Headers["X-Api-Key"]; ok {
		t.Fatalf("auth-shaped header survived: %+v", bogus.Headers)
	}
	// Bedrock keeps its secret env name: the secret itself is read at
	// request time and only the access-key id is replaced.
	bedrock := provider
	bedrock.Type = "bedrock"
	bedrock.SecretKeyEnv = "NEXA_CANARY_SECRET"
	bogusBedrock, err := bogusAuthProvider(bedrock)
	if err != nil {
		t.Fatal(err)
	}
	if bogusBedrock.SecretKeyEnv != "NEXA_CANARY_SECRET" {
		t.Fatalf("bedrock bogus provider lost secret_key_env: %+v", bogusBedrock)
	}
}

func TestStreamTerminalNullFinishReasonIsNotTerminal(t *testing.T) {
	// Regression for the truncated-stream false PASS: a delta chunk that
	// mentions finish_reason with a null value is an open stream.
	open := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n"
	if streamTerminal("openai_compatible", open) {
		t.Fatal("null finish_reason counted as a terminal event")
	}
	closed := open + "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	if !streamTerminal("openai_compatible", closed) {
		t.Fatal("stream with a real finish_reason and [DONE] not terminal")
	}
	if streamTerminal("gemini", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n") {
		t.Fatal("Gemini stream without finishReason counted as terminal")
	}
	if !streamTerminal("gemini", "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n") {
		t.Fatal("Gemini stream with finishReason not terminal")
	}
}

func TestExtractStreamUsageDialects(t *testing.T) {
	cases := []struct {
		name string
		body string
		in   int
		out  int
	}{
		{"openai", "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n", 3, 4},
		{"anthropic", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n", 5, 6},
		{"gemini", "data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":2}}\n\n", 9, 2},
		{"bedrock-envelope", "data: {\"eventType\":\"metadata\",\"payload\":{\"usage\":{\"inputTokens\":10,\"outputTokens\":3}}}\n\n", 10, 3},
	}
	for _, tc := range cases {
		in, out, found := extractStreamUsage([]byte(tc.body))
		if !found || in != tc.in || out != tc.out {
			t.Fatalf("%s: got in=%d out=%d found=%v, want %d/%d", tc.name, in, out, found, tc.in, tc.out)
		}
	}
	if _, _, found := extractStreamUsage([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")); found {
		t.Fatal("a stream without usage must not report usage")
	}
}
