package providers

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
)

// Audit #371 finding F2: the evaluation twin built its HTTP clients
// without the production adapter's CheckRedirect, so a cross-origin
// redirect was followed and the provider's credential header
// (x-api-key here — Go's stdlib strips only Authorization /
// Www-Authenticate / Cookie* cross-host) reached the redirect target.
// The twin must refuse cross-origin redirects exactly like the
// production adapter.
func TestEvaluationTwinRefusesCrossOriginRedirect(t *testing.T) {
	var targetSawAPIKey atomic.Bool
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.Header.Get("x-api-key") != "" {
			targetSawAPIKey.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"stolen"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	production, err := NewAdapter(config.ProviderConfig{
		ID: "eval-redirect", Type: "openai_compatible", BaseURL: source.URL,
		APIKey: "provider-x-api-key-secret", AuthMode: "x-api-key",
		Enabled: true, MaxConcurrency: 2,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	twin, ok := EvaluationTwin(production)
	if !ok {
		t.Fatal("no evaluation twin")
	}

	result, err := LiveComplete(context.Background(), twin, "unit-model", "hello", 16)
	if err != nil {
		t.Fatalf("unexpected LiveComplete error: %v", err)
	}
	// LiveComplete reports transport failures in the result, not as an
	// error: a refused redirect must surface as a transport failure.
	if result.ErrorType != LiveErrTransport {
		t.Fatalf("ErrorType = %q, want %q (cross-origin redirect refused)", result.ErrorType, LiveErrTransport)
	}
	if !strings.Contains(result.Message, "cross-origin") {
		t.Fatalf("Message = %q, want cross-origin refusal", result.Message)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times", targetHits.Load())
	}
	if targetSawAPIKey.Load() {
		t.Fatal("provider x-api-key was forwarded to the cross-origin redirect target")
	}
}
