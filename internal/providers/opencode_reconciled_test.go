package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Reconciliation tests for PRs #694/#695 (see
// docs/OPENCODE_RECONCILIATION.md, defects R1/R2 and gaps R3–R6).
// All canaries below are test-only strings; none is a real credential.

func reconciledCall(t *testing.T, a *httpAdapter, ctx context.Context, method, path string, fwd http.Header) {
	t.Helper()
	resp, err := a.DoPath(ctx, method, path, []byte(`{"model":"m1","messages":[]}`), false, fwd)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
}

// R1: a statically configured provider header must never supply the
// x-opencode-session value. Only an allowlisted forwarded client value
// counts as a client value; Console then deletes the static value and
// Go overwrites it with the derived value.
func TestReconciledSessionStaticHeaderNeverSent(t *testing.T) {
	newAdapter := func(t *testing.T, typ string, fwdNames []string) (*httpAdapter, *[]http.Header) {
		t.Helper()
		var paths []string
		var headers []http.Header
		var keys []string
		srv := opencodeFakeServer(t, &paths, &headers, &keys)
		t.Cleanup(srv.Close)
		p := config.ProviderConfig{ID: "x", Type: typ, BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
			Headers: map[string]string{"x-opencode-session": "ses_static_value"}, ForwardHeaders: fwdNames}
		a, err := newHTTPAdapter(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return a, &headers
	}

	t.Run("console deletes static header", func(t *testing.T) {
		a, headers := newAdapter(t, "opencode-console", nil)
		reconciledCall(t, a, WithSessionKey(context.Background(), "conv-a"), http.MethodPost, config.OpencodeChatPath, nil)
		if got := (*headers)[0].Get("x-opencode-session"); got != "" {
			t.Fatalf("console sent static session header %q, want empty", got)
		}
	})
	t.Run("go overwrites static header with derived value", func(t *testing.T) {
		a, headers := newAdapter(t, "opencode-go", nil)
		reconciledCall(t, a, WithSessionKey(context.Background(), "conv-a"), http.MethodPost, config.OpencodeChatPath, nil)
		got := (*headers)[0].Get("x-opencode-session")
		if got == "" || got == "ses_static_value" || !strings.HasPrefix(got, "ses_") {
			t.Fatalf("go header = %q, want derived ses_ value, not the static one", got)
		}
	})
	t.Run("allowlisted client value still wins over static", func(t *testing.T) {
		for _, typ := range []string{"opencode-console", "opencode-go"} {
			a, headers := newAdapter(t, typ, []string{"x-opencode-session"})
			fwd := http.Header{}
			fwd.Set("x-opencode-session", "ses_client_value")
			reconciledCall(t, a, WithSessionKey(context.Background(), "conv-a"), http.MethodPost, config.OpencodeChatPath, fwd)
			if got := (*headers)[0].Get("x-opencode-session"); got != "ses_client_value" {
				t.Fatalf("%s: header = %q, want the allowlisted client value", typ, got)
			}
		}
	})
	t.Run("unallowlisted client value does not win over static", func(t *testing.T) {
		// Without ForwardHeaders the client header never reaches the
		// adapter, so Console deletes (not forwards) and Go derives.
		a, headers := newAdapter(t, "opencode-console", nil)
		fwd := http.Header{}
		fwd.Set("x-opencode-session", "ses_client_value")
		reconciledCall(t, a, WithSessionKey(context.Background(), "conv-a"), http.MethodPost, config.OpencodeChatPath, fwd)
		if got := (*headers)[0].Get("x-opencode-session"); got != "" {
			t.Fatalf("console sent %q for an unallowlisted client value, want empty", got)
		}
	})
}

// R2: configured chat/responses/messages paths match by suffix, so a
// provider whose base URL carries a prefix (or a custom chat path)
// still gets the header on inference calls.
func TestReconciledSessionConfiguredPathSuffix(t *testing.T) {
	var paths []string
	var headers []http.Header
	var keys []string
	srv := opencodeFakeServer(t, &paths, &headers, &keys)
	defer srv.Close()
	p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv.URL + "/prefix", APIKey: "test-key-1", AuthMode: "bearer",
		ChatPath: "/custom/chat", ResponsesPath: "/openai/v1/responses", MessagesPath: "/anthropic/v1/messages"}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithSessionKey(context.Background(), "conv-a")
	reconciledCall(t, a, ctx, http.MethodPost, "/custom/chat", nil)
	if got := headers[0].Get("x-opencode-session"); got == "" || !strings.HasPrefix(got, "ses_") {
		t.Fatalf("prefixed custom chat path got no derived session header, got %q", got)
	}
	reconciledCall(t, a, ctx, http.MethodPost, "/v1/embeddings", nil)
	if got := headers[1].Get("x-opencode-session"); got != "" {
		t.Fatalf("embeddings path leaked session header %q", got)
	}
}

