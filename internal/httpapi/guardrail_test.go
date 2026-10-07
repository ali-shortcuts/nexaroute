package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestGuardrailModes(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	for _, tc := range []struct {
		name, mode string
		want       int
	}{
		{"block", "block", http.StatusForbidden},
		{"audit", "audit", http.StatusOK},
		{"off", "off", http.StatusOK},
	} {
		cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
		cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"sk-guardrail-12345678"}}
		cfg.Guardrails = config.GuardrailConfig{Mode: tc.mode, RejectPII: true, RejectSecrets: true, RejectPromptInjection: true, MaxBodyBytes: 2 << 20}
		s := testGateway(t, cfg)
		body := `{"model":"m","messages":[{"role":"user","content":"contact me at test@example.com"}]}`
		r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-guardrail-12345678")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != tc.want {
			t.Fatalf("%s status=%d body=%s want %d", tc.name, rr.Code, rr.Body.String(), tc.want)
		}
	}
}
