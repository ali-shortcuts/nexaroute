package httpapi

import (
	"net/http"
	"runtime"
)

const gatewayVersion = "0.3"

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "Universal LLM Gateway",
		"version": gatewayVersion,
		"go":      runtime.Version(),
		"protocols": map[string]any{
			"ingress":  []string{"anthropic_messages", "openai_chat_completions"},
			"upstream": []string{"anthropic_compatible", "openai_compatible"},
		},
		"endpoints": []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/models", "/healthz", "/readyz", "/metrics"},
	})
}
