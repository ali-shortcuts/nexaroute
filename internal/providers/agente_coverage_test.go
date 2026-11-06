package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Agent E coverage: the last uncovered blocks of internal/providers
// (CI baseline, main 58576e62: 21 statements). All upstream traffic here
// is faked with httptest servers or a stub RoundTripper; nothing here is
// live-provider evidence. Fake credential values below are obviously
// fake ("test-key-*") and must never be mistaken for real secrets.

// ---------------------------------------------------------------------------
// profiles.go: AuthHeader modes, Supports, template refusal, task label.
// ---------------------------------------------------------------------------

func TestAgentEProfileAuthHeaderModes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"bearer", "Authorization"},
		{"x-api-key", "x-api-key"},
		{"x-goog-api-key", "x-goog-api-key"},
		{"api-key", "api-key"},
		{"none", ""},
		{"something-else", ""},
	} {
		p := Profile{ID: "test-auth-" + tc.mode, AuthMode: tc.mode}
		if got := p.AuthHeader(); got != tc.want {
			t.Errorf("AuthHeader() for mode %q = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestAgentEProfileSupportsCapability(t *testing.T) {
	p := Profile{
		ID:           "test-supports",
		Capabilities: []string{ProfileCapStreamingSSE, ProfileCapToolCalls},
	}
	if !p.Supports(ProfileCapToolCalls) {
		t.Errorf("Supports(%q) = false, want true (documented capability)", ProfileCapToolCalls)
	}
	if !p.Supports(ProfileCapStreamingSSE) {
		t.Errorf("Supports(%q) = false, want true (documented capability)", ProfileCapStreamingSSE)
	}
	if p.Supports(ProfileCapImageInput) {
		t.Errorf("Supports(%q) = true, want false (absent means not documented)", ProfileCapImageInput)
	}
	if (Profile{}).Supports(ProfileCapAuth) {
		t.Errorf("Supports on a profile with no capabilities = true, want false")
	}
}

func TestAgentEProfileToProviderConfigWithBaseRejectsTemplate(t *testing.T) {
	p := Profile{
		ID: "test-templated", ProviderName: "Test", Protocol: ProfileProtocolChat,
		Dialect: "openai_compatible", BaseURL: "https://example.com/{account_id}/{gateway_id}",
		AuthMode: "bearer",
	}
	if _, err := p.ToProviderConfigWithBase("https://example.com/{account_id}/{gateway_id}", "TEST_PROFILE_KEY_ENV"); err == nil {
		t.Fatal("ToProviderConfigWithBase with an unsubstituted template must fail closed")
	} else if !strings.Contains(err.Error(), "template") {
		t.Fatalf("template error = %q, want it to name the template", err.Error())
	}
	// The same profile with a substituted base configures cleanly.
	cfg, err := p.ToProviderConfigWithBase("https://example.com/acct-1/gtw-1", "TEST_PROFILE_KEY_ENV")
	if err != nil {
		t.Fatalf("substituted ToProviderConfigWithBase: %v", err)
	}
	if cfg.BaseURL != "https://example.com/acct-1/gtw-1" || cfg.APIKey != "" {
		t.Fatalf("substituted config = %+v, want substituted base and no credential value", cfg)
	}
}

func TestAgentEProfileProviderSpecificLabelIsTask(t *testing.T) {
	// No registry profile pairs Protocol "provider_specific" with a
	// dialect, so the "task" label branch needs a constructed profile.
	p := Profile{
		ID: "test-task", ProviderName: "TestVendor", Protocol: ProfileProtocolProviderSpecific,
		Dialect: "openai_compatible", BaseURL: "https://example.com",
		AuthMode: "bearer",
	}
	cfg, err := p.ToProviderConfig("TEST_PROFILE_KEY_ENV")
	if err != nil {
		t.Fatalf("ToProviderConfig for a provider-specific profile with a dialect: %v", err)
	}
	if cfg.Name != "TestVendor task" {
		t.Fatalf("config name = %q, want %q", cfg.Name, "TestVendor task")
	}
	if cfg.Type != "openai_compatible" || cfg.APIKey != "" || cfg.APIKeyEnv != "TEST_PROFILE_KEY_ENV" {
		t.Fatalf("config = %+v, want dialect openai_compatible, env reference only", cfg)
	}
}

// ---------------------------------------------------------------------------
// http_adapter.go: DoPath credential loop break (614) and its epilogue
// (745-746, 749). The loop can only break when the last available
// credential starts cooling *after* hasAvailableCredential has already
// approved a retry but *before* the next reserve — a window that in
// production only a concurrent request can open. The test opens it
// deterministically: the 429 body blocks inside ReadAll (which runs
// after the availability check), the test cools the remaining
// credential while DoPath is parked there, then releases the body.
// ---------------------------------------------------------------------------

// agentEBlockingBody blocks its first Read until unblock is closed, so
// the test can interleave credential cooling between the adapter's
// availability check and its next credential selection. The 10s valve
// keeps a failed orchestration from hanging the suite.
type agentEBlockingBody struct {
	readStarted chan struct{}
	unblock     chan struct{}
	payload     []byte
	once        sync.Once
	done        bool
}

func (b *agentEBlockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.readStarted) })
	select {
	case <-b.unblock:
	case <-time.After(10 * time.Second):
		return 0, io.EOF
	}
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	return copy(p, b.payload), nil
}

