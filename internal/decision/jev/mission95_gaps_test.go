package jev

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func TestMission95Buckets(t *testing.T) {
	latCases := []struct {
		ms, want string
		succ     int64
		val      float64
	}{
		{"unknown-zero-success", "unknown", 0, 10},
		{"unknown-nonpositive", "unknown", 1, 0},
		{"unknown-nan", "unknown", 1, math.NaN()},
		{"unknown-inf", "unknown", 1, math.Inf(1)},
		{"low", "low", 1, 49.9},
		{"medium-at-50", "medium", 1, 50},
		{"medium", "medium", 1, 199.9},
		{"high-at-200", "high", 1, 200},
	}
	for _, tc := range latCases {
		if got := latencyBucket(tc.val, tc.succ); got != tc.want {
			t.Errorf("latencyBucket(%v,%d) = %q, want %q", tc.val, tc.succ, got, tc.want)
		}
	}
	costCases := []struct {
		want  string
		val   float64
		known bool
	}{
		{"unknown", 0.001, false},
		{"unknown", math.NaN(), true},
		{"unknown", math.Inf(1), true},
		{"unknown", -1, true},
		{"low", 0.0009, true},
		{"medium", 0.001, true},
		{"medium", 0.0099, true},
		{"high", 0.01, true},
	}
	for _, tc := range costCases {
		if got := costBucket(tc.val, tc.known); got != tc.want {
			t.Errorf("costBucket(%v,%v) = %q, want %q", tc.val, tc.known, got, tc.want)
		}
	}
	relCases := []struct {
		want string
		s, f int64
		rate float64
	}{
		{"unknown", 0, 0, 0},
		{"unknown", 1, 1, math.NaN()},
		{"unknown", 1, 1, math.Inf(1)},
		{"higher", 9, 1, 0.09},
		{"medium", 9, 1, 0.1},
		{"medium", 9, 1, 0.29},
		{"lower", 9, 1, 0.3},
	}
	for _, tc := range relCases {
		if got := reliabilityBucket(tc.s, tc.f, tc.rate); got != tc.want {
			t.Errorf("reliabilityBucket(%d,%d,%v) = %q, want %q", tc.s, tc.f, tc.rate, got, tc.want)
		}
	}
	headCases := []struct {
		want    string
		window  int
		require int
	}{
		{"unknown", 0, 10},
		{"unknown", 100, 0},
		{"low", 50, 100},
		{"high", 100, 10},
		{"medium", 100, 50},
		{"low", 100, 90},
		{"low", 100, 100},
	}
	for _, tc := range headCases {
		if got := contextHeadroomBucket(tc.window, tc.require); got != tc.want {
			t.Errorf("contextHeadroomBucket(%d,%d) = %q, want %q", tc.window, tc.require, got, tc.want)
		}
	}
}

func TestMission95BuildTaskSummaryComplexityDerivation(t *testing.T) {
	req := decision.DecisionRequest{
		TaskProfile: taskprofile.TaskProfile{Type: "coding"},
		Features:    feature.RequestFeatures{EstimatedTotalTokens: 15000},
	}
	if got := BuildTaskSummary(req); !strings.Contains(got, "complexity=high") {
		t.Fatalf("high tokens: %q", got)
	}
	req.Features.EstimatedTotalTokens = 5000
	if got := BuildTaskSummary(req); !strings.Contains(got, "complexity=medium") {
		t.Fatalf("medium tokens: %q", got)
	}
	req.Features.EstimatedTotalTokens = 100
	if got := BuildTaskSummary(req); !strings.Contains(got, "complexity=low") {
		t.Fatalf("low tokens: %q", got)
	}
	// Explicit complexity wins over derivation.
	req.TaskProfile.Complexity = "high"
	req.Features.EstimatedTotalTokens = 1
	if got := BuildTaskSummary(req); !strings.Contains(got, "complexity=high") {
		t.Fatalf("explicit: %q", got)
	}
	// Candidate with no capabilities reports caps=none and all buckets.
	c := decision.Candidate{ID: "p1/m1", ContextWindow: 1000}
	desc := BuildCandidateDescription(c, 100)
	if !strings.Contains(desc, "caps=none") || !strings.Contains(desc, "latency=unknown") {
		t.Fatalf("no-cap desc: %q", desc)
	}
	// BuildJevRequest skips candidates missing from the mapping, then
	// validation rejects the too-small candidate set.
	mapping := BuildOpaqueMapping([]decision.Candidate{{ID: "p1/m1"}})
	_, err := BuildJevRequest(decision.DecisionRequest{
		Candidates: []decision.Candidate{{ID: "p1/m1"}, {ID: "p2/m2"}},
	}, mapping)
	if err == nil {
		t.Fatalf("expected validation error for unmapped candidate set")
	}
	// Required window falls back to estimated tokens when MinContextWindow is 0.
	jr, err := BuildJevRequest(decision.DecisionRequest{
		EstimatedInputTokens: 800,
		MaxOutputTokens:      200,
		Candidates:           []decision.Candidate{{ID: "p1/m1", ContextWindow: 5000}, {ID: "p2/m2", ContextWindow: 5000}},
	}, BuildOpaqueMapping([]decision.Candidate{{ID: "p1/m1"}, {ID: "p2/m2"}}))
	if err != nil {
		t.Fatalf("fallback request: %v", err)
	}
	if len(jr.Candidates) != 2 || jr.Candidates[0].ID != "c0" {
		t.Fatalf("sorted candidates: %+v", jr.Candidates)
	}
}

