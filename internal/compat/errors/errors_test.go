package errors

import "testing"

func TestClassifyTemperatureUnsupported(t *testing.T) {
	body := []byte(`{"error":{"message":"temperature is not supported","type":"invalid_request_error"}}`)
	r := Classify(400, body)
	if r.Class != UnsupportedParameter {
		t.Fatalf("class = %s, want UNSUPPORTED_PARAMETER", r.Class)
	}
	if r.Parameter != "temperature" {
		t.Fatalf("parameter = %q, want temperature", r.Parameter)
	}
	if r.Capability != "temperature" {
		t.Fatalf("capability = %q, want temperature", r.Capability)
	}
	if r.AffectsHealth || r.AffectsProvider || r.Failover {
		t.Fatalf("capability failure must be health-neutral: %+v", r)
	}
	if !r.RetryableRepair {
		t.Fatalf("temperature failure should be repairable: %+v", r)
	}
}

func TestClassifyUnknownParameter(t *testing.T) {
	r := Classify(400, []byte(`{"error":{"message":"Unknown parameter: reasoning_effort"}}`))
	if r.Class != UnsupportedParameter || r.Parameter != "reasoning_effort" {
		t.Fatalf("got %+v", r)
	}
}

func TestClassifyAuthStaysProviderScoped(t *testing.T) {
	r := Classify(401, []byte(`{"error":{"message":"invalid API key"}}`))
	if r.Class != InvalidKey {
		t.Fatalf("class = %s", r.Class)
	}
	if !r.AffectsHealth || !r.AffectsProvider || !r.Failover {
		t.Fatalf("auth failure must fail over + signal provider: %+v", r)
	}
}

func TestClassifyModelNotFound(t *testing.T) {
	r := Classify(404, []byte(`{"error":{"message":"model not found"}}`))
	if r.Class != ModelNotFound || !r.AffectsHealth || !r.Failover {
		t.Fatalf("got %+v", r)
	}
	if r.AffectsProvider {
		t.Fatalf("model 404 must stay deployment-scoped: %+v", r)
	}
}

func TestClassifyRateLimitAndQuota(t *testing.T) {
	r := Classify(429, []byte(`{"error":{"message":"rate limit exceeded"}}`))
	if r.Class != RateLimit {
		t.Fatalf("class = %s", r.Class)
	}
	r = Classify(402, []byte(`{"error":{"message":"quota exhausted"}}`))
	if r.Class != QuotaExhausted {
		t.Fatalf("class = %s", r.Class)
	}
}

func TestClassifyContextOverflowHealthNeutral(t *testing.T) {
	r := Classify(400, []byte(`{"error":{"message":"context length exceeded: too many tokens"}}`))
	if r.Class != ContextOverflow {
		t.Fatalf("class = %s", r.Class)
	}
	if r.AffectsHealth || r.AffectsProvider {
		t.Fatalf("context overflow must not mark a healthy model dead: %+v", r)
	}
}

func TestClassifyToolCallingUnsupported(t *testing.T) {
	r := Classify(400, []byte(`{"error":{"message":"tools are not supported by this model"}}`))
	if r.Class != UnsupportedParameter && r.Class != UnsupportedToolCalling {
		t.Fatalf("class = %s", r.Class)
	}
	if r.AffectsHealth {
		t.Fatalf("tool capability failure must be health-neutral: %+v", r)
	}
}

func TestClassifyTimeoutAndNetwork(t *testing.T) {
	r := Classify(0, []byte(`context deadline exceeded`))
	if r.Class != Timeout {
		t.Fatalf("class = %s", r.Class)
	}
	r = Classify(0, []byte(`connection reset by peer`))
	if r.Class != NetworkError {
		t.Fatalf("class = %s", r.Class)
	}
}

func TestClassifyServerError(t *testing.T) {
	r := Classify(500, []byte(`internal error`))
	if r.Class != UpstreamInternalError || !r.Failover || !r.AffectsHealth {
		t.Fatalf("got %+v", r)
	}
	r = Classify(503, []byte(`overloaded`))
	if r.Class != UpstreamOverload {
		t.Fatalf("class = %s", r.Class)
	}
}
