package livecanary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Issue #646: the auth_failure scenario's status rule (400/401/403 pass,
// 200 fails) was previously pinned only through the openai_compatible
// adapter, and PR #773 pinned error-envelope *bodies* per dialect through
// that same adapter, explicitly noting that parsing does not branch on
// dialect. What was not pinned is the auth_failure path itself per
// dialect: each dialect sends its credential under a different header
// (Bearer, x-api-key, x-goog-api-key, api-key, SigV4 Credential), hits a
// different path (Anthropic /v1/messages, Gemini /v1beta/models/...,
// Vertex projects/locations/..., Azure deployments/..., Bedrock
// /model/.../converse), and is rejected with a different native status
// (Gemini 400, the rest 401/403). These tests pin that whole per-dialect
// path through the real Runner, using loopback fakes only.
//
// Honest scope, stated in each case: the fake does not verify a SigV4
// signature and does not verify any provider's real rejection behaviour.
// A PASS here means only that the Runner sent the bogus credential in
// the dialect's shape, the fake rejected it with the dialect's native
// status, and the Runner recorded PASS with exactly one call and no
// credential material in the report. It is FIXTURE evidence, not LIVE
// acceptance for any provider. No live provider call is made.

const issue646BogusCredential = "nexaroute-acceptance-invalid-credential"

type issue646AuthFake struct {
	hits   atomic.Int64
	mu     sync.Mutex
	paths  []string
	header []string
	// check reports whether the request carried the bogus credential in
	// the dialect's shape. If it does not, the fake answers 200, which
	// must make auth_failure FAIL (a credential that was not actually
	// sent bogus cannot be claimed rejected).
	check  func(r *http.Request) bool
	status int
	body   string
}

func (f *issue646AuthFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.RequestURI())
	var headers []string
	for name, values := range r.Header {
		for _, v := range values {
			headers = append(headers, name+": "+v)
		}
	}
	f.header = append(f.header, strings.Join(headers, "; "))
	f.mu.Unlock()
	if f.check != nil && f.check(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"chatcmpl-canary","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
}

