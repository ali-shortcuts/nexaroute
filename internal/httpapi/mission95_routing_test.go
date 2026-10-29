package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

func TestMission95PolicyForStatus(t *testing.T) {
	cases := []struct {
		code     int
		kind     string
		failover bool
	}{
		{400, "caller_invalid_request", false},
		{422, "caller_invalid_request", false},
		{401, "provider_auth_failed", true},
		{403, "provider_auth_failed", true},
		{402, "provider_billing", true},
		{404, "provider_request_rejected", true},
		{408, "provider_timeout", true},
		{504, "provider_timeout", true},
		{409, "provider_transient_request", true},
		{425, "provider_transient_request", true},
		{429, "provider_rate_limited", true},
		{503, "provider_overloaded", true},
		{529, "provider_overloaded", true},
		{500, "provider_server_error", true},
		{502, "provider_server_error", true},
		{200, "_OTHER", false},
		{301, "_OTHER", false},
		{499, "_OTHER", false},
	}
	for _, tc := range cases {
		p := policyForStatus(tc.code)
		if p.ErrorType != tc.kind || p.Failover != tc.failover {
			t.Errorf("code %d: policy=%+v", tc.code, p)
		}
		if errorTypeForStatus(tc.code) != tc.kind || retryable(tc.code) != tc.failover || failoverEligible(tc.code) != tc.failover {
			t.Errorf("code %d: helper wrappers disagree", tc.code)
		}
	}
	// Auth failures quarantine + hard-cool; transient requests do not.
	if p := policyForStatus(401); !p.QuarantineDeployment || !p.SignalProvider || !p.HardCooldown {
		t.Errorf("401 policy flags = %+v", p)
	}
	if p := policyForStatus(409); p.QuarantineDeployment || p.SignalProvider || p.HardCooldown {
		t.Errorf("409 policy flags = %+v", p)
	}
}

func TestMission95GatewayDeadlineAndRouteContext(t *testing.T) {
	ctx, cancel := routeContext(context.Background(), true, time.Second)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("streaming route context carries a deadline")
	}
	ctx2, cancel2 := routeContext(context.Background(), false, time.Second)
	defer cancel2()
	if _, ok := ctx2.Deadline(); !ok {
		t.Error("bounded route context lacks a deadline")
	}
	ctx3, cancel3 := routeContext(context.Background(), false, 0)
	defer cancel3()
	if _, ok := ctx3.Deadline(); ok {
		t.Error("zero-timeout route context carries a deadline")
	}
	// Deadline exceeded on the route side only.
	routeCtx, routeCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer routeCancel()
	time.Sleep(2 * time.Millisecond)
	if !gatewayDeadlineExceeded(routeCtx, context.Background()) {
		t.Error("expired route deadline not detected")
	}
	if gatewayDeadlineExceeded(context.Background(), context.Background()) {
		t.Error("live contexts reported as deadline-exceeded")
	}
	// Caller cancellation always takes precedence over a transport timeout.
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientCancel()
	if gatewayDeadlineError(routeCtx, clientCtx, timeoutErr{"t"}) {
		t.Error("cancelled client reported as gateway deadline")
	}
	// Bounded route + timeout error = gateway deadline even before expiry.
	bounded, bcancel := context.WithTimeout(context.Background(), time.Minute)
	defer bcancel()
	if !gatewayDeadlineError(bounded, context.Background(), timeoutErr{"t"}) {
		t.Error("bounded timeout error not classified as gateway deadline")
	}
	if gatewayDeadlineError(bounded, context.Background(), context.Canceled) {
		t.Error("non-timeout error classified as gateway deadline")
	}
	if gatewayDeadlineError(context.Background(), context.Background(), timeoutErr{"t"}) {
		t.Error("unbounded context classified as gateway deadline")
	}
	if !clientRequestGone(clientCtx) || clientRequestGone(context.Background()) {
		t.Error("clientRequestGone inverted")
	}
	// Jitter bounds: deterministic per request+attempt, capped at 5s.
	for _, attempt := range []int{-1, 0, 1, 3, 6, 99} {
		got := jitteredRetryBackoff(time.Second, attempt, "req-1")
		if got < 0 || got > 5*time.Second {
			t.Errorf("attempt %d backoff = %v", attempt, got)
		}
	}
	if jitteredRetryBackoff(0, 1, "x") != 0 || jitteredRetryBackoff(-time.Second, 1, "x") != 0 {
		t.Error("non-positive base did not yield zero backoff")
	}
	if jitteredRetryBackoff(time.Second, 2, "req-1") != jitteredRetryBackoff(time.Second, 2, "req-1") {
		t.Error("backoff not deterministic")
	}
	if jitteredRetryBackoff(10*time.Second, 6, "x") > 5*time.Second {
		t.Error("backoff cap violated")
	}
}

