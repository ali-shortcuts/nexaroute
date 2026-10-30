package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Wave 2 (issue #663): hedging.go blocks uncovered in the 2026-10-29
// baseline profile. The race tests below use channel choreography, not
// sleeps, so each outcome is deterministic: the leg functions signal each
// other rather than racing a timer.
//
// Deliberately NOT covered: launchSecondary's already-launched guard
// (hedging.go:132-133) — launchSecondary is a closure called only from the
// one-shot timer case, so the guard cannot fire through the race loop; it
// is defensive code, not a reachable branch.

func TestWave2HedgeNormalizeNilResult(t *testing.T) {
	out := normalizeHedgeResult(hedgeResult{})
	if out.err == nil {
		t.Fatal("nil response without error must be converted to an error")
	}
	if !strings.Contains(out.err.Error(), "nil response") {
		t.Fatalf("error = %q, want it to mention the nil response", out.err.Error())
	}
}

func TestWave2HedgeAbandonNilLeg(t *testing.T) {
	s := &Server{bus: events.New(32)}
	// Must be a no-op, not a panic: callers abandon legs that never started.
	s.abandonHedgeLeg(nil, "p/m", "req-nil", "never started")
}

func TestWave2HedgeBoundedErrorNil(t *testing.T) {
	if got := boundedProtocolMappingError(nil); got != nil {
		t.Fatalf("nil error must stay nil, got %v", got)
	}
}

func TestWave2HedgeBoundedErrorTruncates(t *testing.T) {
	long := strings.Repeat("x", 600)
	got := boundedProtocolMappingError(errors.New(long))
	if got == nil {
		t.Fatal("long error must survive as a bounded error")
	}
	if !strings.HasSuffix(got.Error(), "...") {
		t.Fatalf("long error must be truncated with an ellipsis, got %q", got.Error())
	}
	if len(got.Error()) > len("unsupported protocol mapping: ")+512+3 {
		t.Fatalf("bounded error too long: %d bytes", len(got.Error()))
	}
}

func wave2HedgeGateway(t *testing.T, providers []config.ProviderConfig) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = providers
	return testGateway(t, cfg)
}

func TestWave2HedgeBuildOpenAIMissingCandidate(t *testing.T) {
	s := wave2HedgeGateway(t, []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "a-model", Enabled: true}},
	}})
	// No deployment "no/such" exists, so currentRouteCandidate fails and the
	// build reports false instead of fabricating an attempt.
	if _, ok := s.buildOpenAIAttempt(router.Scored{Deployment: router.Deployment{ID: "no/such"}},
		router.Requirement{}, nil, core.OpenAIRequest{Model: "a-model"}); ok {
		t.Fatal("build with an unknown candidate must return false")
	}
}

func TestWave2HedgeBuildOpenAIBedrockEncodeFails(t *testing.T) {
	// Dummy credentials only: the adapter is never asked to send anything
	// (the build fails at protocol encoding). The names are test-local.
	t.Setenv("WAVE2_BEDROCK_SECRET", "dummy-secret")
	s := wave2HedgeGateway(t, []config.ProviderConfig{{
		ID: "b", Name: "B", Type: "bedrock", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true, SecretKeyEnv: "WAVE2_BEDROCK_SECRET",
		Credentials: []config.CredentialConfig{{APIKey: "dummy-access-key", Enabled: true}},
		Models:      []config.ModelConfig{{ID: "m", Model: "bedrock-model", Enabled: true}},
	}})
	in := core.OpenAIRequest{Model: "bedrock-model", Messages: []core.OpenAIMessage{{
		Role: "user",
		Content: []any{map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/xyz;base64,AAAA"},
		}},
	}}}
	// Control: the same deployment with a text-only message builds fine,
	// proving the candidate itself resolves; only the image breaks the
	// build, so the failure below is the encoder, not candidate lookup.
	control := core.OpenAIRequest{Model: "bedrock-model", Messages: []core.OpenAIMessage{{
		Role: "user", Content: "hello",
	}}}
	if _, ok := s.buildOpenAIAttempt(router.Scored{Deployment: router.Deployment{ID: "b/m"}},
		router.Requirement{}, nil, control); !ok {
		t.Fatal("text-only Bedrock build must succeed (candidate control)")
	}
	// Decode accepts any data-URI media type, but the Bedrock encoder has
	// an allowlist, so finishCanonicalAttempt fails: the build reports
	// false and emits an encode_fail event (the returned bundle is empty
	// by construction, so the event is the precise evidence).
	if _, ok := s.buildOpenAIAttempt(router.Scored{Deployment: router.Deployment{ID: "b/m"}},
		router.Requirement{}, nil, in); ok {
		t.Fatal("build with an unencodable image must return false")
	}
	if !wave3BusHasKind(s, "encode_fail") {
		t.Fatal("failed Bedrock encode must emit an encode_fail event")
	}
}

