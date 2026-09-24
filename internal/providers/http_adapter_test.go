package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestConfiguredCredentialOverridesStaleCustomAuthHeader(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "real-key", AuthMode: "bearer", Headers: map[string]string{"Authorization": "Bearer stale-key"}, Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer real-key" {
		t.Fatalf("configured credential lost precedence: %q", gotAuth)
	}
}

func TestCustomAuthorizationAllowedWhenAuthModeNone(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL + "/v1", AuthMode: "none", Headers: map[string]string{"Authorization": "Custom abc"}, Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Custom abc" {
		t.Fatalf("custom auth unexpectedly replaced: %q", gotAuth)
	}
}

func TestEndpointAllowsAbsoluteOverride(t *testing.T) {
	got := endpoint("https://api.example.com/v1", "https://catalog.example.com/models")
	if got != "https://catalog.example.com/models" {
		t.Fatalf("absolute endpoint=%q", got)
	}
}

func TestCredentialP2CPrefersLessActiveKey(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{{Key: "a"}, {Key: "b"}}}
	first, _, ok := a.reserveCredential(nil)
	if !ok {
		t.Fatal("first credential was not selected")
	}
	second, _, ok := a.reserveCredential(nil)
	if !ok {
		t.Fatal("second credential was not selected")
	}
	if first == second {
		t.Fatalf("power-of-two key selection reused busy key %d", first)
	}
	a.releaseCredential(first)
	a.releaseCredential(second)
}

func TestCredentialP2CSkipsCoolingKey(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{
		{Key: "cooling", CooldownUntil: time.Now().Add(time.Hour)},
		{Key: "ready"},
	}}
	idx, key, ok := a.reserveCredential(nil)
	if !ok || idx != 1 || key != "ready" {
		t.Fatalf("selected idx=%d key=%q ok=%v", idx, key, ok)
	}
	a.releaseCredential(idx)
}

type terminalErrorBody struct{}

func (terminalErrorBody) Read([]byte) (int, error) { return 0, errors.New("terminal read failure") }
func (terminalErrorBody) Close() error             { return nil }

func TestReleaseOnDoneBodyReleasesOnTerminalReadError(t *testing.T) {
	released := 0
	body := &releaseOnDoneBody{
		ReadCloser: terminalErrorBody{},
		release: func() {
			released++
		},
	}
	if _, err := body.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected terminal read error")
	}
	if released != 1 {
		t.Fatalf("release count=%d want 1", released)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("release ran more than once: %d", released)
	}
}

