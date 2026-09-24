package errors

import "testing"

// Fixture matrix across provider error shapes. No API keys needed; every
// case is a mocked upstream response. See spec section 25 (subset: error
// classification across provider dialects).
func TestProviderErrorMatrix(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		class    Class
		health   bool
		failover bool
	}{
		{"openai-temperature", 400, `{"error":{"message":"temperature is not supported","type":"invalid_request_error","code":"unsupported_parameter"}}`, UnsupportedParameter, false, false},
		{"anthropic-temperature", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"temperature: Extra inputs are not permitted"}}`, UnsupportedParameter, false, false},
		{"nvidia-reasoning", 400, `{"detail":"reasoning_effort is not supported by this model"}`, UnsupportedParameter, false, false},
		{"deepseek-stream-options", 400, `{"error":{"message":"Unrecognized request argument supplied: stream_options"}}`, UnsupportedParameter, false, false},
		{"openrouter-tools", 400, `{"error":{"message":"This model does not support tool calling","code":400}}`, UnsupportedToolCalling, false, false},
		{"gemini-vision", 400, `{"error":{"message":"Vision input is not supported for this model"}}`, UnsupportedVision, false, false},
		{"generic-max-completion", 400, `{"error":{"message":"max_completion_tokens unsupported, use max_tokens"}}`, UnsupportedParameter, false, false},
		{"openai-context", 400, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, ContextOverflow, false, false},
		{"anthropic-context", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 90000 tokens > 200000"}}`, ContextOverflow, false, false},
		{"openai-auth", 401, `{"error":{"message":"Incorrect API key provided"}}`, InvalidKey, true, true},
		{"anthropic-auth", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, InvalidKey, true, true},
		{"openai-model404", 404, `{"error":{"message":"The model 'zzz' does not exist","code":"model_not_found"}}`, ModelNotFound, true, true},
		{"generic-endpoint404", 404, `{"error":"not found"}`, EndpointNotFound, true, true},
		{"openai-ratelimit", 429, `{"error":{"message":"Rate limit reached"}}`, RateLimit, true, true},
		{"anthropic-overload", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, UpstreamOverload, true, true},
		{"generic-500", 500, `internal server error`, UpstreamInternalError, true, true},
		{"generic-timeout", 0, `context deadline exceeded`, Timeout, true, true},
		{"generic-network", 0, `connection reset by peer`, NetworkError, true, true},
		{"openai-billing", 402, `{"error":{"message":"quota exceeded, check billing"}}`, QuotaExhausted, true, true},
		{"generic-caller-invalid", 400, `{"error":{"message":"messages: field required"}}`, InvalidRequestSchema, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Classify(tc.status, []byte(tc.body))
			if r.Class != tc.class {
				t.Fatalf("class = %s, want %s (msg=%q)", r.Class, tc.class, r.Message)
			}
			if r.AffectsHealth != tc.health {
				t.Fatalf("affectsHealth = %v, want %v", r.AffectsHealth, tc.health)
			}
			if r.Failover != tc.failover {
				t.Fatalf("failover = %v, want %v", r.Failover, tc.failover)
			}
		})
	}
}
