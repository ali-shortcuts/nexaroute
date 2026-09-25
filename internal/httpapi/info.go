package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/buildinfo"
	"net/http"
	"runtime"
)

const gatewayVersion = buildinfo.Version

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "NexaRoute",
		"version": gatewayVersion,
		"go":      runtime.Version(),
		"protocols": map[string]any{
			"ingress":  []string{"anthropic_messages", "openai_chat_completions", "openai_responses"},
			"upstream": []string{"anthropic_compatible", "openai_compatible", "openai_responses", "gemini"},
		},
		"endpoints": []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses", "/v1/models", "/healthz", "/readyz", "/metrics"},
	})
}