func TestMission95RetryAfter(t *testing.T) {
	// Seconds form, capped by max.
	h := http.Header{"Retry-After": []string{"120"}}
	if got := retryAfterDuration(h, 10*time.Second); got != 10*time.Second {
		t.Errorf("capped retry-after = %v", got)
	}
	if got := retryAfterDuration(http.Header{"Retry-After": []string{"5"}}, 10*time.Second); got != 5*time.Second {
		t.Errorf("retry-after = %v", got)
	}
	// No header: fallback is 30s, or max when smaller.
	if got := retryAfterDuration(http.Header{}, 0); got != 30*time.Second {
		t.Errorf("default fallback = %v", got)
	}
	if got := retryAfterDuration(http.Header{}, 5*time.Second); got != 5*time.Second {
		t.Errorf("max-bounded fallback = %v", got)
	}
	// Garbage and non-positive values fall back.
	for _, v := range []string{"not-a-delay", "0", "-5", "999999999999999999999"} {
		if got := retryAfterDuration(http.Header{"Retry-After": []string{v}}, 10*time.Second); got != 10*time.Second {
			t.Errorf("value %q gave %v, want 10s fallback", v, got)
		}
	}
	// HTTP-date form in the future, capped by max.
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfterDuration(http.Header{"Retry-After": []string{future}}, 10*time.Second); got != 10*time.Second {
		t.Errorf("future date capped = %v", got)
	}
	if got := retryAfterDuration(http.Header{"Retry-After": []string{future}}, 0); got < 60*time.Second || got > 90*time.Second {
		t.Errorf("future date uncapped = %v", got)
	}
	// Past date falls back.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := retryAfterDuration(http.Header{"Retry-After": []string{past}}, 0); got != 30*time.Second {
		t.Errorf("past date = %v", got)
	}
	// Response value: delta-seconds only, never raw header text.
	if got := retryAfterResponseValue(http.Header{}, 0); got != "" {
		t.Errorf("empty header value = %q", got)
	}
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{"999"}}, 3*time.Second); got != "3" {
		t.Errorf("capped response value = %q", got)
	}
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{"0"}}, 0); got != "0" {
		t.Errorf("zero response value = %q", got)
	}
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{"garbage"}}, 0); got != "" {
		t.Errorf("garbage response value = %q", got)
	}
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{past}}, 0); got != "0" {
		t.Errorf("past date response value = %q", got)
	}
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{future}}, 10*time.Second); got != "10" {
		t.Errorf("future date capped response value = %q", got)
	}
	// Sub-second max still yields at least 1 second for a seconds value.
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{"5"}}, 500*time.Millisecond); got != "1" {
		t.Errorf("sub-second max response value = %q", got)
	}
	// Upstream error text is truncated at 1024 bytes.
	if got := upstreamError(502, []byte(strings.Repeat("x", 2000))); !strings.HasSuffix(got, "…") || len(got) > len("upstream 502: ")+1024+3 {
		t.Errorf("truncated upstream error len=%d", len(got))
	}
	if got := upstreamError(502, []byte("  bad gateway ")); got != "upstream 502: bad gateway" {
		t.Errorf("upstream error = %q", got)
	}
}