func TestWave2HedgeBuildAnthropicMissingCandidate(t *testing.T) {
	s := wave2HedgeGateway(t, []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "a-model", Enabled: true}},
	}})
	if _, ok := s.buildAnthropicAttempt(router.Scored{Deployment: router.Deployment{ID: "no/such"}},
		router.Requirement{}, nil, core.AnthropicRequest{Model: "a-model"}); ok {
		t.Fatal("build with an unknown candidate must return false")
	}
}

func TestWave2HedgeBuildAnthropicDecodeFails(t *testing.T) {
	s := wave2HedgeGateway(t, []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "a-model", Enabled: true}},
	}})
	// "tool" is not a valid Anthropic message role, so Decode fails and the
	// build reports false with a bounded build error.
	in := core.AnthropicRequest{Model: "a-model", MaxTokens: 32, Messages: []core.AnthMessage{{
		Role:    "tool",
		Content: []byte(`"hi"`),
	}}}
	bundle, ok := s.buildAnthropicAttempt(router.Scored{Deployment: router.Deployment{ID: "a/m"}},
		router.Requirement{}, nil, in)
	if ok {
		t.Fatal("build with an undecodable request must return false")
	}
	if bundle.buildErr == nil {
		t.Fatal("failed decode must leave a bounded build error")
	}
}

func wave2HedgeOK(t *testing.T) *http.Response {
	t.Helper()
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}
}

func TestWave2HedgeRacePrimaryErrorNoSecondary(t *testing.T) {
	s := &Server{bus: events.New(32)}
	primaryErr := errors.New("primary transport boom")
	secondaryCalled := false
	out := s.hedgedUpstreamDo(context.Background(), "req-p-err", "p/m", "q/m",
		10*time.Second,
		func(context.Context) (*http.Response, error) { return nil, primaryErr },
		func(context.Context) (*http.Response, error) {
			secondaryCalled = true
			return wave2HedgeOK(t), nil
		})
	if out.err != primaryErr {
		t.Fatalf("err = %v, want the primary error", out.err)
	}
	if out.hedgeLaunched || secondaryCalled {
		t.Fatal("a fast primary failure must return before the hedge timer fires")
	}
}

func TestWave2HedgeRaceSecondaryWinsAfterPrimaryError(t *testing.T) {
	s := &Server{bus: events.New(32)}
	secondaryDone := make(chan struct{})
	out := s.hedgedUpstreamDo(context.Background(), "req-s-wins", "p/m", "q/m",
		time.Millisecond,
		func(context.Context) (*http.Response, error) {
			// The primary may only fail once the hedge has already won,
			// so the loop takes the secondary-won branch.
			<-secondaryDone
			return nil, errors.New("primary late failure")
		},
		func(context.Context) (*http.Response, error) {
			resp := wave2HedgeOK(t)
			close(secondaryDone)
			return resp, nil
		})
	if out.err != nil || out.resp == nil || out.resp.StatusCode != 200 {
		t.Fatalf("secondary winner: err = %v resp = %v", out.err, out.resp)
	}
	if !out.secondaryWon || !out.hedgeLaunched {
		t.Fatalf("outcome = %+v, want secondaryWon + hedgeLaunched", out)
	}
}

