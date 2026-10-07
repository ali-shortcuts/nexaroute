package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestVirtualKeyTPMRejectsBeforeProvider(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, VirtualKeys: []config.VirtualKeyConfig{{ID: "vk-tpm", KeyHash: keyDigest("nrk_tpm"), AllowedModels: []string{"m"}, TPM: 20}}}
	s := testGateway(t, cfg)
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer nrk_tpm")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s want 429", rr.Code, rr.Body.String())
	}
}
