package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestRuntimeVersionAndProtocolSurfaceMatchV060(t *testing.T) {
	if gatewayVersion != "0.6.0" {
		t.Fatalf("gatewayVersion=%q want 0.6.0", gatewayVersion)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/version", nil)
	(&Server{}).hello(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var got struct {
		Version   string `json:"version"`
		Protocols struct {
			Ingress  []string `json:"ingress"`
			Upstream []string `json:"upstream"`
		} `json:"protocols"`
		Endpoints []string `json:"endpoints"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.6.0" {
		t.Fatalf("/version reports %q want 0.6.0", got.Version)
	}
	for _, want := range []string{"anthropic_messages", "openai_chat_completions", "openai_responses"} {
		if !containsString(got.Protocols.Ingress, want) {
			t.Fatalf("ingress protocols missing %q: %#v", want, got.Protocols.Ingress)
		}
	}
	for _, want := range []string{"anthropic_compatible", "openai_compatible", "openai_responses", "gemini"} {
		if !containsString(got.Protocols.Upstream, want) {
			t.Fatalf("upstream protocols missing %q: %#v", want, got.Protocols.Upstream)
		}
	}
	if !containsString(got.Endpoints, "/v1/responses") {
		t.Fatalf("runtime endpoints missing /v1/responses: %#v", got.Endpoints)
	}
}
