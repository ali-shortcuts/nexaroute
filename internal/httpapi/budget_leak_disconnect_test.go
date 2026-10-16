package httpapi

// Wave C-4 (issue #405), part 3: adversarial client
// disconnect/cancellation. A disconnect at any point in a request's
// life must still close the budget hold exactly once — never leak it
// open, never double-commit. Disconnects keep the ledger's
// documented TTL backstop (the upstream may have done work), so the
// invariant is the Tier-3 one: after the TTL the hold is gone and
// exactly one estimate is committed.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// TestLeakDisconnect_DuringAttempt: the client goes away while the
// upstream attempt is still in flight (deterministic: the upstream
// blocks until the request context is done or a safety timeout).
// The hold must survive as a TTL reservation and then close exactly
// once — never leak open, never double-commit.
func TestLeakDisconnect_DuringAttempt(t *testing.T) {
	hit := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done(): // gateway cancels on client disconnect
		case <-time.After(5 * time.Second): // safety net for cleanup
		}
	}))
	t.Cleanup(up.Close)
	s := leakGateway(t, 5, leakOKChat(), func(cfg *config.Config) {
		cfg.Providers[0].BaseURL = up.URL
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(leakChatBody))
	req = req.WithContext(ctx)
	req.Header.Set("x-api-key", "lk-key")
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		t.Fatal("guard: upstream was never hit")
	}
	cancel() // client disconnects mid-attempt
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("guard: handler did not return after client cancel")
	}
	leakAssertTTLExactlyOnce(t, s, "disconnect during attempt")
}

// TestLeakDisconnect_MidStream: the client cancels after receiving
// the first stream chunks. The upstream streams until the request
// context is done, so the stream can never complete cleanly — no
// usage report is possible and the TTL backstop must close the hold
// exactly once.
func TestLeakDisconnect_MidStream(t *testing.T) {
	chunk := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(up.Close)
	s := leakGateway(t, 5, leakOKChat(), func(cfg *config.Config) {
		cfg.Providers[0].BaseURL = up.URL
	})
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "lk-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("guard: stream request failed: %v", err)
	}
	// Read exactly two chunks, then disconnect mid-stream.
	buf := make([]byte, 4096)
	chunks := 0
	for chunks < 2 {
		n, rerr := resp.Body.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), "data:") {
			chunks++
		}
		if rerr != nil {
			break
		}
	}
	if chunks < 2 {
		t.Fatalf("guard: only received %d chunks before cancel", chunks)
	}
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Give the gateway a moment to observe the disconnect and unwind.
	time.Sleep(500 * time.Millisecond)
	leakAssertTTLExactlyOnce(t, s, "disconnect mid-stream")
}

// TestLeakDisconnect_BeforeFirstByte: cancel immediately after the
// request is dispatched, before any upstream bytes arrive.
func TestLeakDisconnect_BeforeFirstByte(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second): // safety net for cleanup
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	}))
	t.Cleanup(up.Close)
	s := leakGateway(t, 5, leakOKChat(), func(cfg *config.Config) {
		cfg.Providers[0].BaseURL = up.URL
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(leakChatBody))
	req = req.WithContext(ctx)
	req.Header.Set("x-api-key", "lk-key")
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(200 * time.Millisecond) // dispatch is in flight, upstream blocked
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("guard: handler did not return after client cancel")
	}
	leakAssertTTLExactlyOnce(t, s, "disconnect before first byte")
}