func TestWave2HedgeRaceSecondaryFailsAfterPrimaryError(t *testing.T) {
	s := &Server{bus: events.New(32)}
	secondaryStarted := make(chan struct{})
	primaryErred := make(chan struct{})
	primaryErr := errors.New("primary first failure")
	out := s.hedgedUpstreamDo(context.Background(), "req-both-fail-rev", "p/m", "q/m",
		time.Millisecond,
		func(context.Context) (*http.Response, error) {
			// Fail only once the hedge leg exists, so the loop records
			// the primary failure first and keeps waiting for the hedge.
			<-secondaryStarted
			close(primaryErred)
			return nil, primaryErr
		},
		func(context.Context) (*http.Response, error) {
			close(secondaryStarted)
			// Fail only after the primary has failed, so the loop takes
			// the "primary already failed" branch on the hedge result.
			<-primaryErred
			return nil, errors.New("secondary late failure")
		})
	if out.err != primaryErr {
		t.Fatalf("err = %v, want the primary error deterministically", out.err)
	}
	if !out.hedgeLaunched {
		t.Fatal("hedge must have launched")
	}
}

func TestWave2HedgeRaceContextCancelNoSecondary(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	block := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := s.hedgedUpstreamDo(ctx, "req-ctx", "p/m", "q/m", 10*time.Second, block, block)
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", out.err)
	}
	if out.hedgeLaunched {
		t.Fatal("cancel before the timer must not launch the hedge")
	}
}

func TestWave2HedgeRaceContextCancelWithSecondary(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithCancel(context.Background())
	block := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := s.hedgedUpstreamDo(ctx, "req-ctx-hedge", "p/m", "q/m",
		time.Millisecond,
		block,
		// The hedge leg itself cancels the route context, so the loop
		// abandons both legs through the cancellation branch.
		func(ctx context.Context) (*http.Response, error) {
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		})
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", out.err)
	}
	if !out.hedgeLaunched {
		t.Fatal("hedge must have launched before the cancellation")
	}
}

func TestWave2HedgeDoAttemptSkipsCanonical(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	partnerHits := 0
	partnerUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		partnerHits++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"partner":true}`)
	}))
	defer partnerUp.Close()
	s := wave2HedgeGateway(t, []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "a-model", Enabled: true}},
	}, {
		ID: "b", Name: "B", Type: "openai_compatible", BaseURL: partnerUp.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "b-model", Enabled: true}},
	}})
	adapter, ok := s.reg.Get("a")
	if !ok {
		t.Fatal("test gateway must expose the provider adapter")
	}
	partnerAdapter, ok := s.reg.Get("b")
	if !ok {
		t.Fatal("test gateway must expose the partner adapter")
	}
	cfg := config.Default()
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 50
	cfg.Routing.MaxAttempts = 3
	buildCalled := false
	primary := hedgeAttemptBundle{
		c:             router.Scored{Deployment: router.Deployment{ID: "a/m", ProviderID: "a", ProviderType: "openai_compatible", Model: "a-model"}},
		a:             adapter,
		canonicalKind: "canonical-x",
	}
	partner := hedgeAttemptBundle{
		c: router.Scored{Deployment: router.Deployment{ID: "b/m", ProviderID: "b", ProviderType: "openai_compatible", Model: "b-model"}},
		a: partnerAdapter,
	}
	candidates := []router.Scored{primary.c, partner.c}
	out, _, hedged := s.doAttemptWithHedge(context.Background(), "req-canon-skip",
		cfg, candidates, 0, 1, 3, primary,
		func(int) (hedgeAttemptBundle, bool) {
			// The partner IS built before the canonical check: the
			// skip discards it, it must never be dispatched.
			buildCalled = true
			return partner, true
		}, false, nil)
	if !buildCalled {
		t.Fatal("partner must be built before the canonical check runs")
	}
	if hedged {
		t.Fatal("canonical attempt must run as a plain attempt")
	}
	if partnerHits != 0 {
		t.Fatalf("partner upstream hit %d times, want 0 (built but never dispatched)", partnerHits)
	}
	if out.resp == nil || out.resp.StatusCode != 200 {
		t.Fatalf("plain attempt response = %v, want the primary upstream 200", out.resp)
	}
	if out.resp != nil && out.resp.Body != nil {
		_ = out.resp.Body.Close()
	}
}
