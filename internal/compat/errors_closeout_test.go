package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestProviderErrorBodyParsingAndQuotaSignals(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error":{"message":"nested provider detail"}}`, "nested provider detail"},
		{`{"error":"plain provider string"}`, "plain provider string"},
		{`{"message":"top-level detail"}`, "top-level detail"},
		{"not-json", "not-json"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := messageFromBody([]byte(tc.body)); got != tc.want {
			t.Errorf("messageFromBody(%q)=%q want %q", tc.body, got, tc.want)
		}
	}
	for _, msg := range []string{"quota exhausted", "billing balance is low", "payment required"} {
		if !mentionsQuota(msg) {
			t.Errorf("quota phrase not recognized: %q", msg)
		}
	}
	if mentionsQuota("authentication failed") {
		t.Fatal("authentication error was misclassified as quota")
	}
	if got := structuredParamFromBody([]byte(`{"error":{"param":"request.temperature"}}`)); got != "request.temperature" {
		t.Fatalf("structured parameter=%q", got)
	}
	if got := structuredParamFromBody([]byte(`{"field":"top_p"}`)); got != "top_p" {
		t.Fatalf("top-level field=%q", got)
	}
	if got := structuredParamFromBody([]byte("invalid")); got != "" {
		t.Fatalf("invalid body parameter=%q", got)
	}
}

func TestClassifyUpstreamFailuresAndTheirPolicies(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		class  ErrorClass
	}{
		{"invalid key", 401, `{"error":{"message":"invalid API key"}}`, ClassInvalidKey},
		{"quota", 403, `{"message":"quota exceeded"}`, ClassQuotaExhausted},
		{"rate limit", 429, ``, ClassRateLimit},
		{"context overflow", 400, `{"message":"maximum context length is 8192 tokens, requested 9000"}`, ClassContextOverflow},
		{"unsupported parameter", 400, `{"error":{"param":"temperature","message":"unsupported parameter temperature"}}`, ClassUnsupportedParameter},
		{"unsupported vision", 400, `{"message":"image input not supported"}`, ClassUnsupportedVision},
		{"unsupported tools", 400, `{"message":"tool calling is not supported"}`, ClassUnsupportedTools},
		{"invalid schema", 422, `{"message":"failed to decode request schema"}`, ClassInvalidRequestSchema},
		{"model missing", 404, `{"message":"model not found"}`, ClassModelNotFound},
		{"endpoint missing", 404, `{"message":"unknown URL path"}`, ClassEndpointNotFound},
		{"overload", 503, ``, ClassUpstreamOverload},
		{"internal", 500, ``, ClassUpstreamInternal},
		{"unknown", 418, ``, ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyUpstreamError(tc.status, []byte(tc.body))
			if got.Class != tc.class {
				t.Fatalf("class=%s want=%s (%+v)", got.Class, tc.class, got)
			}
			policy := got.Policy()
			if policy.ErrorType == "" {
				t.Fatal("policy has empty error type")
			}
			if tc.class == ClassInvalidRequestSchema && (policy.Failover || !got.CallerError) {
				t.Fatalf("caller error should not fail over: %+v %+v", got, policy)
			}
			if tc.class == ClassRateLimit && (!policy.Failover || !policy.HardCooldown || got.HTTPStatus() != http.StatusTooManyRequests) {
				t.Fatalf("rate-limit policy/status mismatch: %+v %+v", got, policy)
			}
			if tc.class == ClassContextOverflow && got.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("context overflow status=%d", got.HTTPStatus())
			}
		})
	}
	capability := Classified{Class: ClassUnsupportedParameter, CapabilityFailure: true}
	if policy := capability.Policy(); policy.Failover || !policy.Repairable {
		t.Fatalf("capability rejection should be repaired without health penalty: %+v", policy)
	}
	if got := (Classified{Class: ClassUnknown}).HTTPStatus(); got != http.StatusBadGateway {
		t.Fatalf("unknown status=%d", got)
	}
	if got := (Classified{Class: ClassUpstreamInternal, StatusCode: 502}).HTTPStatus(); got != 502 {
		t.Fatalf("preserved status=%d", got)
	}
}

func TestTransportClassifiersContextTokenParsingAndCapabilityLabels(t *testing.T) {
	if got := ClassifyTransportError(nil); got.Class != ClassNetworkError || !got.RetryableSamePayload {
		t.Fatalf("nil transport error=%+v", got)
	}
	if got := ClassifyTransportError(errors.New("context deadline exceeded")); got.Class != ClassTimeout {
		t.Fatalf("deadline classification=%+v", got)
	}
	if got := ClassifyTransportError(errors.New("context canceled")); got.Class != ClassNetworkError || got.RetryableSamePayload {
		t.Fatalf("cancellation classification=%+v", got)
	}
	if got := ClassifyMalformedResponse(strings.Repeat("x", 600)); got.Class != ClassMalformedResponse || len(got.Message) != 512 {
		t.Fatalf("malformed response bounds/class=%+v", got)
	}
	if got := ClassifyStreamProtocolError("bad terminal frame"); got.Class != ClassStreamProtocolError || !got.RetryableSamePayload {
		t.Fatalf("stream classification=%+v", got)
	}
	limit, requested := ParseContextTokens("maximum context length is 80k tokens; requested 90000 tokens")
	if limit != 80000 || requested != 90000 {
		t.Fatalf("context token parse=(%d,%d)", limit, requested)
	}
	if limit, requested := ParseContextTokens("no token limits mentioned"); limit != 0 || requested != 0 {
		t.Fatalf("unexpected token parse=(%d,%d)", limit, requested)
	}
	for _, tc := range []struct {
		c    Classified
		want string
	}{
		{Classified{Class: ClassUnknown}, string(ClassUnknown)},
		{Classified{Capability: "tools", Parameter: "tools"}, "tools"},
		{Classified{Capability: "vision", Parameter: "image_url"}, "vision/image_url"},
		{Classified{Parameter: "temperature"}, "temperature"},
	} {
		if got := tc.c.CapabilityLabel(); got != tc.want {
			t.Errorf("CapabilityLabel=%q want %q", got, tc.want)
		}
	}
}

func TestCapabilitySetAndGetCoverDeclaredMatrixAndRejectUnknownNames(t *testing.T) {
	var got ModelCapabilities
	for _, name := range AllCapabilities {
		got.Set(name, Supported)
		if value := got.Get(name); value != Supported {
			t.Errorf("%s was not stored: %s", name, value)
		}
	}
	got.Set("untrusted_capability", Unsupported)
	if value := got.Get("untrusted_capability"); value != UnknownSupport {
		t.Fatalf("unknown capability was injected: %v", value)
	}
	if got.Clone() != got {
		t.Fatal("value capability clone differed from original")
	}
}