func (b *agentEBlockingBody) Close() error { return nil }

type agentERoundTripFunc func(*http.Request) (*http.Response, error)

func (f agentERoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAgentEDoPathBreaksWhenRemainingCredentialCoolsMidRetry(t *testing.T) {
	body := &agentEBlockingBody{
		readStarted: make(chan struct{}),
		unblock:     make(chan struct{}),
		payload:     []byte(`{"error":"rate limited"}`),
	}
	var calls atomic.Int32
	var gotAuth atomic.Value
	rt := agentERoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if n := calls.Add(1); n > 1 {
			// Must never happen on the break path; return a plain 200 so
			// a second attempt surfaces as a wrong result, not a hang.
			return &http.Response{
				Status: "200 OK", StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{}`)), Request: r,
			}, nil
		}
		gotAuth.Store(r.Header.Get("Authorization"))
		return &http.Response{
			Status: "429 Too Many Requests", StatusCode: http.StatusTooManyRequests,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   body, Request: r,
		}, nil
	})

	srv := mission95Serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("fake transport must intercept the request; httptest server must not be hit")
		w.WriteHeader(http.StatusTeapot)
	})
	p := config.ProviderConfig{ID: "agente-break", Type: "openai_compatible", BaseURL: srv.URL, Enabled: true}
	p.Credentials = []config.CredentialConfig{
		{APIKey: "test-key-alpha", Enabled: true},
		{APIKey: "test-key-beta", Enabled: true},
	}
	a, err := newHTTPAdapter(p, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.creds) != 2 {
		t.Fatalf("creds=%d, want 2", len(a.creds))
	}
	a.c.Transport = rt

	type outcome struct {
		resp *http.Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := a.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", []byte(`{"model":"m","messages":[]}`), false, nil)
		done <- outcome{resp, err}
	}()

	// DoPath is now parked inside ReadAll of the 429 body, i.e. past
	// hasAvailableCredential (which approved the retry) and before the
	// next reserveCredentialWithSession.
	select {
	case <-body.readStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("DoPath never reached the retry body read")
	}
	// Cool every credential while the retry is committed but unselected.
	a.cooldownCredential(0, http.StatusTooManyRequests, "")
	a.cooldownCredential(1, http.StatusTooManyRequests, "")
	close(body.unblock)

	select {
	case r := <-done:
		if r.resp != nil {
			_ = r.resp.Body.Close()
			t.Fatalf("DoPath returned a response (status %d), want nil response with the retry error", r.resp.StatusCode)
		}
		if r.err == nil {
			t.Fatal("DoPath returned nil error, want the credential-rejected error from the break path")
		}
		if !strings.Contains(r.err.Error(), "credential rejected http 429") {
			t.Fatalf("DoPath error = %q, want it to report the 429 credential rejection", r.err.Error())
		}
		if strings.Contains(r.err.Error(), "test-key-alpha") || strings.Contains(r.err.Error(), "test-key-beta") {
			t.Fatalf("DoPath error leaks a credential value: %q", r.err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DoPath did not return after the body was released")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d, want exactly 1 (the break path must not retry)", got)
	}
	auth, _ := gotAuth.Load().(string)
	if auth != "Bearer test-key-alpha" && auth != "Bearer test-key-beta" {
		t.Fatalf("upstream Authorization = %q, want one of the two fake keys", auth)
	}
}

// ---------------------------------------------------------------------------
// http_adapter.go: nextCredentialRetryAfter d<=0 guard (1307).
// The guard can only fire when a credential's CooldownUntil falls
// between the function's two time.Now() samples — a nanosecond-scale
// race in production. The probe makes the gap measurable by placing
// the target behind a long filler scan and aiming its expiry into
// that window; a miss therefore means "not found", never a wrong
// answer, and the deterministic contract is asserted separately.
// ---------------------------------------------------------------------------

func TestAgentENextCredentialRetryAfterRaceWindow(t *testing.T) {
	far := &httpAdapter{creds: []credentialState{
		{Key: "test-key-a", LastStatus: http.StatusTooManyRequests, CooldownUntil: time.Now().Add(time.Hour)},
	}}
	d, found := far.nextCredentialRetryAfter()
	if !found || d <= 0 || d > time.Hour {
		t.Fatalf("far-future 429 cooldown: found=%v d=%v, want found with 0<d<=1h", found, d)
	}
	expired := &httpAdapter{creds: []credentialState{
		{Key: "test-key-a", LastStatus: http.StatusTooManyRequests, CooldownUntil: time.Now().Add(-time.Minute)},
	}}
	if _, found := expired.nextCredentialRetryAfter(); found {
		t.Fatal("expired 429 cooldown must not report a retry-after")
	}
	other := &httpAdapter{creds: []credentialState{
		{Key: "test-key-a", LastStatus: http.StatusServiceUnavailable, CooldownUntil: time.Now().Add(time.Hour)},
	}}
	if _, found := other.nextCredentialRetryAfter(); found {
		t.Fatal("non-429 cooldown must not report a retry-after")
	}

	// Race-window probe: the target sits behind fillers that are all
	// skipped by the first guard clause, so the scan takes a measurable
	// time; aiming the target's expiry inside that scan must make the
	// d<=0 guard fire (found=false) even though the expiry was in the
	// future when the call started.
	const fillers = 200000
	creds := make([]credentialState, 0, fillers+1)
	for i := 0; i < fillers; i++ {
		creds = append(creds, credentialState{Key: "filler"})
	}
	creds = append(creds, credentialState{Key: "test-key-target", LastStatus: http.StatusTooManyRequests, CooldownUntil: time.Now().Add(24 * time.Hour)})
	a := &httpAdapter{creds: creds}
	start := time.Now()
	if _, found := a.nextCredentialRetryAfter(); !found {
		t.Fatal("calibration call must find the far-future 429 cooldown")
	}
	scan := time.Since(start)
	if scan < 10*time.Microsecond {
		t.Fatalf("scan calibration implausibly fast: %v", scan)
	}
	hit := false
	for _, frac := range []float64{0.05, 0.1, 0.2, 0.4, 0.6, 0.8} {
		for attempt := 0; attempt < 20 && !hit; attempt++ {
			a.creds[fillers].CooldownUntil = time.Now().Add(time.Duration(float64(scan) * frac))
			if _, found := a.nextCredentialRetryAfter(); !found {
				hit = true
			}
		}
		if hit {
			break
		}
	}
	t.Logf("nextCredentialRetryAfter d<=0 race probe: scan=%v hit=%v", scan, hit)
	if hit {
		a.creds[fillers].CooldownUntil = time.Now().Add(time.Hour)
		if _, found := a.nextCredentialRetryAfter(); !found {
			t.Fatal("far-future cooldown must still report after the probe")
		}
	}
}
