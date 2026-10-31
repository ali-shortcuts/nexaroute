package nexaflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayClientLoopbackOnly is the token-disclosure regression
// test for nexaflow.gateway_base_url: a GatewayClient must never
// resolve its auth token, and must never make an HTTP request, when
// its base URL is not an approved loopback URL. The recording
// resolver and recording transport prove both halves: if either were
// called, the test fails, so a passing test means the token stayed
// inside the process.
//
// Approved forms (127.0.0.1, ::1, localhost) are covered separately
// below against real loopback servers, so this file pins both the
// acceptance and the rejection sides of the same rule.
func TestGatewayClientLoopbackOnly(t *testing.T) {
	ctx := context.Background()
	rejected := []string{
		"https://example.com",
		"https://gateway.example:8443",
		"http://8.8.8.8:8080",
		"http://10.0.0.5:8080",
		"http://127.0.0.2:8080",
		"http://127.0.0.1.evil.com:8080",
		"http://sub.localhost:8080",
		"http://user:secret@127.0.0.1:8080",
		"http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080?token=x",
		"http://127.0.0.1:8080#section",
		"ftp://127.0.0.1:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"http://0.0.0.0:8080",
		"://bad",
		"",
	}
	for _, base := range rejected {
		t.Run(base, func(t *testing.T) {
			resolverCalled := false
			transportCalled := false
			client := &GatewayClient{
				BaseURL:      base,
				AuthTokenRef: "tok-ref",
				SecretResolver: func(string) (string, error) {
					resolverCalled = true
					return "test-token-that-must-not-leave", nil
				},
				HTTPClient: &http.Client{
					Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
						transportCalled = true
						return nil, errors.New("transport must not be called for a rejected base URL")
					}),
				},
			}
			if _, err := client.Chat(ctx, ChatRequest{Selector: "x", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err == nil {
				t.Fatalf("Chat with base URL %q accepted, want rejection", base)
			}
			if _, _, err := client.VideoAsset(ctx, "job-9", "a1"); err == nil {
				t.Fatalf("VideoAsset with base URL %q accepted, want rejection", base)
			}
			if resolverCalled {
				t.Fatalf("auth token resolver was called for rejected base URL %q", base)
			}
			if transportCalled {
				t.Fatalf("HTTP transport was called for rejected base URL %q", base)
			}
		})
	}
}

// TestGatewayClientLoopbackAccepted proves the approved spellings
// still work end to end, with the Bearer token actually arriving at
// the loopback server (and nowhere else in this test).
func TestGatewayClientLoopbackAccepted(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// IPv4 loopback, as httptest reports it.
	ipv4 := &GatewayClient{
		BaseURL: srv.URL, AuthTokenRef: "tok-ref",
		SecretResolver: func(string) (string, error) { return "test-token", nil },
	}
	if chat, err := ipv4.Chat(ctx, ChatRequest{Selector: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err != nil || chat.Text != "hello" {
		t.Fatalf("IPv4 loopback chat = %+v, err %v", chat, err)
	}

	// localhost spelling of the same loopback server.
	localhostURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	localhostClient := &GatewayClient{
		BaseURL: localhostURL, AuthTokenRef: "tok-ref",
		SecretResolver: func(string) (string, error) { return "test-token", nil },
	}
	if chat, err := localhostClient.Chat(ctx, ChatRequest{Selector: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err != nil || chat.Text != "hello" {
		t.Fatalf("localhost chat = %+v, err %v", chat, err)
	}

	// IPv6 loopback, served on [::1] explicitly.
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable in this environment: %v", err)
	}
	v6srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	v6srv.Listener = ln
	v6srv.Start()
	defer v6srv.Close()
	v6 := &GatewayClient{
		BaseURL: v6srv.URL, AuthTokenRef: "tok-ref",
		SecretResolver: func(string) (string, error) { return "test-token", nil },
	}
	if chat, err := v6.Chat(ctx, ChatRequest{Selector: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err != nil || chat.Text != "hello" {
		t.Fatalf("IPv6 loopback chat = %+v, err %v (url %s)", chat, err, v6srv.URL)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
