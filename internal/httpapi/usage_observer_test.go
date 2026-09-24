package httpapi

import (
	"io"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	usageacct "github.com/ali-shortcuts/nexaroute/internal/usage"
)

func testUsageDeployment() router.Deployment {
	return router.Deployment{
		ID: "p/m", ProviderID: "p", ProviderType: "openai_compatible", Model: "m",
		Pricing: &config.PricingConfig{InputUSDPerMillion: 2, OutputUSDPerMillion: 8},
	}
}

func TestUsageObserverRecordsExactNonStreamOpenAIUsage(t *testing.T) {
	m := usageacct.New()
	body := &usageObserverBody{
		ReadCloser: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":250,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":50}}}`)),
		manager: m, deployment: testUsageDeployment(), protocol: "openai",
		jsonBuf: make([]byte, 0, 1024),
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	st := m.Total()
	if st.ExactRequests != 1 || st.UnknownRequests != 0 || st.InputTokens != 1000 || st.OutputTokens != 250 || st.CacheReadInputTokens != 100 || st.ReasoningTokens != 50 {
		t.Fatalf("unexpected usage: %+v", st)
	}
	if st.PricedRequests != 1 || st.EstimatedCostNanoUSD != 4_000_000 {
		t.Fatalf("unexpected cost: %+v", st)
	}
}

func TestUsageObserverMergesAnthropicSSEUsageAcrossFrames(t *testing.T) {
	m := usageacct.New()
	d := testUsageDeployment()
	d.ProviderType = "anthropic_compatible"
	stream := "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":\n" +
		"data: {\"input_tokens\":120,\"output_tokens\":0,\"cache_read_input_tokens\":20}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":30}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	body := &usageObserverBody{
		ReadCloser: io.NopCloser(strings.NewReader(stream)),
		manager: m, deployment: d, protocol: "anthropic", stream: true,
		sse: &usageSSETracker{protocol: "anthropic"},
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	st := m.Total()
	if st.ExactRequests != 1 || st.InputTokens != 120 || st.OutputTokens != 30 || st.CacheReadInputTokens != 20 {
		t.Fatalf("unexpected SSE usage: %+v", st)
	}
}

func TestUsageObserverMarksCompletedOpenAIStreamWithoutUsageUnknown(t *testing.T) {
	m := usageacct.New()
	stream := "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	body := &usageObserverBody{
		ReadCloser: io.NopCloser(strings.NewReader(stream)),
		manager: m, deployment: testUsageDeployment(), protocol: "openai", stream: true,
		sse: &usageSSETracker{protocol: "openai"},
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	st := m.Total()
	if st.ExactRequests != 0 || st.UnknownRequests != 1 || st.PricedRequests != 0 {
		t.Fatalf("missing usage must stay unknown: %+v", st)
	}
}

func TestUsageObserverDoesNotTreatEmptyUsageEnvelopeAsExact(t *testing.T) {
	m := usageacct.New()
	body := &usageObserverBody{
		ReadCloser: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`)),
		manager: m, deployment: testUsageDeployment(), protocol: "openai",
		jsonBuf: make([]byte, 0, 128),
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
	if got := m.Total(); got.UnknownRequests != 1 || got.ExactRequests != 0 {
		t.Fatalf("empty usage envelope became exact: %+v", got)
	}
}
