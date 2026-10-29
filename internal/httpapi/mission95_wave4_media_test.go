package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave4MediaGateway(t *testing.T, cfg config.Config, handler http.HandlerFunc) (*Server, *mediaCapture) {
	t.Helper()
	cap := &mediaCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	cfg.Providers = []config.ProviderConfig{{
		ID: "mp", Name: "Media", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model-x", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, cap
}

func wave4MediaVEGateway(t *testing.T, enabled bool, privacy string, noFallback bool) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.FallbackOnUnknownModel = !noFallback
	cfg.Providers = []config.ProviderConfig{{
		ID: "mp", Name: "Media", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, DataHandling: config.DataHandlingConfig{TrainsOnData: "yes"},
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model-x", Enabled: true, Weight: 1}},
	}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Name: "Pool", Mode: "explicit", Deployments: []string{"mp/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool", Privacy: privacy}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding", PublicModel: "coding", RouteProfile: "profile", Enabled: &enabled, Protocols: []string{"openai"}}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

// A disabled virtual endpoint is a 404, not a generic 400.
func TestWave4MediaDisabledEndpoint(t *testing.T) {
	s := wave4MediaVEGateway(t, false, "", false)
	rr := mediaPost(t, s, "/v1/embeddings", "application/json", []byte(`{"model":"coding","input":"hello"}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled VE status=%d body=%s, want 404", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("body=%s, want the disabled reason", rr.Body.String())
	}
}

// Privacy is the sole cause of an empty candidate set: the provider
// trains on data while the route profile demands no training.
func TestWave4MediaPrivacyUnavailable(t *testing.T) {
	s := wave4MediaVEGateway(t, true, "no_training", false)
	rr := mediaPost(t, s, "/v1/embeddings", "application/json", []byte(`{"model":"coding","input":"hello"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("privacy status=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no deployment satisfies the required privacy class") {
		t.Fatalf("body=%s, want the privacy message", rr.Body.String())
	}
}

// Providers without a media equivalent refuse explicitly instead of
// having their payload mangled into a guessed URL.
func TestWave4MediaUnsupportedProvider(t *testing.T) {
	s, cap := mediaGateway(t, "anthropic_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	rr := mediaPost(t, s, "/v1/embeddings", "application/json", []byte(`{"model":"m","input":"hello"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unsupported status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "supported only for openai_compatible and azure_openai") {
		t.Fatalf("body=%s, want the explicit refusal", rr.Body.String())
	}
	if cap.calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", cap.calls)
	}
}

// With unknown-model fallback disabled, a virtual-endpoint media
// request drops every candidate at the per-attempt eligibility re-check
// (the public model is not a deployment model), so no upstream attempt
// is made and the gateway reports no compatible healthy deployment.
func TestWave4MediaVECandidateDroppedAtRecheck(t *testing.T) {
	s := wave4MediaVEGateway(t, true, "", true)
	rr := mediaPost(t, s, "/v1/embeddings", "application/json", []byte(`{"model":"coding","input":"hello"}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("dropped status=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("body=%s, want the no-deployment message", rr.Body.String())
	}
}

// A truncated binary upstream body (declared length never fulfilled)
// is a per-candidate failure, not a committed 200: the gateway answers
// 502 and never writes the partial bytes.
func TestWave4MediaTruncatedBinaryUpstream(t *testing.T) {
	s, _ := wave4MediaGateway(t, config.Default(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte{0x49, 0x44, 0x33, 0x00, 0xFF})
	})
	rr := mediaPost(t, s, "/v1/audio/speech", "application/json", []byte(`{"model":"m","input":"say it","voice":"alloy"}`))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("truncated status=%d body=%s, want 502", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unreadable") {
		t.Fatalf("body=%s, want the unreadable note", rr.Body.String())
	}
}

// Model extraction stops at the model part, but the rewrite walks the
// whole body: a malformed part after the model field fails the rewrite
// with a 400 before any upstream attempt.
func TestWave4MediaMultipartRewriteFailure(t *testing.T) {
	s, cap := mediaGateway(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"transcribed"}`)
	})
	boundary := "wave4media"
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		"m\r\n" +
		"--" + boundary + "\r\n" +
		"Bad Header Without Colon\r\n\r\n" +
		"xxx\r\n" +
		"--" + boundary + "--\r\n"
	rr := mediaPost(t, s, "/v1/audio/transcriptions", "multipart/form-data; boundary="+boundary, []byte(body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("rewrite failure status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "malformed MIME header") {
		t.Fatalf("body=%s, want the malformed-header note", rr.Body.String())
	}
	if cap.calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", cap.calls)
	}
}
