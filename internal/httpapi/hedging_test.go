package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

func hedgeTestResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestHedgePrimaryWinnerDoesNotWaitForLoserShutdown(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	secondaryCanceled := make(chan struct{})
	start := time.Now()
	out := s.hedgedUpstreamDo(
		ctx,
		"req-primary-wins",
		"primary",
		"secondary",
		5*time.Millisecond,
		func(context.Context) (*http.Response, error) {
			time.Sleep(25 * time.Millisecond)
			return hedgeTestResponse("primary"), nil
		},
		func(ctx context.Context) (*http.Response, error) {
			<-ctx.Done()
			close(secondaryCanceled)
			return nil, ctx.Err()
		},
	)
	elapsed := time.Since(start)

	if out.err != nil {
		t.Fatalf("unexpected hedge error: %v", out.err)
	}
	if out.secondaryWon || !out.hedgeLaunched {
		t.Fatalf("unexpected hedge outcome: %+v", out)
	}
	if out.resp == nil {
		t.Fatal("primary winner response is nil")
	}
	defer out.resp.Body.Close()
	if elapsed > 150*time.Millisecond {
		t.Fatalf("primary winner was blocked behind loser shutdown: %s", elapsed)
	}
	select {
	case <-secondaryCanceled:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("secondary loser was not cancelled promptly")
	}
}

func TestHedgeSecondaryWinnerCancelsPrimaryImmediately(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	primaryCanceled := make(chan struct{})
	out := s.hedgedUpstreamDo(
		ctx,
		"req-secondary-wins",
		"primary",
		"secondary",
		5*time.Millisecond,
		func(ctx context.Context) (*http.Response, error) {
			<-ctx.Done()
			close(primaryCanceled)
			return nil, ctx.Err()
		},
		func(context.Context) (*http.Response, error) {
			time.Sleep(20 * time.Millisecond)
			return hedgeTestResponse("secondary"), nil
		},
	)

	if out.err != nil {
		t.Fatalf("unexpected hedge error: %v", out.err)
	}
	if !out.secondaryWon || !out.hedgeLaunched {
		t.Fatalf("secondary should have won: %+v", out)
	}
	if out.resp == nil {
		t.Fatal("secondary winner response is nil")
	}
	defer out.resp.Body.Close()
	select {
	case <-primaryCanceled:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("primary loser was not cancelled promptly")
	}
}

func TestHedgeBothFailuresReturnsPrimaryErrorDeterministically(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	primaryErr := context.DeadlineExceeded
	secondaryErr := context.Canceled
	out := s.hedgedUpstreamDo(
		ctx,
		"req-both-fail",
		"primary",
		"secondary",
		time.Millisecond,
		func(context.Context) (*http.Response, error) {
			time.Sleep(15 * time.Millisecond)
			return nil, primaryErr
		},
		func(context.Context) (*http.Response, error) {
			time.Sleep(5 * time.Millisecond)
			return nil, secondaryErr
		},
	)
	if out.err != primaryErr {
		t.Fatalf("both failures should report primary error: got %v want %v", out.err, primaryErr)
	}
	if !out.hedgeLaunched {
		t.Fatal("expected hedge to launch")
	}
}

func hedgeBudgetProvider(id, baseURL string, priority int) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: "openai_compatible", BaseURL: baseURL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{
			ID: "m", Model: id + "-model", Aliases: []string{"client"},
			Enabled: true, Priority: priority, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true},
		}},
	}
}

func TestFastPrimaryFailureDoesNotSkipUnlaunchedHedgeCandidate(t *testing.T) {
	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"fast failure","type":"server_error"}}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer second.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 150
	cfg.Routing.RetryBackoffMS = 1
	cfg.Providers = []config.ProviderConfig{
		hedgeBudgetProvider("first", first.URL, 0),
		hedgeBudgetProvider("second", second.URL, 1),
	}
	srv := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unlaunched hedge candidate was skipped: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls first=%d second=%d want 1,1", firstCalls.Load(), secondCalls.Load())
	}
}

func TestLaunchedHedgeCountsAgainstMaxAttemptsEvenWhenPrimaryWins(t *testing.T) {
	var primaryCalls, hedgeCalls, thirdCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		time.Sleep(35 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"primary failed","type":"server_error"}}`)
	}))
	defer primary.Close()
	hedge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hedgeCalls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
			// Test teardown must stay bounded even if a platform transport
			// delays propagating client cancellation to the server context.
		}
	}))
	defer hedge.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"third","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"should not run"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer third.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 5
	cfg.Routing.RetryBackoffMS = 1
	cfg.Providers = []config.ProviderConfig{
		hedgeBudgetProvider("primary", primary.URL, 0),
		hedgeBudgetProvider("hedge", hedge.URL, 1),
		hedgeBudgetProvider("third", third.URL, 2),
	}
	srv := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if primaryCalls.Load() != 1 || hedgeCalls.Load() != 1 {
		t.Fatalf("hedge was not actually launched: primary=%d hedge=%d", primaryCalls.Load(), hedgeCalls.Load())
	}
	if thirdCalls.Load() != 0 {
		t.Fatalf("max_attempts=2 was exceeded; third provider called %d time(s)", thirdCalls.Load())
	}
	if rr.Code == http.StatusOK {
		t.Fatalf("third provider appears to have been used despite exhausted attempt budget: %s", rr.Body.String())
	}
}
