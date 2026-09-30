package compat

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Finding F7: Support tri-state helpers, Store lifecycle, dialect seeding and
// error-classifier helpers were partly or wholly uncovered. These tests assert
// observable contract/classifier behavior.
func TestSupportStringAndJSONRoundTrip(t *testing.T) {
	if Supported.String() != "supported" || Unsupported.String() != "unsupported" || UnknownSupport.String() != "unknown" {
		t.Fatal("tri-state strings wrong")
	}
	for _, s := range []Support{Supported, Unsupported, UnknownSupport} {
		b, err := s.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var back Support
		if err := back.UnmarshalJSON(b); err != nil || back != s {
			t.Fatalf("round trip failed for %v: %s %v", s, b, back)
		}
	}
	// Legacy boolean forms stay accepted.
	var b Support
	if err := b.UnmarshalJSON([]byte("true")); err != nil || b != Supported {
		t.Fatalf("true should map to supported: %v %v", b, err)
	}
	if err := b.UnmarshalJSON([]byte("false")); err != nil || b != Unsupported {
		t.Fatalf("false should map to unsupported: %v %v", b, err)
	}
	if err := b.UnmarshalJSON([]byte(`"weird"`)); err != nil || b != UnknownSupport {
		t.Fatalf("unknown token should map to unknown: %v %v", b, err)
	}
	var viaJSON ModelCapabilities
	if err := json.Unmarshal([]byte(`{"temperature":"supported"}`), &viaJSON); err != nil || viaJSON.Temperature != Supported {
		t.Fatalf("struct unmarshal failed: %+v %v", viaJSON, err)
	}
	orig := ModelCapabilities{Temperature: Supported, Tools: Unsupported}
	if clone := orig.Clone(); clone != orig {
		t.Fatal("Clone must preserve the matrix")
	}
}

func TestStoreLifecycleDropResetSnapshot(t *testing.T) {
	s := NewStore()
	s.Seed("dep-a", ModelCapabilities{Temperature: Supported}, SourceProbe, "seed", "k1")
	s.Seed("dep-b", ModelCapabilities{Tools: Supported}, SourceProbe, "seed", "k1")
	if s.Count() != 2 {
		t.Fatalf("count=%d", s.Count())
	}
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot=%d", len(snap))
	}
	s.SetRepair("dep-a", "dropped temperature")
	s.SetIssue("dep-a", "temperature rejected once")
	got := s.Get("dep-a")
	if got.LastRepair != "dropped temperature" || got.LastCompatibilityIssue != "temperature rejected once" {
		t.Fatalf("repair/issue not recorded: %+v", got)
	}
	s.Drop("dep-a")
	if s.Count() != 1 {
		t.Fatal("Drop should remove one contract")
	}
	if _, ok := s.Get("dep-a").Evidence[CapTemperature]; ok {
		t.Fatal("dropped contract should be gone")
	}
	s.Reset()
	if s.Count() != 0 || len(s.Snapshot()) != 0 {
		t.Fatal("Reset should clear all contracts")
	}
}

func TestStoreLearnNumericAndProtocol(t *testing.T) {
	s := NewStore()
	s.LearnNumeric("dep", 0, 0) // no-op guard
	if s.Count() != 0 {
		t.Fatal("zero numeric facts must not create a contract")
	}
	s.LearnNumeric("dep", 8192, 1024)
	got := s.Get("dep")
	if got.Capabilities.ContextWindow != 8192 || got.Capabilities.MaxOutput != 1024 {
		t.Fatalf("numeric facts not learned: %+v", got.Capabilities)
	}
	s.LearnProtocol("dep2", "") // empty protocol is a no-op
	if s.Count() != 1 {
		t.Fatal("empty protocol must not create a contract")
	}
	s.LearnProtocol("dep2", "openai_chat")
	if s.Get("dep2").Capabilities.NativeProtocol != "openai_chat" {
		t.Fatal("protocol not learned")
	}
}

func TestStoreIneligibleForMatrix(t *testing.T) {
	s := NewStore()
	s.Seed("dep", ModelCapabilities{
		Tools: Unsupported, Vision: Unsupported, Streaming: Supported,
		Reasoning: Unsupported, JSONObject: Unsupported, JSONSchema: Unsupported,
	}, SourceProbe, "seed", "k1")
	cases := []struct {
		name string
		req  RequirementProfile
		want string
	}{
		{"tools", RequirementProfile{Tools: true}, CapTools},
		{"vision", RequirementProfile{Vision: true}, CapVision},
		{"streaming-supported", RequirementProfile{Streaming: true}, ""},
		{"reasoning", RequirementProfile{Reasoning: true}, CapReasoning},
		{"structured", RequirementProfile{StructuredOutput: true}, CapStructuredOutput},
		{"eligible", RequirementProfile{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap, inelig := s.IneligibleFor("dep", tc.req)
			if cap != tc.want || inelig != (tc.want != "") {
				t.Fatalf("got %q,%v want %q", cap, inelig, tc.want)
			}
		})
	}
}