// R4: an expired session pin is ignored (tournament picks); a live pin
// still pins. White-box: the pin map is seeded directly.
func TestReconciledSessionPinExpiredFallsBack(t *testing.T) {
	mk := func() *httpAdapter {
		// BaseURL is a dummy loopback: reserveCredentialWithSession
		// never touches the network, but construction validates egress.
		p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: "http://127.0.0.1:9", APIKey: "test-key-1", AuthMode: "bearer",
			Credentials: []config.CredentialConfig{{Name: "pool-a", APIKey: "test-key-2", Enabled: true}}}
		a, err := newHTTPAdapter(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	h := SessionHash("conv-expired")
	t.Run("expired pin falls back to tournament", func(t *testing.T) {
		a := mk()
		a.sessionPins = map[string]sessionPin{h: {credIdx: 1, expires: time.Now().Add(-time.Hour)}}
		idx, _, ok := a.reserveCredentialWithSession(map[int]bool{}, h)
		if !ok || idx != 0 {
			t.Fatalf("expired pin: chosen=%d ok=%v, want tournament pick 0", idx, ok)
		}
	})
	t.Run("live pin still pins", func(t *testing.T) {
		a := mk()
		a.sessionPins = map[string]sessionPin{h: {credIdx: 1, expires: time.Now().Add(time.Hour)}}
		idx, _, ok := a.reserveCredentialWithSession(map[int]bool{}, h)
		if !ok || idx != 1 {
			t.Fatalf("live pin: chosen=%d ok=%v, want pinned 1", idx, ok)
		}
	})
}

// R6: parallel requests sharing one session key pin a single
// credential (no split-brain under concurrency). Run under -race.
func TestReconciledConcurrentSessionAffinity(t *testing.T) {
	// Own mutex-protected fake: opencodeFakeServer appends without
	// synchronization, which -race would (correctly) flag here.
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "pool-a", APIKey: "test-key-2", Enabled: true}}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithSessionKey(context.Background(), "conv-parallel")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.DoPath(ctx, http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"m1","messages":[]}`), false, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 16 {
		t.Fatalf("requests = %d, want 16", len(keys))
	}
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("concurrent affinity split: keys %v", keys)
		}
	}
}

// R5: an opencode adapter with zero credentials sends no Authorization
// header and returns the upstream 401 — no panic, no fake success.
func TestReconciledEmptyPoolOpencode(t *testing.T) {
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"missing key"}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv.URL, AuthMode: "bearer"}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.DoPath(context.Background(), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"m1","messages":[]}`), false, nil)
	if err != nil {
		t.Fatalf("empty-pool DoPath must return the upstream response, got error %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty-pool status = %d, want 401", resp.StatusCode)
	}
	if len(headers) != 1 || headers[0].Get("Authorization") != "" {
		t.Fatalf("empty-pool must send no Authorization header, got %v", headers)
	}
}

// Stream handling: a mid-stream upstream death triggers no second
// credential attempt — the adapter tries exactly one credential.
func TestReconciledStreamMidErrorNoRetry(t *testing.T) {
	var mu sync.Mutex
	var hits int
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Handler returns without [DONE]: the stream dies mid-flight.
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "go1", Type: "opencode-go", BaseURL: srv.URL, APIKey: "test-key-1", AuthMode: "bearer",
		Credentials: []config.CredentialConfig{{Name: "pool-a", APIKey: "test-key-2", Enabled: true}}}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.DoPath(context.Background(), http.MethodPost, config.OpencodeChatPath, []byte(`{"model":"m1","messages":[]}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "first") {
		t.Fatalf("stream body lost the first chunk: %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("mid-stream death caused %d upstream hits, want exactly 1 (no second credential attempt)", hits)
	}
	if len(keys) != 1 {
		t.Fatalf("credential keys used = %v, want exactly one", keys)
	}
}
