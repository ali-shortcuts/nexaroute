package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

const logicalPaywallBody = `{"id":"chatcmpl-dead","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The account behind this API key doesn't have enough credits. Please top up (https://enter.pollinations.ai/top-up) or complete a quest, then try again."},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":40}}`

func paywallUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, logicalPaywallBody)
	}))
}

func anthropicBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
}

func postChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestLogicalError200PaywallFailsOverToHealthy(t *testing.T) {
	dead := paywallUpstream()
	defer dead.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, dead, solid, nil)
	before := s.hm.Get("flaky/m1")
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 200 {
		t.Fatalf("failover must produce 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("response must come from the healthy secondary: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "doesn't have enough credits") {
		t.Fatalf("paywall text must never be forwarded as a completion: %s", rr.Body.String())
	}
	st := s.hm.Get("flaky/m1")
	if st.Status == health.Healthy || st.Failures <= before.Failures {
		t.Fatalf("paywall deployment must record failure, not success: %+v", st)
	}
	if st.Successes != before.Successes {
		t.Fatalf("paywall deployment must record no new success: %+v", st)
	}
}

func TestLogicalError200PaywallSingleProviderIs502(t *testing.T) {
	dead := paywallUpstream()
	defer dead.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, dead, solid, func(c *config.Config) { c.Providers = c.Providers[:1] })
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 502 {
		t.Fatalf("exhausted paywall must be 502, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "quota") {
		t.Fatalf("client error must name the quota cause: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "doesn't have enough credits") {
		t.Fatalf("raw paywall text must not leak as the client body: %s", rr.Body.String())
	}
}

func TestLogicalError200ErrorEnvelopeFailsOver(t *testing.T) {
	envelope := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance","type":"unknown_error","code":"invalid_request_error"}}`)
	}))
	defer envelope.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, envelope, solid, nil)
	before := s.hm.Get("flaky/m1")
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("200 error envelope must fail over: %d %s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("flaky/m1"); st.Successes != before.Successes || st.Failures <= before.Failures {
		t.Fatalf("envelope deployment must record failure: %+v", st)
	}
}

func TestLogicalErrorFinishReasonErrorFailsOver(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"error"}]}`)
	}))
	defer broken.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, broken, solid, nil)
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("finish_reason error must fail over: %d %s", rr.Code, rr.Body.String())
	}
}

func TestLogicalErrorAnthropicIngressFailsOver(t *testing.T) {
	dead := paywallUpstream()
	defer dead.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, dead, solid, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/messages", strings.NewReader(anthropicBody("up-m1")))
	req.Header.Set("x-api-key", "nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("translated failover must produce 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("translated response must come from the secondary: %s", rr.Body.String())
	}
}

func sseStreamServer(chunks ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
	}))
}

func TestLogicalErrorStreamedPaywallDoesNotRecordSuccess(t *testing.T) {
	paywalled := sseStreamServer(
		`{"choices":[{"delta":{"role":"assistant","content":"The account behind this API key "},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"doesn't have enough credits. Please top up."},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	defer paywalled.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, paywalled, solid, nil)
	before := s.hm.Get("flaky/m1")
	streamBody := chatBody("up-m1")[:len(chatBody("up-m1"))-1] + `, "stream":true}`
	rr := postChat(t, s, streamBody)
	if rr.Code != 200 {
		t.Fatalf("committed stream returns 200, got %d", rr.Code)
	}
	st := s.hm.Get("flaky/m1")
	if st.Successes != before.Successes || st.Failures <= before.Failures {
		t.Fatalf("streamed paywall must record failure, not success: %+v", st)
	}
	found := false
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "stream_fail" && e.ErrorType == "provider_billing" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a stream_fail event typed provider_billing")
	}
}

func TestLogicalErrorStreamFinishReasonErrorRecordsFailure(t *testing.T) {
	broken := sseStreamServer(
		`{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"error"}]}`,
	)
	defer broken.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, broken, solid, nil)
	before := s.hm.Get("flaky/m1")
	streamBody := chatBody("up-m1")[:len(chatBody("up-m1"))-1] + `, "stream":true}`
	rr := postChat(t, s, streamBody)
	if rr.Code != 200 {
		t.Fatalf("committed stream returns 200, got %d", rr.Code)
	}
	if st := s.hm.Get("flaky/m1"); st.Successes != before.Successes || st.Failures <= before.Failures {
		t.Fatalf("finish_reason error stream must record failure: %+v", st)
	}
}

func TestLogicalError429QuotaEventTypeIsBilling(t *testing.T) {
	quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`)
	}))
	defer quota.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, quota, solid, nil)
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("quota 429 must fail over: %d %s", rr.Code, rr.Body.String())
	}
	found := false
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "route_fail" && e.Deployment == "flaky/m1" && e.ErrorType == "provider_billing" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a route_fail event typed provider_billing for insufficient_quota")
	}
}

func TestLogicalErrorNeverLeaksProviderKey(t *testing.T) {
	leaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		// Upstream echoes the credential inside its error text.
		fmt.Fprint(w, `{"error":{"message":"key sk-flaky has insufficient credits, top up now","code":"insufficient_credits"}}`)
	}))
	defer leaky.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, leaky, solid, func(c *config.Config) { c.Providers = c.Providers[:1] })
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 502 {
		t.Fatalf("expected 502, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "sk-flaky") {
		t.Fatalf("provider key leaked into client error: %s", rr.Body.String())
	}
	for _, e := range s.bus.Snapshot() {
		if strings.Contains(e.Message, "sk-flaky") {
			t.Fatalf("provider key leaked into event %q: %s", e.Kind, e.Message)
		}
	}
}