func TestMission95ParseAndValidateBranches(t *testing.T) {
	allowed := map[string]struct{}{"c0": {}, "c1": {}}
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"invalid-json", "{nope"},
		{"nonzero-code", `{"code": 7, "data": {"decision": "c0"}}`},
		{"missing-data", `{"code": 0}`},
		{"invalid-data-json", `{"code": 0, "data": "str"}`},
		{"missing-decision", `{"code": 0, "data": {}}`},
		{"unknown-candidate", `{"code": 0, "data": {"decision": "c9"}}`},
		{"confidence-oob", `{"code": 0, "data": {"decision": "c0", "confidence": 1.5}}`},
		{"confidence-negative", `{"code": 0, "data": {"decision": "c0", "confidence": -0.1}}`},
		{"unknown-in-probs", `{"code": 0, "data": {"decision": "c0", "probabilities": {"c9": 0.5}}}`},
		{"prob-oob", `{"code": 0, "data": {"decision": "c0", "probabilities": {"c0": 2}}}`},
		{"prob-negative", `{"code": 0, "data": {"decision": "c0", "probabilities": {"c0": -0.5}}}`},
	}
	for _, tc := range cases {
		_, err := ParseAndValidate([]byte(tc.body), allowed)
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
	// Unknown candidate is a distinct typed error.
	_, err := ParseAndValidate([]byte(`{"code": 0, "data": {"decision": "c9"}}`), allowed)
	if !remote.IsUnknownCandidate(err) {
		t.Fatalf("unknown candidate not typed: %v", err)
	}
	// Too many probabilities: 101 entries over allowed IDs.
	bigAllowed := map[string]struct{}{}
	probs := map[string]float64{}
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("c%d", i)
		bigAllowed[id] = struct{}{}
		probs[id] = 0.5
	}
	// Marshal manually to keep the test deterministic.
	probJSON := `{"code": 0, "data": {"decision": "c0", "probabilities": {`
	parts := []string{}
	for k, v := range probs {
		parts = append(parts, fmt.Sprintf("%q:%v", k, v))
	}
	// Sort-free: order does not matter for validation outcome.
	probJSON += strings.Join(parts, ",") + "}}}"
	if _, err := ParseAndValidate([]byte(probJSON), bigAllowed); err == nil {
		t.Fatalf("expected too-many-probabilities error")
	}
	// Success: absent confidence defaults to 0, guidance ignored.
	parsed, err := ParseAndValidate([]byte(`{"code": 0, "data": {"decision": "c1", "guidance": "pick c1"}}`), allowed)
	if err != nil {
		t.Fatalf("success: %v", err)
	}
	if parsed.SelectedID != "c1" || parsed.Confidence != 0 {
		t.Fatalf("parsed = %+v", parsed)
	}
	parsed, err = ParseAndValidate([]byte(`{"code": 0, "data": {"decision": "c0", "confidence": 0.75, "probabilities": {"c0": 0.75, "c1": 0.25}}}`), allowed)
	if err != nil || parsed.Confidence != 0.75 {
		t.Fatalf("with confidence: %+v %v", parsed, err)
	}
}

