package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
)

func wave4PlannerServer(t *testing.T, status int, body string, check func(r *http.Request, raw []byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if check != nil {
			check(r, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

type wave4ErrReader struct{ err error }

func (r wave4ErrReader) Read(_ []byte) (int, error) { return 0, r.err }

type wave4RoundTripper struct {
	status int
	body   io.ReadCloser
	err    error
}

func (rt wave4RoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	if rt.err != nil {
		return nil, rt.err
	}
	return &http.Response{
		StatusCode: rt.status,
		Body:       rt.body,
		Header:     make(http.Header),
	}, nil
}

// Timeout <= 0 falls back to the 120s default instead of failing: a planner
// built with Timeout 0 still completes against a live endpoint.
func TestWave4PlannerZeroTimeoutDefaults(t *testing.T) {
	srv := wave4PlannerServer(t, http.StatusOK, `{"choices":[{"message":{"content":"ok-plan"}}]}`, nil)
	defer srv.Close()
	p := &agent.OpenAIPlanner{BaseURL: srv.URL, Timeout: 0}
	out, err := p.Complete(context.Background(), "m", "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "ok-plan" {
		t.Fatalf("out = %q, want ok-plan", out)
	}
}

// JSONMode sends response_format json_object and the endpoint's JSON reply
// is returned verbatim.
func TestWave4PlannerJSONModeSendsResponseFormat(t *testing.T) {
	var gotFormat string
	srv := wave4PlannerServer(t, http.StatusOK, `{"choices":[{"message":{"content":"{\"a\":1}"}}]}`, func(_ *http.Request, raw []byte) {
		var body struct {
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request JSON: %v", err)
			return
		}
		if body.ResponseFormat != nil {
			gotFormat = body.ResponseFormat.Type
		}
	})
	defer srv.Close()
	p := &agent.OpenAIPlanner{BaseURL: srv.URL, JSONMode: true}
	out, err := p.Complete(context.Background(), "m", "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotFormat != "json_object" {
		t.Fatalf("response_format = %q, want json_object", gotFormat)
	}
	if out != `{"a":1}` {
		t.Fatalf("out = %q, want the JSON payload", out)
	}
}

// A key variable that becomes blank (whitespace-only) after construction
// fails closed at call time instead of sending an empty bearer token.
func TestWave4PlannerBlankKeyAtCallTimeFailsClosed(t *testing.T) {
	srv := wave4PlannerServer(t, http.StatusOK, `{"choices":[{"message":{"content":"x"}}]}`, func(r *http.Request, _ []byte) {
		t.Errorf("endpoint must not be reached, got Authorization %q", r.Header.Get("Authorization"))
	})
	defer srv.Close()
	t.Setenv("WAVE4_PLANNER_BLANK_KEY", "   ")
	p := &agent.OpenAIPlanner{BaseURL: srv.URL, APIKeyEnv: "WAVE4_PLANNER_BLANK_KEY"}
	if _, err := p.Complete(context.Background(), "m", "s", "u"); err == nil || !strings.Contains(err.Error(), "WAVE4_PLANNER_BLANK_KEY") {
		t.Fatalf("err = %v, want a missing-key error naming the variable", err)
	}
}

// A transport failure (server gone) is wrapped as a planner request failure.
func TestWave4PlannerTransportErrorWrapped(t *testing.T) {
	srv := wave4PlannerServer(t, http.StatusOK, `{"choices":[{"message":{"content":"x"}}]}`, nil)
	srv.Close()
	p := &agent.OpenAIPlanner{BaseURL: srv.URL}
	if _, err := p.Complete(context.Background(), "m", "s", "u"); err == nil || !strings.Contains(err.Error(), "planner request failed") {
		t.Fatalf("err = %v, want a wrapped request failure", err)
	}
}

// A body that errors mid-read is reported as a read failure, not as a
// successful empty response.
func TestWave4PlannerReadErrorWrapped(t *testing.T) {
	boom := errors.New("read exploded")
	p := &agent.OpenAIPlanner{
		BaseURL: "http://127.0.0.1:9",
		Client: &http.Client{Transport: wave4RoundTripper{
			status: http.StatusOK,
			body:   io.NopCloser(wave4ErrReader{boom}),
		}},
	}
	if _, err := p.Complete(context.Background(), "m", "s", "u"); err == nil || !strings.Contains(err.Error(), "read planner response") || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrapped read failure", err)
	}
}

// A long upstream error body is truncated to 300 characters in the error:
// the head survives, the tail must not.
func TestWave4PlannerErrorSnippetTruncated(t *testing.T) {
	long := strings.Repeat("z", 500)
	srv := wave4PlannerServer(t, http.StatusBadGateway, long, nil)
	defer srv.Close()
	p := &agent.OpenAIPlanner{BaseURL: srv.URL}
	_, err := p.Complete(context.Background(), "m", "s", "u")
	if err == nil {
		t.Fatal("expected an error for HTTP 502")
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %q, want the status code", err)
	}
	if !strings.Contains(err.Error(), strings.Repeat("z", 300)) {
		t.Fatalf("err must carry the first 300 chars of the body: %q", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("z", 301)) {
		t.Fatalf("err must not carry more than 300 chars of the body: %q", err)
	}
}

// A 200 response that is not chat-completions JSON is rejected.
func TestWave4PlannerInvalidJSONRejected(t *testing.T) {
	srv := wave4PlannerServer(t, http.StatusOK, `not json at all`, nil)
	defer srv.Close()
	p := &agent.OpenAIPlanner{BaseURL: srv.URL}
	if _, err := p.Complete(context.Background(), "m", "s", "u"); err == nil || !strings.Contains(err.Error(), "not valid chat-completions JSON") {
		t.Fatalf("err = %v, want an invalid-JSON error", err)
	}
}

// Empty choices and whitespace-only content are both "no message content".
func TestWave4PlannerNoContentRejected(t *testing.T) {
	for _, body := range []string{
		`{"choices":[]}`,
		`{"choices":[{"message":{"content":"   "}}]}`,
	} {
		srv := wave4PlannerServer(t, http.StatusOK, body, nil)
		p := &agent.OpenAIPlanner{BaseURL: srv.URL}
		_, err := p.Complete(context.Background(), "m", "s", "u")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "no message content") {
			t.Fatalf("body %s: err = %v, want no-message-content", body, err)
		}
	}
}