func TestProbeRequiresProtocolValidSuccessEnvelope(t *testing.T) {
	tests := []struct {
		name         string
		providerType string
		body         string
		wantErr      bool
	}{
		{"malformed json", "openai_compatible", "{bad", true},
		{"wrong openai envelope", "openai_compatible", `{"ok":true}`, true},
		{"valid openai", "openai_compatible", `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{}}`, false},
		{"null anthropic content", "anthropic_compatible", `{"id":"x","type":"message","role":"assistant","content":null,"model":"m"}`, true},
		{"valid anthropic", "anthropic_compatible", `{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := config.ProviderConfig{
				ID: "p", Name: "P", Type: tc.providerType, BaseURL: srv.URL,
				ChatPath: "/", MessagesPath: "/", MaxConcurrency: 1, Enabled: true,
			}
			a, err := newHTTPAdapter(p, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = a.Probe(context.Background(), "m", 1)
			if tc.wantErr && err == nil {
				t.Fatal("expected invalid probe response to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid probe failed: %v", err)
			}
		})
	}
}

func TestProbeRejectsTruncatedSuccessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL,
		ChatPath: "/", MaxConcurrency: 1, Enabled: true,
	}
	a, err := newHTTPAdapter(p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Probe(context.Background(), "m", 1); err == nil {
		t.Fatal("truncated 2xx probe body must not mark a deployment healthy")
	}
}

func TestProbeRejectsNullOrNonObjectOpenAIMessage(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"message":null}]}`,
		`{"choices":[{"message":"not-an-object"}]}`,
		`{"choices":[{"message":123}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			p := config.ProviderConfig{
				ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL,
				ChatPath: "/", MaxConcurrency: 1, Enabled: true,
			}
			a, err := newHTTPAdapter(p, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.Probe(context.Background(), "m", 1); err == nil {
				t.Fatalf("invalid OpenAI message envelope was accepted: %s", body)
			}
		})
	}
}

func TestParseRetryAfterBounded(t *testing.T) {
	capDelay := 60 * time.Second
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"normal seconds", "5", 5 * time.Second},
		{"huge seconds", "31536000", capDelay},
		{"overflow integer", "999999999999999999999999999", capDelay},
		{"empty fallback", "", capDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfterBounded(tc.value, capDelay, capDelay); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfterBounded(future, capDelay, capDelay); got != capDelay {
		t.Fatalf("future HTTP-date was not capped: %s", got)
	}
}

func TestCredential429CooldownUsesConfiguredRetryAfterCap(t *testing.T) {
	p := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		MaxConcurrency: 1, Enabled: true, Credentials: []config.CredentialConfig{{Name: "k", APIKey: "secret", Enabled: true}},
	}
	a, err := newHTTPAdapterWithRetryCap(p, time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	a.cooldownCredential(0, http.StatusTooManyRequests, "3600")
	a.credMu.RLock()
	until := a.creds[0].CooldownUntil
	a.credMu.RUnlock()
	if d := until.Sub(before); d < time.Second || d > 3*time.Second {
		t.Fatalf("credential cooldown ignored cap: %s", d)
	}
}
func TestAdapterObservesCommonRateLimitHeaders(t *testing.T) {
	reset := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.Header().Set("anthropic-ratelimit-tokens-limit", "5000")
		w.Header().Set("x-ratelimit-remaining-tokens", "1234")
		w.Header().Set("x-ratelimit-reset-requests", "30s")
		w.Header().Set("anthropic-ratelimit-tokens-reset", reset)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", ChatPath: "/", MaxConcurrency: 1, Enabled: true}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"m","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	st := a.Stats()
	if st.RequestLimit != 100 || st.RemainingRequests != 0 || st.TokenLimit != 5000 || st.RemainingTokens != 1234 {
		t.Fatalf("unexpected quota stats: %+v", st)
	}
	if st.RequestResetUnix <= time.Now().Unix() || st.TokenResetUnix <= time.Now().Unix() || st.RateLimitResetUnix <= time.Now().Unix() {
		t.Fatalf("resource-specific resets were not captured: %+v", st)
	}
}
func TestQuotaReservationReducesEffectiveHeadroomUntilBodyDone(t *testing.T) {
	started := make(chan struct{})
	releaseHeaders := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-releaseHeaders
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	p := config.ProviderConfig{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", ChatPath: "/", MaxConcurrency: 4, Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a.requestLimit.Store(10)
	a.remainingRequests.Store(5)
	a.tokenLimit.Store(1000)
	a.remainingTokens.Store(500)
	a.requestResetUnix.Store(time.Now().Add(time.Minute).Unix())
	a.tokenResetUnix.Store(time.Now().Add(time.Minute).Unix())

	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	ctx := WithQuotaEstimate(context.Background(), 100, 50)
	go func() {
		resp, err := a.Do(ctx, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), false, nil)
		resultCh <- result{resp: resp, err: err}
	}()

	<-started
	st := a.Stats()
	if st.ReservedRequests != 1 || st.ReservedTokens != 150 {
		t.Fatalf("reservation not visible while request is in flight: %+v", st)
	}
	if st.EffectiveRemainingRequests != 4 || st.EffectiveRemainingTokens != 350 {
		t.Fatalf("effective headroom did not subtract reservation: %+v", st)
	}

	close(releaseHeaders)
	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	// No fresh remaining-* headers were returned, so the reservation must stay
	// in place while the response body is still owned by the caller.
	st = a.Stats()
	if st.ReservedRequests != 1 || st.ReservedTokens != 150 {
		t.Fatalf("reservation released before body completion without fresh quota headers: %+v", st)
	}
	if err := got.resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	st = a.Stats()
	if st.ReservedRequests != 0 || st.ReservedTokens != 0 || st.EffectiveRemainingRequests != 5 || st.EffectiveRemainingTokens != 500 {
		t.Fatalf("reservation leaked after body close: %+v", st)
	}
}

func TestFreshQuotaHeaderReleasesOnlyObservedResourceReservation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining-requests", "4")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	p := config.ProviderConfig{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", ChatPath: "/", MaxConcurrency: 2, Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a.requestLimit.Store(10)
	a.remainingRequests.Store(5)
	a.tokenLimit.Store(1000)
	a.remainingTokens.Store(500)

	ctx := WithQuotaEstimate(context.Background(), 100, 50)
	resp, err := a.Do(ctx, []byte(`{"model":"m","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := a.Stats()
	if st.ReservedRequests != 0 || st.RemainingRequests != 4 || st.EffectiveRemainingRequests != 4 {
		t.Fatalf("fresh request quota header did not replace local request reservation: %+v", st)
	}
	if st.ReservedTokens != 150 || st.EffectiveRemainingTokens != 350 {
		t.Fatalf("token reservation should remain until body completion without a fresh token header: %+v", st)
	}
	_ = resp.Body.Close()
	if st = a.Stats(); st.ReservedTokens != 0 || st.EffectiveRemainingTokens != 500 {
		t.Fatalf("token reservation leaked after body close: %+v", st)
	}
}

func TestQuotaReservationRequiresTaggedDataPlaneContext(t *testing.T) {
	a := &httpAdapter{}
	if r := a.reserveQuota(context.Background()); r != nil {
		t.Fatal("untagged probe/admin context unexpectedly reserved provider quota")
	}
	if st := a.Stats(); st.ReservedRequests != 0 || st.ReservedTokens != 0 {
		t.Fatalf("untagged context polluted quota stats: %+v", st)
	}
}

func TestQuotaEstimateContextBoundsAndSumsTokens(t *testing.T) {
	ctx := WithQuotaEstimate(context.Background(), 120, 30)
	got, ok := quotaEstimateFromContext(ctx)
	if !ok || got != 150 {
		t.Fatalf("quota estimate=%d ok=%v want 150,true", got, ok)
	}
	ctx = WithQuotaEstimate(context.Background(), -10, 30)
	got, ok = quotaEstimateFromContext(ctx)
	if !ok || got != 30 {
		t.Fatalf("negative estimate handling=%d ok=%v", got, ok)
	}
}

func TestReviewRedirectDoesNotLeakProviderKey(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var leaked bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			leaked = r.Header.Get("x-api-key") != ""
			w.WriteHeader(200)
		}))
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		p := config.ProviderConfig{ID: "p", Type: "anthropic_compatible", BaseURL: source.URL, APIKey: "private-key", AuthMode: "x-api-key", MessagesPath: "/v1/messages", Enabled: true}
		a, err := newHTTPAdapter(p, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := a.Do(context.Background(), []byte(`{}`), stream, nil)
		if resp != nil {
			resp.Body.Close()
		}
		target.Close()
		source.Close()
		if leaked {
			t.Fatalf("stream=%v cross-origin redirect leaked key (err=%v)", stream, err)
		}
	}
}

func TestReviewSameOriginRedirectWorks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("x-api-key") != "private-key" || r.Method != "POST" {
			t.Error("lost request semantics")
		}
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	a, err := newHTTPAdapter(config.ProviderConfig{ID: "p", Type: "anthropic_compatible", BaseURL: up.URL, APIKey: "private-key", AuthMode: "x-api-key", MessagesPath: "/start", Enabled: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