func TestMission95ProviderConstructionBranches(t *testing.T) {
	for _, cfg := range []ProviderConfig{
		{Type: "jev"},
		{ID: "x", Type: "other"},
		{ID: "x", Type: "jev", PrivacyMode: "full"},
	} {
		if _, err := NewProvider(cfg); err == nil {
			t.Fatalf("config %+v should fail", cfg)
		}
	}
	t.Setenv("MISSION95_JEV_KEY", "env-key-123")
	p, err := NewProvider(ProviderConfig{ID: "jev-main", Type: "jev", Enabled: true, APIKeyEnv: "MISSION95_JEV_KEY"})
	if err != nil {
		t.Fatalf("env key: %v", err)
	}
	if !p.HasAPIKey() {
		t.Fatalf("env key should be configured")
	}
	// Explicit key wins when the env var is unset.
	p2, err := NewProvider(ProviderConfig{ID: "jev-main", Type: "jev", Enabled: true, APIKey: "direct-key", APIKeyEnv: "MISSION95_JEV_KEY_UNSET_XYZ"})
	if err != nil {
		t.Fatalf("direct key: %v", err)
	}
	if !p2.HasAPIKey() {
		t.Fatalf("direct key should be configured")
	}
	// BaseURL default and custom.
	if got := p.BaseURL(); got != defaultBaseURL {
		t.Fatalf("default BaseURL = %q", got)
	}
	p3, _ := NewProvider(ProviderConfig{ID: "j", Type: "jev", BaseURL: "https://example.com"})
	if got := p3.BaseURL(); got != "https://example.com" {
		t.Fatalf("custom BaseURL = %q", got)
	}
	// String() must not leak the key.
	s := p2.String()
	if strings.Contains(s, "direct-key") {
		t.Fatalf("String leaks key: %q", s)
	}
	if !strings.Contains(s, "jev-main") || !strings.Contains(s, "enabled=true") || !strings.Contains(s, "key_configured=true") {
		t.Fatalf("String = %q", s)
	}
	// isRemoteError seams.
	var target *remote.Error
	if isRemoteError(nil, &target) {
		t.Fatalf("nil error should not be remote")
	}
	if isRemoteError(errors.New("plain"), &target) {
		t.Fatalf("plain error should not be remote")
	}
	if !isRemoteError(remote.NewError(remote.ErrTimeout, "t"), &target) || target == nil {
		t.Fatalf("remote error should be detected")
	}
	// isLoopbackBaseURL branches.
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://[::1]:8080", true},
		{"https://127.0.0.1:8080", false},
		{"http://example.com", false},
		{"http://169.254.169.254", false},
		{"://bad", false},
		{"http://", false},
	} {
		if got := isLoopbackBaseURL(tc.raw); got != tc.want {
			t.Errorf("isLoopbackBaseURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	// UpdateFromConfig: bad base URL fails and leaves the provider intact.
	p4, _ := NewProvider(ProviderConfig{ID: "j", Type: "jev", Enabled: true, APIKey: "k1"})
	if err := p4.UpdateFromConfig("k2", "https://user:pw@example.com", true); err == nil {
		t.Fatalf("bad update should fail")
	}
	if p4.HasAPIKey() && p4.BaseURL() == "https://user:pw@example.com" {
		t.Fatalf("failed update must not take effect")
	}
	if err := p4.UpdateFromConfig("k2", "https://example.com", false); err != nil {
		t.Fatalf("good update: %v", err)
	}
	if p4.BaseURL() != "https://example.com" {
		t.Fatalf("updated BaseURL = %q", p4.BaseURL())
	}
}

func mission95JevCandidates() []decision.Candidate {
	return []decision.Candidate{
		{ID: "p1/m1", ContextWindow: 8000, Successes: 10, EWMALatencyMS: 30, PriceKnown: true, EstimatedCostUSD: 0.0005},
		{ID: "p2/m2", ContextWindow: 8000, Successes: 10, EWMALatencyMS: 40, PriceKnown: true, EstimatedCostUSD: 0.0006},
	}
}

func mission95JevProvider(t *testing.T, handler http.Handler) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := NewProvider(ProviderConfig{
		ID: "jev-main", Type: "jev", Enabled: true, APIKey: "test-key",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return p
}

func TestMission95ProviderDecidePaths(t *testing.T) {
	ctx := context.Background()
	two := mission95JevCandidates()

	// Cancelled context abstains with the context error.
	p := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called for cancelled ctx")
	}))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	res, err := p.Decide(canceled, decision.DecisionRequest{Candidates: two})
	if err == nil || !res.Abstained {
		t.Fatalf("cancelled: res=%+v err=%v", res, err)
	}

	// Disabled provider abstains without calling upstream.
	pd, _ := NewProvider(ProviderConfig{ID: "j", Type: "jev", Enabled: false, APIKey: "k"})
	res, err = pd.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained || res.Action != decision.ActionAbstain {
		t.Fatalf("disabled: %+v %v", res, err)
	}

	// Fewer than two candidates abstains.
	pe := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called for <2 candidates")
	}))
	res, err = pe.Decide(ctx, decision.DecisionRequest{Candidates: two[:1]})
	if err != nil || !res.Abstained {
		t.Fatalf("single candidate: %+v %v", res, err)
	}

	// Tampered privacy mode abstains (same-package field access).
	pt := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called for bad privacy mode")
	}))
	pt.privacyMode = "full"
	res, err = pt.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained {
		t.Fatalf("bad privacy: %+v %v", res, err)
	}

	// Success path maps the opaque ID back to the physical ID.
	ps := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code": 0, "data": {"decision": "c1", "confidence": 0.8}}`)
	}))
	res, err = ps.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || res.Action != decision.ActionSelect || res.SelectedID != "p2/m2" || res.Confidence != 0.8 {
		t.Fatalf("success: %+v %v", res, err)
	}
	// Unknown candidate in the envelope abstains with the unknown code.
	pu := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code": 0, "data": {"decision": "c9"}}`)
	}))
	res, err = pu.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained || res.ReasonCodes[0] != decision.ReasonExternalUnknownCandidate {
		t.Fatalf("unknown: %+v %v", res, err)
	}
	// Garbage body abstains as invalid response.
	pg := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	res, err = pg.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained || res.ReasonCodes[0] != decision.ReasonExternalInvalidResponse {
		t.Fatalf("garbage: %+v %v", res, err)
	}
	// HTTP 500 abstains as HTTP error.
	ph := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"code": 1}`)
	}))
	res, err = ph.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained || res.ReasonCodes[0] != decision.ReasonExternalHTTPError {
		t.Fatalf("http500: %+v %v", res, err)
	}
	// Redirect is never followed and abstains as invalid response.
	pr := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.com/elsewhere")
		w.WriteHeader(302)
	}))
	res, err = pr.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained || res.ReasonCodes[0] != decision.ReasonExternalInvalidResponse {
		t.Fatalf("redirect: %+v %v", res, err)
	}
	// Oversized response abstains as response-too-large.
	pb := mission95JevProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, remote.MaxResponseBodyBytes+16))
	}))
	res, err = pb.Decide(ctx, decision.DecisionRequest{Candidates: two})
	if err != nil || !res.Abstained || res.ReasonCodes[0] != decision.ReasonExternalResponseTooLarge {
		t.Fatalf("big body: %+v %v", res, err)
	}
}

// errTransport returns a fixed error for every request.
type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestMission95ProviderDecideTransportErrors(t *testing.T) {
	ctx := context.Background()
	two := mission95JevCandidates()
	mk := func(rt http.RoundTripper) *Provider {
		p, err := NewProvider(ProviderConfig{
			ID: "jev-main", Type: "jev", Enabled: true, APIKey: "test-key",
			BaseURL: "http://example.test", HTTPClient: &http.Client{Transport: rt},
		})
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		return p
	}
	cases := []struct {
		name string
		err  error
		want decision.ReasonCode
	}{
		{"timeout", context.DeadlineExceeded, decision.ReasonExternalTimeout},
		{"generic-unavailable", errors.New("dial refused"), decision.ReasonExternalProviderUnavailable},
		{"ssrf", fmt.Errorf("x: %w", remote.ErrSSRFBlocked), decision.ReasonExternalProviderUnavailable},
		// Note: a transport returning ErrRedirectNotAllowed / an HTTP-error
		// kind is remapped to Unavailable by remote.Client.Do, so those
		// provider branches are covered instead by the httptest 302/500
		// cases above, which is how they are reachable in production.
	}
	for _, tc := range cases {
		p := mk(errTransport{err: tc.err})
		res, err := p.Decide(ctx, decision.DecisionRequest{Candidates: two})
		if err != nil || !res.Abstained || len(res.ReasonCodes) == 0 || res.ReasonCodes[0] != tc.want {
			t.Errorf("%s: res=%+v err=%v", tc.name, res, err)
		}
	}
}
