package providers

import (
	"strings"
	"testing"
)

func TestClassifyUpstreamResponse(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantNil    bool
		wantClass  UpstreamErrorClass
		wantSubstr string
	}{
		// Genuine successes stay successes.
		{
			name:    "openai chat completion",
			status:  200,
			body:    `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
			wantNil: true,
		},
		{
			name:    "anthropic message",
			status:  200,
			body:    `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`,
			wantNil: true,
		},
		{
			name:   "completion discussing quota wording mid-text is clean",
			status: 200,
			body: `{"choices":[{"message":{"role":"assistant","content":"Sure, happy to explain those API concepts in detail for you here, ` +
				`starting with a general overview of how provider accounts and error messages work in practice. ` +
				`The phrase 'insufficient balance' means your account ran out of funds. ` +
				`Let me explain billing in detail and how top up flows usually work."},"finish_reason":"stop"}]}`,
			wantNil: true,
		},
		{
			name:    "top up your coffee is clean",
			status:  200,
			body:    `{"choices":[{"message":{"role":"assistant","content":"Top up your coffee and let's continue debugging."},"finish_reason":"stop"}]}`,
			wantNil: true,
		},
		{
			name:    "unknown extra success fields stay clean",
			status:  200,
			body:    `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"detail":"some future diagnostics","status":"complete"}`,
			wantNil: true,
		},
		// The reported Pollinations case: HTTP 200, paywall inside content.
		{
			name:       "pollinations 200 with paywall content",
			status:     200,
			body:       `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The account behind this API key doesn't have enough credits. Please top up (https://enter.pollinations.ai/top-up) or complete a quest, then try again."},"finish_reason":"stop"}]}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "quota",
		},
		{
			name:       "pollinations-style needs paid pollen",
			status:     200,
			body:       `{"choices":[{"message":{"role":"assistant","content":"This model needs paid Pollen. Free-tier keys cannot use it."},"finish_reason":"stop"}]}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "quota",
		},
		{
			name:       "200 error envelope object",
			status:     200,
			body:       `{"error":{"message":"Insufficient Balance","type":"unknown_error","code":"invalid_request_error"}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "Insufficient Balance",
		},
		{
			name:       "200 error envelope string (LM Studio style)",
			status:     200,
			body:       `{"error":"Unexpected endpoint or method. (POST /chat/completions)"}`,
			wantClass:  UpstreamNotFound,
			wantSubstr: "Unexpected endpoint",
		},
		{
			name:       "200 anthropic type error",
			status:     200,
			body:       `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantClass:  UpstreamOverloaded,
			wantSubstr: "Overloaded",
		},
		{
			name:       "200 success false proxy shape",
			status:     200,
			body:       `{"status":402,"success":false,"error":{"code":"INSUFFICIENT","message":"not enough credits"}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "not enough credits",
		},
		{
			name:       "200 finish_reason error",
			status:     200,
			body:       `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"error"}]}`,
			wantClass:  UpstreamServer,
			wantSubstr: "finish_reason",
		},
		{
			name:       "200 literal null",
			status:     200,
			body:       `null`,
			wantClass:  UpstreamBadPayload,
			wantSubstr: "null",
		},
		{
			name:       "200 empty body",
			status:     200,
			body:       `   `,
			wantClass:  UpstreamBadPayload,
			wantSubstr: "empty",
		},
		{
			name:       "200 HTML body",
			status:     200,
			body:       `<html><body>Bad Gateway</body></html>`,
			wantClass:  UpstreamBadPayload,
			wantSubstr: "HTML",
		},
		{
			name:       "200 malformed JSON",
			status:     200,
			body:       `{"choices":[{`,
			wantClass:  UpstreamBadPayload,
			wantSubstr: "malformed",
		},
		{
			name:       "openai 429 insufficient_quota is quota not throttle",
			status:     429,
			body:       `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "quota",
		},
		{
			name:       "openai 429 rate limit reached stays throttle",
			status:     429,
			body:       `{"error":{"message":"Rate limit reached for gpt-4o. Limit 10000, please try again in 1s.","type":"requests","code":"rate_limit_exceeded"}}`,
			wantClass:  UpstreamRateLimit,
			wantSubstr: "Rate limit",
		},
		{
			name:       "openai 401 incorrect key",
			status:     401,
			body:       `{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","code":"invalid_api_key"}}`,
			wantClass:  UpstreamAuth,
			wantSubstr: "Incorrect API key",
		},
		{
			name:       "openai 404 model not found",
			status:     404,
			body:       `{"error":{"message":"The model 'gpt-9' does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			wantClass:  UpstreamNotFound,
			wantSubstr: "does not exist",
		},
		{
			name:       "anthropic 529 overloaded",
			status:     529,
			body:       `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantClass:  UpstreamOverloaded,
			wantSubstr: "Overloaded",
		},
		{
			name:       "anthropic 401 invalid key",
			status:     401,
			body:       `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantClass:  UpstreamAuth,
			wantSubstr: "invalid x-api-key",
		},
		{
			name:       "openrouter 402 no credits",
			status:     402,
			body:       `{"error":{"message":"Insufficient credits. This account never purchased credits.","code":402}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "Insufficient credits",
		},
		{
			name:       "openrouter 402 needs more credits",
			status:     402,
			body:       `{"error":{"code":402,"message":"This request requires more credits, or fewer max_tokens."}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "requires more credits",
		},
		{
			name:       "deepseek 402 insufficient balance",
			status:     402,
			body:       `{"error":{"message":"Insufficient Balance","type":"unknown_error","code":"invalid_request_error"}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "Insufficient Balance",
		},
		{
			name:       "xai-style 402 usage balance exhausted",
			status:     402,
			body:       `{"error":{"message":"usage balance exhausted","code":"insufficient_credits"}}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "balance exhausted",
		},
		{
			name:       "503 plain text stays server",
			status:     503,
			body:       `Service Unavailable`,
			wantClass:  UpstreamServer,
			wantSubstr: "Service Unavailable",
		},
		{
			name:       "500 empty body stays server",
			status:     500,
			body:       ``,
			wantClass:  UpstreamServer,
			wantSubstr: "http 500",
		},
		{
			name:       "400 invalid request",
			status:     400,
			body:       `{"error":{"message":"messages: roles must alternate","type":"invalid_request_error"}}`,
			wantClass:  UpstreamInvalid,
			wantSubstr: "roles must alternate",
		},
		{
			name:       "fastapi detail shape",
			status:     422,
			body:       `{"detail":"API key is invalid"}`,
			wantClass:  UpstreamAuth,
			wantSubstr: "invalid",
		},
		{
			name:       "anthropic content paywall",
			status:     200,
			body:       `{"type":"message","role":"assistant","content":[{"type":"text","text":"Out of credits. Please add funds to continue."}],"stop_reason":"end_turn"}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "quota",
		},
		{
			name:       "content part array paywall",
			status:     200,
			body:       `{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"Quota exceeded for this key."}]},"finish_reason":"stop"}]}`,
			wantClass:  UpstreamQuota,
			wantSubstr: "quota",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyUpstreamResponse(tc.status, []byte(tc.body))
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected class %q, got nil", tc.wantClass)
			}
			if got.Class != tc.wantClass {
				t.Fatalf("expected class %q, got %q (%v)", tc.wantClass, got.Class, got)
			}
			if got.Status != tc.status {
				t.Fatalf("expected status %d, got %d", tc.status, got.Status)
			}
			if !strings.Contains(got.Error(), tc.wantSubstr) {
				t.Fatalf("error %q should contain %q", got.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestClassifySSEData(t *testing.T) {
	cases := []struct {
		name      string
		protocol  string
		data      string
		wantNil   bool
		wantClass UpstreamErrorClass
	}{
		{name: "done marker", protocol: "openai", data: "[DONE]", wantNil: true},
		{name: "clean delta", protocol: "openai", data: `{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`, wantNil: true},
		{name: "clean terminal", protocol: "openai", data: `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, wantNil: true},
		{name: "garbage stays nil", protocol: "openai", data: `not json`, wantNil: true},
		{
			name: "error chunk quota", protocol: "openai",
			data:      `{"error":{"message":"Insufficient credits.","code":402}}`,
			wantClass: UpstreamQuota,
		},
		{
			name: "finish_reason error", protocol: "openai",
			data:      `{"choices":[{"delta":{},"finish_reason":"error"}]}`,
			wantClass: UpstreamServer,
		},
		{
			name: "anthropic error event", protocol: "anthropic",
			data:      `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantClass: UpstreamOverloaded,
		},
		{
			name: "anthropic stop_reason error", protocol: "anthropic",
			data:      `{"type":"message_delta","delta":{"stop_reason":"error"}}`,
			wantClass: UpstreamServer,
		},
		{name: "anthropic text delta clean", protocol: "anthropic", data: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifySSEData(tc.protocol, []byte(tc.data))
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected class %q, got nil", tc.wantClass)
			}
			if got.Class != tc.wantClass {
				t.Fatalf("expected class %q, got %q", tc.wantClass, got.Class)
			}
		})
	}
}

func TestStreamContentSniffer(t *testing.T) {
	var s StreamContentSniffer
	s.Add("The account behind this API key ")
	s.Add("doesn't have enough credits. Please top up.")
	if got := s.Sniff(); got != UpstreamQuota {
		t.Fatalf("expected quota, got %q", got)
	}
	var clean StreamContentSniffer
	clean.Add("Hello! ")
	clean.Add("How can I help?")
	if got := clean.Sniff(); got != UpstreamOK {
		t.Fatalf("expected clean, got %q", got)
	}
	// Late discussion of quota wording must not match: only the head counts.
	var late StreamContentSniffer
	late.Add(strings.Repeat("lorem ipsum dolor sit amet. ", 20))
	late.Add("by the way, insufficient balance means no funds.")
	if got := late.Sniff(); got != UpstreamOK {
		t.Fatalf("expected clean for late mention, got %q", got)
	}
}

func TestUpstreamErrorClassKeyScoped(t *testing.T) {
	for class, want := range map[UpstreamErrorClass]bool{
		UpstreamAuth: true, UpstreamQuota: true, UpstreamRateLimit: true,
		UpstreamNotFound: false, UpstreamInvalid: false,
		UpstreamOverloaded: false, UpstreamServer: false, UpstreamBadPayload: false,
	} {
		if got := class.KeyScoped(); got != want {
			t.Fatalf("class %q KeyScoped=%v, want %v", class, got, want)
		}
	}
	if UpstreamQuota.CooldownStatus() != 402 || UpstreamRateLimit.CooldownStatus() != 429 || UpstreamAuth.CooldownStatus() != 401 {
		t.Fatal("cooldown status mapping wrong")
	}
}

func FuzzClassifyUpstreamNeverPanics(f *testing.F) {
	f.Add(200, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	f.Add(429, `{"error":{"message":"You exceeded your current quota","code":"insufficient_quota"}}`)
	f.Add(200, `{"error":"boom"}`)
	f.Fuzz(func(t *testing.T, status int, body string) {
		if status < 0 || status > 999 {
			return
		}
		_ = ClassifyUpstreamResponse(status, []byte(body))
		_ = ClassifySSEData("openai", []byte(body))
		_ = ClassifySSEData("anthropic", []byte(body))
		var s StreamContentSniffer
		s.Add(body)
		_ = s.Sniff()
	})
}