func TestSeedFromDialectAndAliasParameter(t *testing.T) {
	d := Dialects()[DialectOpenAI]
	static := ModelCapabilities{Temperature: Unsupported}
	seeded := SeedFromDialect(d, static)
	if seeded.Temperature != Unsupported {
		t.Fatal("static config must override dialect priors")
	}
	if seeded.Tools != Supported {
		t.Fatal("dialect priors should seed tools")
	}
	if seeded.NativeProtocol != "openai_chat" {
		t.Fatalf("native protocol=%q", seeded.NativeProtocol)
	}
	plain := DialectProfile{Name: "x", ParameterAliases: map[string]string{"max_completion_tokens": "max_tokens"}}
	if got := plain.AliasParameter("max_completion_tokens"); got != "max_tokens" {
		t.Fatalf("alias not applied: %q", got)
	}
	if got := plain.AliasParameter("temperature"); got != "temperature" {
		t.Fatalf("unmapped name must pass through: %q", got)
	}
	if got := (DialectProfile{Name: "y"}).AliasParameter("temperature"); got != "temperature" {
		t.Fatalf("nil alias map must pass through: %q", got)
	}
}

func TestDetectDialectFingerprints(t *testing.T) {
	cases := map[string]string{
		"https://integrate.api.nvidia.com/v1":          DialectNvidiaNIM,
		"https://api.deepseek.com/v1":                  DialectDeepSeek,
		"https://openrouter.ai/api/v1":                 DialectOpenRouter,
		"https://api.groq.com/openai/v1":               DialectGroq,
		"https://api.together.xyz/v1":                  DialectTogether,
		"https://generativelanguage.googleapis.com/v1": DialectGemini,
		"https://api.openai.com/v1":                    DialectOpenAI,
		"https://api.anthropic.com/v1":                 DialectAnthropic,
	}
	for url, want := range cases {
		if got := DetectDialect("p", "openai_compatible", url, ""); got.Name != want {
			t.Fatalf("%s -> %q want %q", url, got.Name, want)
		}
	}
	if got := DetectDialect("p", "openai_compatible", "https://example.com", DialectGroq); got.Name != DialectGroq {
		t.Fatal("explicit dialect must win")
	}
	if got := DetectDialect("p", "anthropic_compatible", "https://example.com", ""); got.Name != DialectGenericAnthro {
		t.Fatalf("provider-type fallback wrong: %q", got.Name)
	}
	if got := DetectDialect("p", "gemini", "https://example.com", ""); got.Name != DialectGemini {
		t.Fatalf("gemini fallback wrong: %q", got.Name)
	}
	if got := DetectDialect("p", "openai_responses", "https://example.com", ""); got.Name != DialectOpenAIResponse {
		t.Fatalf("responses fallback wrong: %q", got.Name)
	}
	if got := DetectDialect("p", "other", "https://example.com", ""); got.Name != DialectGenericOpenAI {
		t.Fatalf("default fallback wrong: %q", got.Name)
	}
}

func TestClassifyTransportAndMalformed(t *testing.T) {
	if got := ClassifyTransportError(nil); got.Class != ClassNetworkError || !got.RetryableSamePayload {
		t.Fatalf("nil transport: %+v", got)
	}
	if got := ClassifyTransportError(errors.New("context deadline exceeded")); got.Class != ClassTimeout {
		t.Fatalf("deadline: %+v", got)
	}
	canceled := ClassifyTransportError(errors.New("context canceled"))
	if canceled.Class != ClassNetworkError || canceled.RetryableSamePayload {
		t.Fatalf("canceled must not be retried with same payload: %+v", canceled)
	}
	if got := ClassifyTransportError(errors.New("connection refused by peer")); got.Class != ClassNetworkError {
		t.Fatalf("refused: %+v", got)
	}
	if got := ClassifyTransportError(errors.New("weird local failure")); got.Class != ClassNetworkError {
		t.Fatalf("default transport class: %+v", got)
	}
	if got := ClassifyMalformedResponse("bad body"); got.Class != ClassMalformedResponse || !got.RetryableSamePayload {
		t.Fatalf("malformed: %+v", got)
	}
	if got := ClassifyStreamProtocolError("bad SSE frame"); got.Class != ClassStreamProtocolError || !got.RetryableSamePayload {
		t.Fatalf("stream protocol: %+v", got)
	}
}

