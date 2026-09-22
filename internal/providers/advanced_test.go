package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/universal-llm-gateway/internal/config"
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
