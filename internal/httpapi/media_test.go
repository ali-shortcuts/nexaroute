package httpapi

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Media passthrough e2e fixtures (M5-1c).

type mediaCapture struct {
	mu          sync.Mutex
	path        string
	contentType string
	body        []byte
	calls       int
}

func (c *mediaCapture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.contentType = r.Header.Get("Content-Type")
	c.body = b
	c.calls++
}

func mediaGateway(t *testing.T, providerType string, handler http.HandlerFunc) (*Server, *mediaCapture) {
	t.Helper()
	cap := &mediaCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "mp", Name: "Media", Type: providerType, BaseURL: up.URL,
		APIKey: "k", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model-x", Enabled: true, Weight: 1}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, cap
}

// mediaGatewayNoFallback is mediaGateway with the router-wide
// fallback_on_unknown_model control disabled.
func mediaGatewayNoFallback(t *testing.T, providerType string, handler http.HandlerFunc) (*Server, *mediaCapture) {
	t.Helper()
	cap := &mediaCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Routing.FallbackOnUnknownModel = false
	p := config.ProviderConfig{
		ID: "mp", Name: "Media", Type: providerType, BaseURL: up.URL,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model-x", Enabled: true, Weight: 1}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, cap
}

func mediaPost(t *testing.T, s *Server, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestMediaEmbeddingsEndToEnd(t *testing.T) {
	s, cap := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"upstream-model-x","usage":{"prompt_tokens":7,"total_tokens":7}}`)
	})
	rr := mediaPost(t, s, "/v1/embeddings", "application/json",
		[]byte(`{"model":"m","input":"hello"}`))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"embedding"`) {
		t.Fatalf("embeddings status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cap.path != "/v1/embeddings" {
		t.Fatalf("upstream path=%q", cap.path)
	}
	if !strings.Contains(string(cap.body), `"model":"upstream-model-x"`) {
		t.Fatalf("model not rewritten upstream: %s", cap.body)
	}
	if rr.Header().Get("X-Gateway-Upstream-Model") != "upstream-model-x" {
		t.Fatalf("gateway headers missing: %v", rr.Header())
	}
}

func TestMediaSpeechBinaryPassthrough(t *testing.T) {
	audio := []byte{0x49, 0x44, 0x33, 0x00, 0xFF, 0x10, 0x42}
	s, _ := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	})
	rr := mediaPost(t, s, "/v1/audio/speech", "application/json",
		[]byte(`{"model":"m","input":"say it","voice":"alloy"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("speech status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), audio) {
		t.Fatalf("binary body mangled: %v", rr.Body.Bytes())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content type=%q", ct)
	}
}

func TestMediaTranscriptionsMultipartModelRewrite(t *testing.T) {
	s, cap := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"transcribed"}`)
	})
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("model", "m")
	fw, err := mw.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("RIFF-fake-audio-bytes"))
	_ = mw.Close()

	rr := mediaPost(t, s, "/v1/audio/transcriptions", mw.FormDataContentType(), form.Bytes())
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "transcribed") {
		t.Fatalf("transcriptions status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cap.path != "/v1/audio/transcriptions" {
		t.Fatalf("upstream path=%q", cap.path)
	}
	if !strings.HasPrefix(cap.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("upstream content type=%q", cap.contentType)
	}
	upstreamBody := string(cap.body)
	if !strings.Contains(upstreamBody, "upstream-model-x") || strings.Contains(upstreamBody, `"m"`) && strings.Contains(upstreamBody, "name=\"model\"\r\n\r\nm\r\n") {
		t.Fatalf("model field not rewritten: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, "RIFF-fake-audio-bytes") || !strings.Contains(upstreamBody, "clip.wav") {
		t.Fatalf("file part mangled: %s", upstreamBody)
	}
}

func TestMediaImagesAndRerankPassthrough(t *testing.T) {
	s, cap := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "rerank") {
			_, _ = io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.9}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`)
	})
	rr := mediaPost(t, s, "/v1/images/generations", "application/json",
		[]byte(`{"model":"m","prompt":"a cat"}`))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "b64_json") || cap.path != "/v1/images/generations" {
		t.Fatalf("images status=%d path=%q body=%s", rr.Code, cap.path, rr.Body.String())
	}
	rr = mediaPost(t, s, "/v1/rerank", "application/json",
		[]byte(`{"model":"m","query":"q","documents":["a","b"],"top_n":1}`))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "relevance_score") || cap.path != "/v1/rerank" {
		t.Fatalf("rerank status=%d path=%q body=%s", rr.Code, cap.path, rr.Body.String())
	}
}

func TestMediaUnsupportedProviderTypeAndUnknownModel(t *testing.T) {
	s, _ := mediaGateway(t, "anthropic_compatible", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	rr := mediaPost(t, s, "/v1/embeddings", "application/json",
		[]byte(`{"model":"m","input":"hello"}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "openai_compatible") {
		t.Fatalf("unsupported type: status=%d body=%s", rr.Code, rr.Body.String())
	}
	s2, cap2 := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	rr = mediaPost(t, s2, "/v1/embeddings", "application/json",
		[]byte(`{"model":"nope","input":"hello"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown model should use fallback deployment when enabled: status=%d body=%s", rr.Code, rr.Body.String())
	}
	cap2.mu.Lock()
	calls, body := cap2.calls, string(append([]byte(nil), cap2.body...))
	cap2.mu.Unlock()
	if calls != 1 || !strings.Contains(body, `"model":"upstream-model-x"`) {
		t.Fatalf("unknown-model fallback did not call the fallback deployment with its configured model: calls=%d body=%s", calls, body)
	}
	// With fallback_on_unknown_model disabled, the same unknown-model
	// media request is a gateway-side 503 rejection.
	s3, _ := mediaGatewayNoFallback(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	rr = mediaPost(t, s3, "/v1/embeddings", "application/json",
		[]byte(`{"model":"nope","input":"hello"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown model with fallback off: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMediaFailoverToSecondDeployment(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	mkUpstream := func(id string, status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls[id]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
	}
	bad := mkUpstream("bad", http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	defer bad.Close()
	good := mkUpstream("good", http.StatusOK, `{"object":"list","data":[],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
	defer good.Close()

	cfg := config.Default()
	mk := func(id, url string, weight float64) config.ProviderConfig {
		p := config.ProviderConfig{
			ID: id, Name: id, Type: "openai_compatible", BaseURL: url,
			APIKey: "k", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Enabled: true, Weight: weight}},
		}
		p.ApplyDefaults()
		return p
	}
	cfg.Providers = []config.ProviderConfig{mk("bad", bad.URL, 100.0), mk("good", good.URL, 1.0)}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	rr := mediaPost(t, s, "/v1/embeddings", "application/json",
		[]byte(`{"model":"m","input":"hello"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("failover status=%d body=%s (calls=%v)", rr.Code, rr.Body.String(), calls)
	}
	if calls["bad"] != 1 || calls["good"] != 1 {
		t.Fatalf("expected one attempt per deployment, calls=%v", calls)
	}
}

// Regression: a non-binary media response was committed with the upstream
// status before its body was read. If the body read failed (truncated
// connection, oversized response), the handler returned a silent 200 with an
// empty body after already recording the deployment as healthy. Read the body
// before committing the client response and fail over / return an error.
func TestMediaEmbeddingsTruncatedUpstreamIsNotSilent200(t *testing.T) {
	s, _ := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"object\":"))
		_ = conn.Close()
	})
	rr := mediaPost(t, s, "/v1/embeddings", "application/json",
		[]byte(`{"model":"m","input":"hello"}`))
	if rr.Code == http.StatusOK {
		t.Fatalf("truncated upstream body returned silent 200 (body=%q); want a gateway error before headers commit", rr.Body.String())
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMediaSpeechTruncatedUpstreamIsNotSilent200(t *testing.T) {
	s, _ := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: audio/mpeg\r\nContent-Length: 4096\r\n\r\n\xff\xfb"))
		_ = conn.Close()
	})
	rr := mediaPost(t, s, "/v1/audio/speech", "application/json",
		[]byte(`{"model":"m","input":"hello","voice":"alloy"}`))
	if rr.Code == http.StatusOK {
		t.Fatalf("truncated audio returned silent 200 with %d bytes", rr.Body.Len())
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
}