func TestMission95HeaderAndSessionHelpers(t *testing.T) {
	for _, k := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Admin-Key", "Cookie", "Set-Cookie", "Connection", "Proxy-Connection", "Transfer-Encoding", "Content-Length", "Host", " authorization "} {
		if !blockedClientForwardHeader(k) {
			t.Errorf("header %q not blocked", k)
		}
	}
	for _, k := range []string{"X-Trace", "Accept", "Content-Type"} {
		if blockedClientForwardHeader(k) {
			t.Errorf("header %q wrongly blocked", k)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "secret")
	r.Header.Set("X-Trace", "trace")
	r.Header["X-Multi"] = []string{"a", "b"}
	copied := copySelectedRequestHeaders(r)
	if copied.Get("Authorization") != "" || copied.Get("X-Trace") != "trace" {
		t.Fatalf("copied headers = %v", copied)
	}
	copied["X-Trace"] = []string{"mutated"}
	if r.Header.Get("X-Trace") != "trace" {
		t.Error("header copy aliases the original slice")
	}
	// Session keys: top-level wins, then metadata, then nested user_id.
	if got := bodySessionKey(map[string]any{"session_id": "top"}); got != "top" {
		t.Errorf("top session = %q", got)
	}
	if got := bodySessionKey(map[string]any{"metadata": map[string]any{"session_id": "meta"}}); got != "meta" {
		t.Errorf("meta session = %q", got)
	}
	if got := bodySessionKey(map[string]any{"metadata": map[string]any{"user_id": map[string]any{"session_id": "nested"}}}); got != "nested" {
		t.Errorf("nested session = %q", got)
	}
	if got := bodySessionKey(map[string]any{"metadata": "not-a-map"}); got != "" {
		t.Errorf("bad metadata session = %q", got)
	}
	if got := boundedSessionValue("  " + strings.Repeat("x", 300) + "  "); len(got) != 256 {
		t.Errorf("bounded session len = %d", len(got))
	}
	// Header session keys take precedence over the body key, in order.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Session-Id", "from-header")
	if got := sessionKeyFromRequestParts(req, "from-body"); got != "from-header" {
		t.Errorf("header session = %q", got)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := sessionKeyFromRequestParts(req2, "from-body"); got != "from-body" {
		t.Errorf("body fallback session = %q", got)
	}
	if got := sessionKeyFromRequest(req2, []byte(`{"session_id":"in-body"}`)); got != "in-body" {
		t.Errorf("request session from body = %q", got)
	}
	if got := sessionKeyFromRequest(req2, []byte(`not json`)); got != "" {
		t.Errorf("invalid body session = %q", got)
	}
}

func TestMission95QuotaAndProviderLoad(t *testing.T) {
	if got := quotaRemainingPressure(-1, 100); got != 0 {
		t.Errorf("negative remaining pressure = %v", got)
	}
	if got := quotaRemainingPressure(10, 0); got != 0 {
		t.Errorf("zero limit pressure = %v", got)
	}
	if got := quotaRemainingPressure(0, 100); got != 4 {
		t.Errorf("exhausted pressure = %v", got)
	}
	if got := quotaRemainingPressure(50, 100); got != 0 {
		t.Errorf("healthy pressure = %v", got)
	}
	if got := quotaRemainingPressure(25, 100); got != 0 {
		t.Errorf("25%% pressure = %v", got)
	}
	if got := quotaRemainingPressure(10, 100); got < 2 || got > 4 {
		t.Errorf("low pressure = %v", got)
	}
	now := time.Now().Unix()
	load := providerLoadFromStats(providers.ProviderStats{
		ActiveRequests: 2, WaitingRequests: 1, MaxConcurrency: 10,
		RemainingRequests: 0, RequestLimit: 100, RequestResetUnix: now + 60,
	}, now)
	if !load.QuotaExhausted || load.QuotaPressure != 4 {
		t.Fatalf("exhausted load = %+v", load)
	}
	// Effective remaining overrides the raw remaining when non-negative.
	load = providerLoadFromStats(providers.ProviderStats{
		RemainingRequests: 0, EffectiveRemainingRequests: 50, RequestLimit: 100,
		RequestResetUnix: now + 60,
	}, now)
	if load.QuotaExhausted {
		t.Errorf("effective remaining still exhausted: %+v", load)
	}
	// Shared reset deadline alone marks exhaustion for a zero remaining.
	load = providerLoadFromStats(providers.ProviderStats{
		RemainingTokens: 0, TokenLimit: 100, RateLimitResetUnix: now + 60,
	}, now)
	if !load.QuotaExhausted {
		t.Errorf("shared reset did not mark exhaustion: %+v", load)
	}
	// Token pressure beats request pressure when larger.
	load = providerLoadFromStats(providers.ProviderStats{
		RemainingRequests: 20, RequestLimit: 100, RequestResetUnix: now + 60,
		RemainingTokens: 0, TokenLimit: 100, TokenResetUnix: now + 60,
	}, now)
	if load.QuotaPressure != 4 {
		t.Errorf("token pressure = %v", load.QuotaPressure)
	}
	// No reset pending: no pressure even at low remaining.
	load = providerLoadFromStats(providers.ProviderStats{
		RemainingRequests: 5, RequestLimit: 100,
	}, now)
	if load.QuotaPressure != 0 || load.QuotaExhausted {
		t.Errorf("no-reset load = %+v", load)
	}
	_ = router.ProviderLoad{}
}

func TestMission95FirstByteAndStripStream(t *testing.T) {
	var observed []time.Duration
	rc := io.NopCloser(strings.NewReader("hello"))
	wrapped := observeFirstByte(rc, time.Now(), func(d time.Duration) { observed = append(observed, d) })
	buf := make([]byte, 2)
	if n, err := wrapped.Read(buf); err != nil || n != 2 || string(buf) != "he" {
		t.Fatalf("first read = %d, %v, %q", n, err, buf)
	}
	if _, err := io.ReadAll(wrapped); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 {
		t.Fatalf("observe calls = %d", len(observed))
	}
	// Nil observer never panics and reads through.
	wrapped2 := observeFirstByte(io.NopCloser(strings.NewReader("x")), time.Now(), nil)
	if b, err := io.ReadAll(wrapped2); err != nil || string(b) != "x" {
		t.Fatalf("nil-observer read = %q, %v", b, err)
	}
	// firstByteBody observes only on bytes, not on EOF-only reads.
	observed = nil
	empty := &firstByteBody{ReadCloser: io.NopCloser(strings.NewReader("")), start: time.Now(), observe: func(d time.Duration) { observed = append(observed, d) }}
	if _, err := empty.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("empty read err = %v", err)
	}
	if len(observed) != 0 {
		t.Error("empty read triggered observation")
	}
	// stripStreamOptions.
	stripped, changed := stripStreamOptions([]byte(`{"model":"m","stream_options":{"include_usage":true}}`))
	if !changed || strings.Contains(string(stripped), "stream_options") {
		t.Fatalf("stripped = %s, changed=%v", stripped, changed)
	}
	if _, changed := stripStreamOptions([]byte(`{"model":"m"}`)); changed {
		t.Error("absent stream_options reported as changed")
	}
	if out, changed := stripStreamOptions([]byte(`{bad`)); changed || string(out) != `{bad` {
		t.Errorf("invalid payload strip = %s, %v", out, changed)
	}
	// patchJSONModel: invalid UTF-8 model rejected, valid model replaced.
	if _, err := patchJSONModel([]byte(`{"model":"old"}`), string([]byte{0xff})); err == nil {
		t.Error("invalid UTF-8 model accepted")
	}
	if _, err := patchJSONModel([]byte(`{`), "new"); err == nil {
		t.Error("invalid JSON patch accepted")
	}
	// inspectRequestJSON: vision + reasoning + token estimate + complexity.
	insp := inspectRequestJSON([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}],"reasoning_effort":"high","session_id":"s1"}`), "image_url", []string{"reasoning_effort"})
	if !insp.Vision || !insp.Reasoning || insp.EstimatedPromptTokens == 0 || insp.BodySessionKey != "s1" {
		t.Fatalf("inspection = %+v", insp)
	}
	respInsp := inspectResponsesRequestJSON([]byte(`{"input":[{"type":"input_image"}],"reasoning":{"effort":"high"}}`))
	if !respInsp.Vision || !respInsp.Reasoning {
		t.Fatalf("responses inspection = %+v", respInsp)
	}
	// routeContext is exercised above; prepareRequirement session wiring.
	reg, err := providers.NewRegistry(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{reg: reg}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Request-Id", "rid-9")
	prepared := s.prepareRequirement(router.Requirement{}, req, "body-key")
	if prepared.SessionKey != "body-key" || prepared.SelectionKey != "rid-9" {
		t.Fatalf("prepared requirement session=%q selection=%q", prepared.SessionKey, prepared.SelectionKey)
	}
	if prepared.LoadForProvider == nil || prepared.LoadForProvider("missing") != (router.ProviderLoad{}) {
		t.Error("LoadForProvider for unknown provider misbehaved")
	}
	if prepared.LoadForProvider("missing") != prepared.LoadForProvider("missing") {
		t.Error("LoadForProvider cache inconsistent")
	}
}
