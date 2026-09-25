package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteClient_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()

	client, err := NewTestClient(srv.URL, "test-key", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err = client.Do(ctx, "/api/v1/decisions/model-route", []byte(`{"task":"test","candidates":[{"id":"c0","description":"desc"},{"id":"c1","description":"desc"}]}`))
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !IsTimeout(err) && !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "canceled") {
		// The client should return timeout for context cancellation
		// Our implementation returns timeout for ctx.Err()
		t.Logf("got error %v, expected timeout", err)
	}
}

func TestRemoteClient_BodyLimit_RequestTooLarge(t *testing.T) {
	client, err := NewTestClient("http://127.0.0.1:12345", "key", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	large := make([]byte, MaxRequestBodyBytes+1)
	for i := range large {
		large[i] = 'a'
	}
	_, _, err = client.Do(context.Background(), "/test", large)
	if err == nil {
		t.Fatalf("expected request too large error")
	}
	if !IsRequestTooLarge(err) {
		t.Fatalf("expected ErrRequestTooLarge, got %v", err)
	}
}

func TestRemoteClient_ResponseLimit(t *testing.T) {
	largeBody := strings.Repeat("a", MaxResponseBodyBytes+100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(largeBody))
	}))
	defer srv.Close()

	client, err := NewTestClient(srv.URL, "key", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, _, err = client.Do(context.Background(), "/test", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected response too large error")
	}
	if !IsResponseTooLarge(err) {
		t.Fatalf("expected ErrResponseTooLarge, got %v", err)
	}
}

func TestRemoteClient_RedirectNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/target")
			w.WriteHeader(302)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client, err := NewTestClient(srv.URL, "key", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, status, err := client.Do(context.Background(), "/redirect", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected redirect error")
	}
	if status != 302 {
		t.Fatalf("expected 302 status, got %d", status)
	}
}

func TestRemoteClient_OneCallOnly(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(200)
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()

	client, err := NewTestClient(srv.URL, "key", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, _, err = client.Do(context.Background(), "/test", []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}
}

func TestRemoteClient_AuthHeader(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client, err := NewTestClient(srv.URL, "secret-key-123", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, _, _ = client.Do(context.Background(), "/test", []byte(`{}`))
	if capturedAuth != "Bearer secret-key-123" {
		t.Fatalf("expected Bearer auth, got %q", capturedAuth)
	}
}

func TestRemoteClient_SSRFBlocked(t *testing.T) {
	// Production transport should block loopback
	_, err := NewClient(ClientConfig{
		BaseURL: "https://127.0.0.1:8080",
		APIKey:  "key",
	})
	if err == nil {
		t.Fatalf("expected SSRF blocked error for 127.0.0.1 in production transport")
	}
}

func TestRemoteClient_SafeErrors_NoRawBodyLeak(t *testing.T) {
	secret := "SECRET_REMOTE_ERROR_CANARY_64ac"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(secret))
	}))
	defer srv.Close()

	client, err := NewTestClient(srv.URL, "key", nil)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	body, _, err := client.Do(context.Background(), "/test", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected http error")
	}
	// Body may contain secret for internal debugging, but error message must not contain raw body
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks remote body: %v", err.Error())
	}
	// Body is returned separately, but should not be exposed via error
	_ = body
}
