package compat

import (
	"encoding/json"
	"errors"
	"testing"
)

// Audit item 7: compat was at 51.6% — store lifecycle and error-message
// helpers had zero coverage.
func TestAuditSupportStringJSON(t *testing.T) {
	if Supported.String() != "supported" || Unsupported.String() != "unsupported" || UnknownSupport.String() != "unknown" {
		t.Fatal("Support.String mismatch")
	}
	for _, s := range []Support{Supported, Unsupported, UnknownSupport} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back Support
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back != s {
			t.Fatalf("round trip %v -> %s -> %v", s, b, back)
		}
	}
	var legacy Support
	if err := json.Unmarshal([]byte(`true`), &legacy); err != nil || legacy != Supported {
		t.Fatalf("legacy bool true = %v, %v", legacy, err)
	}
	if err := json.Unmarshal([]byte(`"fail"`), &legacy); err != nil || legacy != Unsupported {
		t.Fatalf("legacy fail = %v, %v", legacy, err)
	}
}

func TestAuditModelCapabilitiesClone(t *testing.T) {
	m := ModelCapabilities{}
	c := m.Clone()
	_ = c
}

func TestAuditStoreLifecycle(t *testing.T) {
	s := NewStore()
	if s.Count() != 0 {
		t.Fatalf("count = %d, want 0", s.Count())
	}
	s.Seed("d1", ModelCapabilities{}, "test", "seed", "k1")
	if s.Count() != 1 {
		t.Fatalf("count = %d, want 1", s.Count())
	}
	if len(s.Snapshot()) != 1 {
		t.Fatal("snapshot should have 1 contract")
	}
	s.LearnNumeric("d1", 128000, 4096)
	s.LearnProtocol("d1", "openai")
	s.SetRepair("d1", "strip temperature")
	s.SetIssue("d1", "flaky")
	s.LearnSuccess("d1", CapText, "probe", "ok", "k1")
	s.LearnUnsupported("d1", CapTemperature, "probe", "rejected", "k1")
	_ = s.Get("d1")
	if s.Count() != 1 {
		t.Fatalf("count = %d, want 1", s.Count())
	}
	s.Drop("d1")
	if s.Count() != 0 {
		t.Fatalf("count after drop = %d", s.Count())
	}
	s.Seed("d2", ModelCapabilities{}, "test", "seed", "k1")
	s.Reset()
	if s.Count() != 0 {
		t.Fatalf("count after reset = %d", s.Count())
	}
}

func TestAuditTransportErrors(t *testing.T) {
	if c := ClassifyTransportError(nil); c.Class != ClassNetworkError {
		t.Fatalf("nil = %q", c.Class)
	}
	for msg, want := range map[string]ErrorClass{
		"dial: context deadline exceeded": ClassTimeout,
		"read: connection reset by peer":  ClassNetworkError,
		"operation: context canceled":     ClassNetworkError,
		"tls: TLS handshake timeout":      ClassNetworkError,
		"something entirely different":    ClassNetworkError,
	} {
		if c := ClassifyTransportError(errors.New(msg)); c.Class != want {
			t.Fatalf("%q = %q, want %q", msg, c.Class, want)
		}
	}
	if c := ClassifyTransportError(errors.New("context canceled")); c.RetryableSamePayload {
		t.Fatal("canceled must not be retryable with same payload")
	}
	if c := ClassifyMalformedResponse("bad frame"); c.Class != ClassMalformedResponse {
		t.Fatalf("malformed = %q", c.Class)
	}
	if c := ClassifyStreamProtocolError("missing done"); c.Class != ClassStreamProtocolError {
		t.Fatalf("stream = %q", c.Class)
	}
}

func TestAuditMessageHelpers(t *testing.T) {
	body := []byte(`{"error":{"message":"quota exceeded for requests","type":"rate_limit"}}`)
	if got := messageFromBody(body); got == "" {
		t.Fatal("messageFromBody should extract message")
	}
	if !mentionsQuota("Rate limit reached: quota exhausted") {
		t.Fatal("mentionsQuota should match quota language")
	}
	if mentionsQuota("all is well") {
		t.Fatal("mentionsQuota false positive")
	}
}

func TestAuditClassifiedHelpers(t *testing.T) {
	if got := (Classified{Class: ClassContextOverflow}).HTTPStatus(); got != 400 {
		t.Fatalf("overflow status = %d", got)
	}
	if got := (Classified{Class: ClassRateLimit}).HTTPStatus(); got != 429 {
		t.Fatalf("ratelimit status = %d", got)
	}
	if got := (Classified{Class: ClassNetworkError, StatusCode: 502}).HTTPStatus(); got != 502 {
		t.Fatalf("passthrough status = %d", got)
	}
	if got := (Classified{Class: ClassNetworkError}).HTTPStatus(); got != 502 {
		t.Fatalf("default status = %d", got)
	}
	limit, req := ParseContextTokens("maximum context length is 8192 tokens, however you requested 9000 tokens")
	if limit != 8192 || req != 9000 {
		t.Fatalf("tokens = %d, %d", limit, req)
	}
	limit, _ = ParseContextTokens("maximum context length is 128k tokens")
	if limit != 128000 {
		t.Fatalf("k-suffix tokens = %d, want 128000", limit)
	}
	if got := (Classified{Class: ClassUnsupportedParameter, Capability: CapTemperature, Parameter: "temperature"}).CapabilityLabel(); got != CapTemperature {
		t.Fatalf("label = %q", got)
	}
	if got := (Classified{Class: ClassNetworkError}).CapabilityLabel(); got != string(ClassNetworkError) {
		t.Fatalf("label = %q", got)
	}
}

func TestAuditDialectAndRepairHelpers(t *testing.T) {
	if got := mapsToCapability("temperature"); got == "" {
		t.Fatal("temperature should map to a capability")
	}
	if got := mapsToCapability(""); got != "" {
		t.Fatal("empty should map to empty")
	}
	d := DialectProfile{ParameterAliases: map[string]string{"maxTokens": "max_tokens"}}
	if got := d.AliasParameter("maxTokens"); got != "max_tokens" {
		t.Fatalf("alias = %q", got)
	}
	if got := d.AliasParameter("other"); got != "other" {
		t.Fatalf("passthrough = %q", got)
	}
	seeded := SeedFromDialect(DialectProfile{}, ModelCapabilities{})
	_ = seeded
	if got := DescribePlan(RepairPlan{Rules: []RepairRule{{Name: "strip-a"}, {Name: "strip-b"}}}); got != "strip-a,strip-b" {
		t.Fatalf("plan = %q", got)
	}
}