func TestIssue646AuthFailurePerDialect(t *testing.T) {
	cases := []struct {
		name     string
		dialect  string
		model    string
		status   int
		body     string
		check    func(r *http.Request) bool
		pathWant string
	}{
		{
			name:    "openai_compatible",
			dialect: "openai_compatible",
			model:   "canary-model",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"type":"invalid_request_error","message":"invalid api key"}}`,
			check: func(r *http.Request) bool {
				return r.Header.Get("Authorization") == "Bearer "+issue646BogusCredential
			},
		},
		{
			name:    "anthropic_compatible",
			dialect: "anthropic_compatible",
			model:   "canary-model",
			status:  http.StatusUnauthorized,
			body:    `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			check: func(r *http.Request) bool {
				return r.Header.Get("x-api-key") == issue646BogusCredential
			},
			pathWant: "/v1/messages",
		},
		{
			name:    "gemini",
			dialect: "gemini",
			model:   "canary-model",
			status:  http.StatusBadRequest,
			body:    `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`,
			check: func(r *http.Request) bool {
				return r.Header.Get("x-goog-api-key") == issue646BogusCredential
			},
			pathWant: "/v1beta/models/canary-model",
		},
		{
			name:    "vertex",
			dialect: "vertex",
			model:   "canary-model",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"code":401,"message":"Request had invalid authentication credentials","status":"UNAUTHENTICATED"}}`,
			check: func(r *http.Request) bool {
				return r.Header.Get("Authorization") == "Bearer "+issue646BogusCredential
			},
			pathWant: "/v1/projects/canary-project/locations/us-central1/publishers/google/models/canary-model",
		},
		{
			name:    "azure_openai",
			dialect: "azure_openai",
			model:   "canary-model",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`,
			check: func(r *http.Request) bool {
				return r.Header.Get("api-key") == issue646BogusCredential
			},
			pathWant: "/openai/deployments/canary-model/chat/completions",
		},
		{
			name:    "bedrock",
			dialect: "bedrock",
			model:   "anthropic.claude-3-sonnet-20240229-v1:0",
			status:  http.StatusForbidden,
			body:    `{"__type":"com.amazon.bedrock#AccessDeniedException","message":"The security token included in the request is invalid."}`,
			check: func(r *http.Request) bool {
				// SigV4 only: the fake cannot verify the signature, so
				// it checks only that the bogus access-key id is the
				// credential on the wire. The real test credential must
				// never appear here (asserted below).
				return strings.Contains(r.Header.Get("Authorization"), "Credential="+issue646BogusCredential+"/")
			},
			pathWant: "/model/anthropic.claude-3-sonnet-20240229-v1%3A0/converse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &issue646AuthFake{check: tc.check, status: tc.status, body: tc.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
			t.Setenv("NEXA_CANARY_BEDROCK_SECRET", "canary-test-secret-5678")

			provider := testProvider(srv.URL, "NEXA_CANARY_KEY")
			provider.Type = tc.dialect
			// Anthropic/Gemini/Azure: leave AuthMode empty so the
			// adapter's dialect default (x-api-key / x-goog-api-key /
			// api-key) is what the test pins, rather than the
			// testProvider's bearer default. OpenAI/Vertex keep
			// bearer, which is also their default.
			if tc.dialect == "anthropic_compatible" || tc.dialect == "gemini" || tc.dialect == "azure_openai" {
				provider.AuthMode = ""
			}
			if tc.dialect == "vertex" {
				provider.VertexProject = "canary-project"
				provider.VertexLocation = "us-central1"
			}
			if tc.dialect == "bedrock" {
				provider.SecretKeyEnv = "NEXA_CANARY_BEDROCK_SECRET"
				provider.AWSRegion = "us-east-1"
			}
			opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
			opts.Provider = provider
			opts.Model = config.ModelConfig{ID: "m", Model: tc.model, InputCostPerMTok: 1, OutputCostPerMTok: 2}
			opts.Scenarios = []string{"auth_failure"}
			opts.MaxCalls = 1
			opts.CallTimeout = 5 * time.Second
			opts.OverallTimeout = 10 * time.Second
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
			if res.ID != "auth_failure" || res.Status != StatusPass || res.HTTPStatus != tc.status {
				t.Fatalf("auth_failure=%+v, want PASS on http %d", res, tc.status)
			}
			if fake.hits.Load() != 1 {
				t.Fatalf("upstream hits=%d, want exactly 1 (a rejected credential must not be retried)", fake.hits.Load())
			}
			fake.mu.Lock()
			paths := append([]string(nil), fake.paths...)
			headers := append([]string(nil), fake.header...)
			fake.mu.Unlock()
			if tc.pathWant != "" {
				found := false
				for _, p := range paths {
					if strings.Contains(p, tc.pathWant) {
						found = true
					}
				}
				if !found {
					t.Fatalf("paths=%v, want one containing %q", paths, tc.pathWant)
				}
			}
			if tc.dialect == "azure_openai" {
				found := false
				for _, p := range paths {
					if strings.Contains(p, "api-version=") {
						found = true
					}
				}
				if !found {
					t.Fatalf("azure paths=%v, want an api-version query", paths)
				}
			}
			joined := strings.Join(headers, "\n")
			if strings.Contains(joined, "canary-test-secret-1234") || strings.Contains(joined, "canary-test-secret-5678") {
				t.Fatalf("fake saw a real test credential in headers; the bogus credential must be the only credential on the wire")
			}
			summary := report.Summary()
			if strings.Contains(summary, issue646BogusCredential) || strings.Contains(summary, "canary-test-secret-1234") || strings.Contains(summary, "canary-test-secret-5678") {
				t.Fatalf("report summary leaks credential material: %q", summary)
			}
		})
	}
}

// A 200 answer to the bogus credential must FAIL auth_failure for a
// dialect whose native rejection is 400, not only for the OpenAI 401
// case already pinned elsewhere: acceptance of the bogus credential is
// never evidence of rejection, whatever the dialect.
func TestIssue646AuthFailure200NeverPassesForGemini(t *testing.T) {
	fake := &issue646AuthFake{
		check:  func(r *http.Request) bool { return false },
		status: http.StatusBadRequest,
		body:   `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")
	provider := testProvider(srv.URL, "NEXA_CANARY_KEY")
	provider.Type = "gemini"
	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Provider = provider
	opts.Model = config.ModelConfig{ID: "m", Model: "canary-model", InputCostPerMTok: 1, OutputCostPerMTok: 2}
	opts.Scenarios = []string{"auth_failure"}
	opts.MaxCalls = 1
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if len(report.Scenarios) != 1 || report.Scenarios[0].Status != StatusFail || report.Scenarios[0].HTTPStatus != http.StatusOK {
		t.Fatalf("scenarios=%+v, want one FAIL on http 200", report.Scenarios)
	}
	if fake.hits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want exactly 1", fake.hits.Load())
	}
}
