package livecanary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #646: the error_envelope scenario's JSON check is dialect
// independent code (it accepts any body carrying an "error", "message"
// or "Message" key), but only the OpenAI spellings were pinned through
// the real Runner path. These tests pin the native error bodies of the
// other dialects through that same path, using loopback fakes only.
// They pin envelope *parsing*, not dialect adapters: the adapter here
// is openai_compatible in every case, because the parsing code does
// not branch on dialect. No live provider call is made and no dialect
// adapter is claimed verified by this file.
func TestIssue646ErrorEnvelopeDialectBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		// OpenAI / Azure OpenAI shape (Azure errors use the OpenAI envelope).
		{"openai", `{"error":{"message":"bad request","type":"invalid_request_error","code":"invalid"}}`},
		// Anthropic shape: top-level type plus a nested error object.
		{"anthropic", `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`},
		// Gemini / Vertex shape: error object with numeric code and status.
		{"gemini", `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`},
		// Bedrock shape: __type plus a message string (no nested error object).
		{"bedrock", `{"__type":"com.amazonaws.bedrock#ValidationException","message":"bad request"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

			opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
			opts.Scenarios = []string{"error_envelope"}
			runner, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer runner.Close()

			report := runner.Run(context.Background())
			if len(report.Scenarios) != 1 {
				t.Fatalf("scenarios=%d, want 1", len(report.Scenarios))
			}
			res := report.Scenarios[0]
			if res.Status != StatusPass || res.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("body %s: status=%q http=%d error=%q, want PASS on 400",
					tc.body, res.Status, res.HTTPStatus, res.Error)
			}
		})
	}
}

// The negative side through the same real path: a 400 body with none
// of the envelope keys must FAIL, so a provider that returns bare
// detail/status JSON is never reported as having an error envelope.
func TestIssue646ErrorEnvelopeBareBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"bad request","status":"INVALID"}`))
	}))
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"error_envelope"}
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()

	report := runner.Run(context.Background())
	if len(report.Scenarios) != 1 {
		t.Fatalf("scenarios=%d, want 1", len(report.Scenarios))
	}
	res := report.Scenarios[0]
	if res.Status != StatusFail {
		t.Fatalf("status=%q, want FAIL for a body without an envelope key", res.Status)
	}
}
