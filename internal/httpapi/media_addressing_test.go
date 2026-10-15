package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Per-cloud media addressing (PR-A): provider-native URL resolution
// mirrors chat/messages deployment semantics (Azure deployment +
// api-version), and unsupported combinations refuse explicitly.

func TestResolveMediaUpstreamOpenAICompatible(t *testing.T) {
	p := config.ProviderConfig{ID: "p", Type: "openai_compatible"}
	p.ApplyDefaults()
	for kind, want := range map[mediaKind]string{
		mediaEmbeddings:     "/v1/embeddings",
		mediaImages:         "/v1/images/generations",
		mediaSpeech:         "/v1/audio/speech",
		mediaTranscriptions: "/v1/audio/transcriptions",
		mediaRerank:         "/v1/rerank",
	} {
		got, ok := resolveMediaUpstream(p, kind, "upstream-model")
		if !ok || got != want {
			t.Errorf("openai_compatible %s: got %q ok=%v want %q", kind, got, ok, want)
		}
	}
}

func TestResolveMediaUpstreamAzure(t *testing.T) {
	p := config.ProviderConfig{ID: "az", Type: "azure_openai"}
	p.ApplyDefaults() // api_version default 2024-10-21
	for kind, want := range map[mediaKind]string{
		mediaEmbeddings:     "/openai/deployments/dep-gpt4o/embeddings?api-version=2024-10-21",
		mediaImages:         "/openai/deployments/dep-gpt4o/images/generations?api-version=2024-10-21",
		mediaSpeech:         "/openai/deployments/dep-gpt4o/audio/speech?api-version=2024-10-21",
		mediaTranscriptions: "/openai/deployments/dep-gpt4o/audio/transcriptions?api-version=2024-10-21",
	} {
		got, ok := resolveMediaUpstream(p, kind, "dep-gpt4o")
		if !ok || got != want {
			t.Errorf("azure %s: got %q ok=%v want %q", kind, got, ok, want)
		}
	}
	if _, ok := resolveMediaUpstream(p, mediaRerank, "dep-gpt4o"); ok {
		t.Errorf("azure rerank must be refused (no native equivalent)")
	}
	// Custom api-version from config is honored, like chat.
	p.APIVersion = "2025-01-01-preview"
	got, ok := resolveMediaUpstream(p, mediaEmbeddings, "dep-gpt4o")
	if !ok || !strings.HasSuffix(got, "?api-version=2025-01-01-preview") {
		t.Errorf("azure custom version: got %q ok=%v", got, ok)
	}
	// Empty deployment model refuses rather than building a broken URL.
	if _, ok := resolveMediaUpstream(p, mediaEmbeddings, "  "); ok {
		t.Errorf("azure empty deployment must refuse")
	}
}

func TestResolveMediaUpstreamUnsupportedClouds(t *testing.T) {
	for _, typ := range []string{"vertex", "bedrock", "gemini", "anthropic_compatible", "openai_responses"} {
		p := config.ProviderConfig{ID: "x", Type: typ}
		p.ApplyDefaults()
		for _, kind := range []mediaKind{mediaEmbeddings, mediaImages, mediaSpeech, mediaTranscriptions, mediaRerank} {
			if _, ok := resolveMediaUpstream(p, kind, "m"); ok {
				t.Errorf("type %s kind %s must refuse (no byte-compatible native endpoint)", typ, kind)
			}
		}
	}
}

func TestAzureMediaEndToEndDeploymentAddressing(t *testing.T) {
	cap := &mediaCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		// Record query too via path field hack: mediaCapture only keeps path;
		// re-check full URL here for api-version.
		if r.URL.RawQuery != "api-version=2024-10-21" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "az", Name: "Azure", Type: "azure_openai", BaseURL: up.URL,
		APIKey: "azure-secret", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "dep-emb", Enabled: true, Weight: 1}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := mediaPost(t, s, "/v1/embeddings", "application/json", []byte(`{"model":"m","input":"hi"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("azure embeddings status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cap.path != "/openai/deployments/dep-emb/embeddings" {
		t.Fatalf("azure upstream path=%q", cap.path)
	}
	if !strings.Contains(string(cap.body), `"dep-emb"`) {
		t.Fatalf("azure model not rewritten to deployment: %s", cap.body)
	}
}

func TestMediaUnsupportedCombinationsRefuseExplicitly(t *testing.T) {
	// Vertex embeddings: no byte-compatible endpoint — 400 naming the boundary.
	mkGateway := func(p config.ProviderConfig) *Server {
		p.ApplyDefaults()
		cfg := config.Default()
		cfg.Providers = []config.ProviderConfig{p}
		s := testGateway(t, cfg)
		s.SyncCapabilityContracts()
		return s
	}
	upVertex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upVertex.Close)
	s := mkGateway(config.ProviderConfig{
		ID: "vx", Name: "Vertex", Type: "vertex", BaseURL: upVertex.URL,
		APIKey: "tok", VertexProject: "proj-1", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true, Weight: 1}},
	})
	rr := mediaPost(t, s, "/v1/embeddings", "application/json", []byte(`{"model":"m","input":"hi"}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "openai_compatible") {
		t.Fatalf("vertex refuse: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Bedrock embeddings likewise.
	t.Setenv("NEXAROUTE_TEST_BR_SECRET_MEDIA", "s3cr3t")
	upBedrock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upBedrock.Close)
	s2 := mkGateway(config.ProviderConfig{
		ID: "br", Name: "Bedrock", Type: "bedrock", BaseURL: upBedrock.URL,
		APIKey: "AKID", SecretKeyEnv: "NEXAROUTE_TEST_BR_SECRET_MEDIA", AWSRegion: "us-east-1",
		Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "anthropic.claude-v1", Enabled: true, Weight: 1}},
	})
	rr = mediaPost(t, s2, "/v1/embeddings", "application/json", []byte(`{"model":"m","input":"hi"}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "openai_compatible") {
		t.Fatalf("bedrock refuse: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Azure rerank: no native equivalent — 400.
	cap := &mediaCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "az", Name: "Azure", Type: "azure_openai", BaseURL: up.URL,
		APIKey: "k", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "dep-x", Enabled: true, Weight: 1}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s3 := testGateway(t, cfg)
	s3.SyncCapabilityContracts()
	rr = mediaPost(t, s3, "/v1/rerank", "application/json", []byte(`{"model":"m","query":"q","documents":["a"]}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "openai_compatible") {
		t.Fatalf("azure rerank refuse: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cap.calls != 0 {
		t.Fatalf("refused rerank must never reach upstream (calls=%d)", cap.calls)
	}
}