func TestHTTPStatusAndCapabilityLabel(t *testing.T) {
	if got := (Classified{Class: ClassContextOverflow}).HTTPStatus(); got != 400 {
		t.Fatalf("overflow=%d", got)
	}
	if got := (Classified{Class: ClassRateLimit}).HTTPStatus(); got != 429 {
		t.Fatalf("rate limit=%d", got)
	}
	if got := (Classified{Class: ClassUpstreamOverload}).HTTPStatus(); got != 503 {
		t.Fatalf("overload=%d", got)
	}
	if got := (Classified{Class: ClassUnknown, StatusCode: 418}).HTTPStatus(); got != 418 {
		t.Fatalf("status passthrough=%d", got)
	}
	if got := (Classified{Class: ClassUnknown}).HTTPStatus(); got != 502 {
		t.Fatalf("default=%d", got)
	}
	if got := (Classified{Class: ClassUnknown}).CapabilityLabel(); got != string(ClassUnknown) {
		t.Fatalf("bare label=%q", got)
	}
	if got := (Classified{Class: ClassUnsupportedParameter, Capability: CapTemperature, Parameter: "temperature"}).CapabilityLabel(); got != CapTemperature {
		t.Fatalf("matching label=%q", got)
	}
	if got := (Classified{Class: ClassUnsupportedParameter, Capability: CapTemperature, Parameter: "temp_alias"}).CapabilityLabel(); got != CapTemperature+"/temp_alias" {
		t.Fatalf("split label=%q", got)
	}
	if got := (Classified{Class: ClassUnsupportedParameter, Parameter: "temperature"}).CapabilityLabel(); got != "temperature" {
		t.Fatalf("parameter-only label=%q", got)
	}
}

func TestParseContextTokens(t *testing.T) {
	// NOTE: "8192 tokens" currently scales by 1000 because the "k" check
	// matches the 'k' in "tokens" (known quirk, documented in
	// docs/reports/audit-top15-issue57.md; not changed by this coverage PR).
	limit, req := ParseContextTokens("maximum context length is 128k tokens, however you requested 200k tokens")
	if limit != 128000 || req != 200000 {
		t.Fatalf("got %d,%d", limit, req)
	}
	if limit, req := ParseContextTokens("no numbers here"); limit != 0 || req != 0 {
		t.Fatalf("empty should yield zeros: %d,%d", limit, req)
	}
}

func TestProbeHelperBehavior(t *testing.T) {
	if got := mapsToCapability("temperature"); got != CapTemperature {
		t.Fatalf("temperature maps to %q", got)
	}
	if got := mapsToCapability(""); got != "" {
		t.Fatalf("empty maps to %q", got)
	}
	if got := mapsToCapability("definitely-not-a-parameter"); got != "" {
		t.Fatalf("unknown maps to %q", got)
	}
	if got := DescribePlan(RepairPlan{Rules: []RepairRule{{Name: "drop_temperature"}, {Name: "clamp_tokens"}}}); got != "drop_temperature,clamp_tokens" {
		t.Fatalf("plan=%q", got)
	}
	if got := DescribePlan(RepairPlan{}); got != "" {
		t.Fatalf("empty plan=%q", got)
	}
	if got := truncatedSnippet(strings.Repeat("x", 200), 120); len(got) != 123 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long snippet not truncated: len=%d", len(got))
	}
	if got := truncatedSnippet("short", 120); got != "short" {
		t.Fatalf("short snippet=%q", got)
	}
	// Error-body extraction is observable through the classifier: a JSON body
	// naming an unsupported parameter must surface that parameter.
	cls := ClassifyUpstreamError(400, []byte(`{"error":{"message":"unsupported parameter: temperature is not supported","param":"temperature"}}`))
	if cls.Parameter == "" {
		t.Fatalf("parameter not extracted: %+v", cls)
	}
	quota := ClassifyUpstreamError(402, []byte(`{"error":{"message":"quota exceeded for this billing account, model gpt-4"}}`))
	if quota.Class != ClassQuotaExhausted {
		t.Fatalf("quota not detected: %+v", quota)
	}
}
