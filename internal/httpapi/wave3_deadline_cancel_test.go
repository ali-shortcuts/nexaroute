package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Regression tests for the Wave3 deadline/cancellation contract (PR #739):
// a genuine gateway deadline maps to 504, caller cancellation is never
// misreported as a deadline, ordinary upstream errors keep their 502, and
// a client disconnect cancels upstream work and releases the connection.

// wave3TimeoutError is a synthetic transport timeout: Timeout() reports
// true exactly like net/http's "timeout awaiting response headers".
type wave3TimeoutError struct{ msg string }

func (e wave3TimeoutError) Error() string   { return e.msg }
func (e wave3TimeoutError) Timeout() bool   { return true }
func (e wave3TimeoutError) Temporary() bool { return true }

func TestGatewayDeadlineErrorContract(t *testing.T) {
	timeoutErr := wave3TimeoutError{msg: "timeout awaiting response headers"}
	ordinaryErr := errors.New("connection refused")

	expiredRoute := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		t.Cleanup(cancel)
		time.Sleep(5 * time.Millisecond)
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatal("test setup: route context did not expire")
		}
		return ctx
	}
	cases := []struct {
		name    string
		route   func() context.Context
		client  func() context.Context
		err     error
		want504 bool
	}{
		{
			name:   "route timer expired is a deadline",
			route:  expiredRoute,
			client: func() context.Context { return context.Background() },
			err:    timeoutErr, want504: true,
		},
		{
			name:   "route timer expired with ordinary error is still a deadline",
			route:  expiredRoute,
			client: func() context.Context { return context.Background() },
			err:    ordinaryErr, want504: true,
		},
		{
			name: "transport timeout under a bounded route is a deadline",
			route: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				t.Cleanup(cancel)
				return ctx
			},
			client: func() context.Context { return context.Background() },
			err:    timeoutErr, want504: true,
		},
		{
			name: "wrapped transport timeout under a bounded route is a deadline",
			route: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				t.Cleanup(cancel)
				return ctx
			},
			client: func() context.Context { return context.Background() },
			err:    fmt.Errorf("post failed: %w", timeoutErr), want504: true,
		},
		{
			name: "ordinary error under a bounded route is not a deadline",
			route: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				t.Cleanup(cancel)
				return ctx
			},
			client: func() context.Context { return context.Background() },
			err:    ordinaryErr, want504: false,
		},
		{
			name: "nil error is not a deadline",
			route: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				t.Cleanup(cancel)
				return ctx
			},
			client: func() context.Context { return context.Background() },
			err:    nil, want504: false,
		},
		{
			name: "timeout error on an unbounded route is not a deadline",
			route: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				return ctx
			},
			client: func() context.Context { return context.Background() },
			err:    timeoutErr, want504: false,
		},
		{
			name:  "caller cancellation takes precedence over a deadline",
			route: expiredRoute,
			client: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			err: timeoutErr, want504: false,
		},
		{
			name: "caller cancellation takes precedence over a transport timeout",
			route: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				t.Cleanup(cancel)
				return ctx
			},
			client: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			err: timeoutErr, want504: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayDeadlineError(tc.route(), tc.client(), tc.err); got != tc.want504 {
				t.Fatalf("gatewayDeadlineError = %v want %v", got, tc.want504)
			}
		})
	}
}

// wave3SplitTimeoutGateway builds a gateway whose upstream adapters use
// adapterTimeoutMS while request routing uses routeTimeoutMS. Production
// builds both from the same RequestTimeoutMS; splitting them here is the
// only deterministic way to force the transport-timeout-first outcome
// (adapter timer fires while the route timer is still pending) that the
// shared-budget race produces under load. It exercises the error
// classification, not a production configuration.
func wave3SplitTimeoutGateway(t *testing.T, providerCfgs []config.ProviderConfig, adapterTimeoutMS, routeTimeoutMS int) *Server {
	t.Helper()
	routeCfg := config.Default()
	routeCfg.Probe.Enabled = false
	routeCfg.Routing.RetryBackoffMS = 0
	routeCfg.Routing.Strategy = "priority"
	routeCfg.Routing.RequestTimeoutMS = routeTimeoutMS
	routeCfg.Providers = providerCfgs
	routeCfg.ApplyDefaults()
	adapterCfg := routeCfg
	adapterCfg.Routing.RequestTimeoutMS = adapterTimeoutMS
	hm := health.New(routeCfg.Routing.FailureThreshold, routeCfg.Cooldown())
	hm.ConfigureProviderIncidents(routeCfg.Routing.ProviderFailureThreshold, routeCfg.ProviderFailureWindow(), routeCfg.ProviderCooldown())
	reg, err := providers.NewRegistry(adapterCfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(routeCfg, hm)
	bus := events.New(100)
	pe := probe.New(routeCfg, reg, rt, hm, bus)
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := New(routeCfg, configDir+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	if err := srv.SecurityStoreError(); err != nil {
		t.Fatalf("create test security store: %v", err)
	}
	t.Cleanup(func() { _ = srv.CloseSecurityStore() })
	return srv
}

func TestWave3ResponsesClientTimeoutMapsTo504(t *testing.T) {
	// Regression for the CI verify failure: the upstream transport
	// timeout ("timeout awaiting response headers") was mapped to 502
	// because only the narrow route-timer check was consulted. With the
	// adapter budget (200ms) far shorter than the route budget (60s),
	// the transport timeout deterministically arrives while the route
	// timer is still pending; the contract requires 504.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	s := wave3SplitTimeoutGateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, 200, 60000)
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	releaseUpstream()
	if rr.Code != 504 || !strings.Contains(rr.Body.String(), "gateway request timeout") {
		t.Fatalf("responses client-timeout status=%d body=%s want 504 gateway request timeout", rr.Code, rr.Body.String())
	}
}

func TestWave3GeminiClientTimeoutMapsTo504(t *testing.T) {
	// Same shared-budget defect on the Gemini ingress path.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	s := wave3SplitTimeoutGateway(t, []config.ProviderConfig{
		wave3Provider("g", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, 200, 60000)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	releaseUpstream()
	if rr.Code != 504 || !strings.Contains(rr.Body.String(), "gateway request timeout") {
		t.Fatalf("gemini client-timeout status=%d body=%s want 504 gateway request timeout", rr.Code, rr.Body.String())
	}
}

func TestWave3ResponsesBodyReadCancelReleasesUpstream(t *testing.T) {
	// Stronger than TestWave3ResponsesUsageErrorClientGone: besides the
	// empty-body assertion, this pins the cleanup contract — the
	// upstream handler must observe cancellation and the test server
	// must close promptly, so a client disconnect can never leave an
	// active connection behind to hang the package.
	entered := make(chan struct{})
	observedDone := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(observedDone)
	}))
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wave3RequestCtx(s, ctx, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream handler was never entered")
	}
	cancel()
	select {
	case rr := <-done:
		if rr.Body.Len() != 0 {
			t.Fatalf("cancelled usage-error body=%q want empty", rr.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled usage-error handler did not return within 5s")
	}
	select {
	case <-observedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream handler did not observe client cancellation within 5s")
	}
	closeDone := make(chan struct{})
	go func() {
		up.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("test server Close did not return within 5s: active connection leaked")
	}
}
