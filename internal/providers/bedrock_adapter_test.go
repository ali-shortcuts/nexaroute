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

type bedrockCapture struct {
	mu            sync.Mutex
	path          string
	authorization string
	amzDate       string
	token         string
	body          string
}

func (c *bedrockCapture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.EscapedPath()
	c.authorization = r.Header.Get("Authorization")
	c.amzDate = r.Header.Get("X-Amz-Date")
	c.token = r.Header.Get("X-Amz-Security-Token")
	c.body = string(b)
}

func bedrockTestAdapter(t *testing.T, baseURL string) *httpAdapter {
	t.Helper()
	t.Setenv("NEXAROUTE_TEST_AWS_SECRET", "test-secret")
	t.Setenv("NEXAROUTE_TEST_AWS_TOKEN", "test-token")
	p := config.ProviderConfig{
		ID: "br", Name: "Bedrock", Type: "bedrock", BaseURL: baseURL,
		APIKey: "AKIDTEST", SecretKeyEnv: "NEXAROUTE_TEST_AWS_SECRET",
		SessionTokenEnv: "NEXAROUTE_TEST_AWS_TOKEN", AWSRegion: "eu-west-1",
		Enabled: true, MaxConcurrency: 4,
	}
	p.ApplyDefaults()
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBedrockAdapterSignsAndAddressesConverse(t *testing.T) {
	cap := &bedrockCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":1}}`)
	}))
	defer up.Close()
	a := bedrockTestAdapter(t, up.URL)

	resp, err := a.DoPath(context.Background(), http.MethodPost,
		bedrockModelPath("anthropic.claude-v1", false),
		[]byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if cap.path != "/model/anthropic.claude-v1/converse" {
		t.Fatalf("path=%q", cap.path)
	}
	if !strings.HasPrefix(cap.authorization, "AWS4-HMAC-SHA256 Credential=AKIDTEST/") ||
		!strings.Contains(cap.authorization, "/eu-west-1/bedrock/aws4_request") ||
		!strings.Contains(cap.authorization, "SignedHeaders=") ||
		!strings.Contains(cap.authorization, "Signature=") {
		t.Fatalf("authorization=%q", cap.authorization)
	}
	if !strings.Contains(cap.authorization, "host") || !strings.Contains(cap.authorization, "x-amz-date") {
		t.Fatalf("signed headers incomplete: %q", cap.authorization)
	}
	if cap.amzDate == "" || cap.token != "test-token" {
		t.Fatalf("amzDate=%q token=%q", cap.amzDate, cap.token)
	}
	if strings.Contains(cap.authorization, "test-secret") {
		t.Fatal("the secret must never appear in the request")
	}
}

func TestBedrockAdapterMissingSecretFailsClosed(t *testing.T) {
	p := config.ProviderConfig{
		ID: "br", Name: "Bedrock", Type: "bedrock", BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey: "AKIDTEST", SecretKeyEnv: "NEXAROUTE_TEST_AWS_SECRET_UNSET",
		Enabled: true, MaxConcurrency: 4,
	}
	p.ApplyDefaults()
	if _, err := newHTTPAdapter(p, time.Second); err == nil {
		t.Fatal("adapter construction must fail closed when the secret env var is unset")
	} else if strings.Contains(err.Error(), "secret access key: env var") == false {
		t.Fatalf("error should name the env var: %v", err)
	}
}

func TestBedrockModelPathEscapesInferenceProfileIDs(t *testing.T) {
	got := bedrockModelPath("eu.anthropic.claude-v1:0", true)
	want := "/model/eu.anthropic.claude-v1%3A0/converse-stream"
	if got != want {
		t.Fatalf("path=%q want %q", got, want)
	}
}
