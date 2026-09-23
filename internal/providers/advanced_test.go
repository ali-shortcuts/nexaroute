package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestCredentialPoolFailsOverAndCoolsBadKey(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, k)
		mu.Unlock()
		if k == "Bearer bad" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"bad"}`)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "bad", Credentials: []config.CredentialConfig{{Name: "good", APIKey: "good", Enabled: true}}, AuthMode: "bearer", Enabled: true, MaxConcurrency: 4}
	a, err := newHTTPAdapter(p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 got %d", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "Bearer bad" || got[1] != "Bearer good" {
		t.Fatalf("unexpected credential order: %#v", got)
	}
}

func TestCredentialFailoverReturnsFinalTransportErrorNotClosedPriorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer bad":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad"}`)
		case "Bearer drop":
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		default:
			t.Errorf("unexpected credential %q", r.Header.Get("Authorization"))
		}
	}))
	defer srv.Close()

	p := config.ProviderConfig{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL,
		APIKey: "bad", Credentials: []config.CredentialConfig{{Name: "drop", APIKey: "drop", Enabled: true}},
		AuthMode: "bearer", Enabled: true, MaxConcurrency: 2,
	}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("expected no response after final transport error, got status %d", resp.StatusCode)
	}
	if err == nil {
		t.Fatal("expected final transport error")
	}
}

func TestSafeSnippetConcurrentWithCredentialStateChanges(t *testing.T) {
	p := config.ProviderConfig{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: "http://example.invalid",
		APIKey:      "secret-a",
		Credentials: []config.CredentialConfig{{Name: "b", APIKey: "secret-b", Enabled: true}},
		AuthMode:    "bearer", Enabled: true,
	}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			got := a.safeSnippet([]byte("secret-a secret-b provider error"))
			if strings.Contains(got, "secret-a") || strings.Contains(got, "secret-b") {
				t.Errorf("credential leaked from safeSnippet: %q", got)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			a.cooldownCredential(i%2, http.StatusUnauthorized, "")
			a.markCredentialSuccess(i % 2)
		}
	}()
	wg.Wait()
}

func TestForwardHeaderAllowlistDoesNotLeakClientAuth(t *testing.T) {
	var beta, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		auth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "upstream", AuthMode: "bearer", ForwardHeaders: []string{"anthropic-beta", "authorization"}, Enabled: true}
	a, _ := newHTTPAdapter(p, 2*time.Second)
	h := http.Header{"anthropic-beta": []string{"feature-x"}, "Authorization": []string{"Bearer client-secret"}}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, h)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if beta != "feature-x" {
		t.Fatalf("beta not forwarded: %q", beta)
	}
	if auth != "Bearer upstream" {
		t.Fatalf("client auth leaked/overrode upstream auth: %q", auth)
	}
}

func TestProviderConcurrencyLimit(t *testing.T) {
	var active, maxActive int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&active, 1)
		for {
			m := atomic.LoadInt32(&maxActive)
			if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 1}
	a, _ := newHTTPAdapter(p, 2*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("max concurrency=%d want 1", maxActive)
	}
}

func TestProviderConcurrencyHeldUntilResponseBodyClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 1}
	a, err := newHTTPAdapter(p, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	first, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
		if err != nil {
			errCh <- err
			return
		}
		secondCh <- resp
	}()

	select {
	case resp := <-secondCh:
		resp.Body.Close()
		first.Body.Close()
		t.Fatal("second request passed provider concurrency gate before first body was closed")
	case err := <-errCh:
		first.Body.Close()
		t.Fatalf("second request failed unexpectedly: %v", err)
	case <-time.After(80 * time.Millisecond):
		// expected: second request is blocked by the in-flight body
	}

	first.Body.Close()
	select {
	case resp := <-secondCh:
		resp.Body.Close()
	case err := <-errCh:
		t.Fatalf("second request failed after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second request did not proceed after first response body closed")
	}
}

func TestAllCoolingCredentialsAreNotReused(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad credential"}`)
	}))
	defer srv.Close()

	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "only-key", AuthMode: "bearer", Enabled: true, MaxConcurrency: 2}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("first call count=%d want 1", calls.Load())
	}

	_, err = a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("expected cooling-down error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cooling credential was reused; call count=%d", calls.Load())
	}
}

func TestAllRateLimitedCredentialsExposeRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "key", AuthMode: "bearer", Enabled: true}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	_, err = a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err == nil {
		t.Fatal("expected rate-limit cooldown error")
	}
	d, ok := RetryAfter(err)
	if !ok {
		t.Fatalf("expected RetryAfterError, got %T %v", err, err)
	}
	if d <= 0 || d > 1100*time.Millisecond {
		t.Fatalf("retry-after=%s want about 1s", d)
	}
}

func TestRedactBodyCoversAdapterSnapshotAndRotatedEnvCredential(t *testing.T) {
	const envName = "NEXAROUTE_TEST_REDACT_ROTATION"
	old, had := os.LookupEnv(envName)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(envName, old)
		} else {
			_ = os.Unsetenv(envName)
		}
	})
	if err := os.Setenv(envName, "old-secret"); err != nil {
		t.Fatal(err)
	}
	p := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		APIKeyEnv: envName, AuthMode: "bearer", Enabled: true, MaxConcurrency: 1,
	}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv(envName, "new-secret"); err != nil {
		t.Fatal(err)
	}
	got := string(a.RedactBody([]byte("old-secret new-secret public")))
	if strings.Contains(got, "old-secret") || strings.Contains(got, "new-secret") {
		t.Fatalf("credential leaked after env rotation: %q", got)
	}
	if !strings.Contains(got, "public") {
		t.Fatalf("non-secret content was unexpectedly removed: %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorDoesNotFanOutAcrossCredentialPool(t *testing.T) {
	p := config.ProviderConfig{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: "http://example.invalid",
		APIKey: "key-a",
		Credentials: []config.CredentialConfig{
			{Name: "b", APIKey: "key-b", Enabled: true},
			{Name: "c", APIKey: "key-c", Enabled: true},
		},
		AuthMode: "bearer", Enabled: true, MaxConcurrency: 2,
	}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	a.c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("network unavailable")
	})

	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("unexpected response: %v", resp.Status)
	}
	if err == nil {
		t.Fatal("expected transport error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport failure fanned out across credentials: calls=%d want 1", got)
	}
	st := a.Stats()
	if st.ActiveRequests != 0 || st.WaitingRequests != 0 {
		t.Fatalf("provider capacity leaked after transport error: %+v", st)
	}
}

type idleCloseTrackingTransport struct {
	closed atomic.Bool
}

func (t *idleCloseTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
	}, nil
}

func (t *idleCloseTrackingTransport) CloseIdleConnections() {
	t.closed.Store(true)
}

func TestHTTPAdapterCloseIdleConnectionsReachesTransport(t *testing.T) {
	p := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		AuthMode: "none", Enabled: true, MaxConcurrency: 1,
	}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tr := &idleCloseTrackingTransport{}
	a.c.Transport = tr
	a.streamC.Transport = tr
	a.CloseIdleConnections()
	if !tr.closed.Load() {
		t.Fatal("adapter did not close idle transport connections")
	}
}
