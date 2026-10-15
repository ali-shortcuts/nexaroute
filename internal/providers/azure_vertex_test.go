package providers

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

// Wire-shape tests for the M5-1a cloud adapters. Azure OpenAI and
// Vertex AI reuse existing wire protocols; what must be exact is the
// URL construction and the auth header, asserted here against fake
// servers at the adapter level (end-to-end routing is covered in
// internal/httpapi).

type capturedRequest struct {
	path, rawQuery, apiKey, authorization, googKey string
	body                                           string
}

func captureServer(t *testing.T, cap *capturedRequest, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.path = r.URL.Path
		cap.rawQuery = r.URL.RawQuery
		cap.apiKey = r.Header.Get("api-key")
		cap.authorization = r.Header.Get("Authorization")
		cap.googKey = r.Header.Get("x-goog-api-key")
		cap.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAzureAdapterDeploymentPathAndAPIKeyAuth(t *testing.T) {
	var cap capturedRequest
	srv := captureServer(t, &cap, `{"id":"c","choices":[]}`)
	p := config.ProviderConfig{
		ID: "az", Name: "Azure", Type: "azure_openai", BaseURL: srv.URL,
		APIKey: "azure-secret", APIVersion: "2025-01-01-preview", Enabled: true,
	}
	p.ApplyDefaults()
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"dep-gpt4o","messages":[{"role":"user","content":"hi"}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if cap.path != "/openai/deployments/dep-gpt4o/chat/completions" {
		t.Errorf("azure path=%q", cap.path)
	}
	if cap.rawQuery != "api-version=2025-01-01-preview" {
		t.Errorf("azure query=%q", cap.rawQuery)
	}
	if cap.apiKey != "azure-secret" {
		t.Errorf("api-key header=%q, want the provider credential", cap.apiKey)
	}
	if cap.authorization != "" || cap.googKey != "" {
		t.Errorf("azure must not send bearer/goog credentials: auth=%q goog=%q", cap.authorization, cap.googKey)
	}
}

func TestVertexAdapterModelPathAndBearerAuth(t *testing.T) {
	var cap capturedRequest
	srv := captureServer(t, &cap, `{"candidates":[]}`)
	p := config.ProviderConfig{
		ID: "vx", Name: "Vertex", Type: "vertex", BaseURL: srv.URL,
		APIKey: "oauth-token", VertexProject: "my-project", VertexLocation: "europe-west4", Enabled: true,
	}
	p.ApplyDefaults()
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	path := a.vertexModelPath("gemini-2.0-flash", true)
	resp, err := a.DoPath(context.Background(), http.MethodPost, path, []byte(`{"contents":[]}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	want := "/v1/projects/my-project/locations/europe-west4/publishers/google/models/gemini-2.0-flash:streamGenerateContent"
	if cap.path != want {
		t.Errorf("vertex path=%q, want %q", cap.path, want)
	}
	if cap.rawQuery != "alt=sse" {
		t.Errorf("vertex stream query=%q", cap.rawQuery)
	}
	if cap.authorization != "Bearer oauth-token" {
		t.Errorf("vertex authorization=%q, want the bearer credential", cap.authorization)
	}
	if got := a.vertexModelPath("gemini-2.0-flash", false); !strings.HasSuffix(got, ":generateContent") {
		t.Errorf("vertex non-stream path=%q", got)
	}
}
