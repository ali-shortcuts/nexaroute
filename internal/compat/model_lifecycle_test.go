package compat

import (
	"net/http"
	"testing"
)

func TestClassifyRealModelLifecycleFailures(t *testing.T) {
	retired := []byte(`{"type":"about:blank","title":"Gone","status":410,"detail":"The model 'deepseek-ai/deepseek-v4-flash' has reached its end of life on 2026-08-07T09:00:00Z and is no longer available."}`)
	cls := ClassifyUpstreamError(http.StatusGone, retired)
	if cls.Class != ClassModelRetired {
		t.Fatalf("410 EOL class=%q want %q: %+v", cls.Class, ClassModelRetired, cls)
	}
	if p := cls.Policy(); !p.Failover || p.QuarantineDeployment || p.SignalProvider || p.HardCooldown {
		t.Fatalf("retired model policy must fail over without recovery/provider penalty: %+v", p)
	}

	cases := [][]byte{
		[]byte(`{"error":{"message":"Model 'deepseek-v4-flash' is currently unavailable.","type":"invalid_request_error","param":null,"code":"model_unavailable"}}`),
		[]byte(`{"error":{"message":"Model 'XiaomiMiMo/MiMo-V2.5' is currently unavailable.","type":"invalid_request_error","param":null,"code":"model_unavailable"}}`),
	}
	for _, body := range cases {
		cls = ClassifyUpstreamError(http.StatusBadRequest, body)
		if cls.Class != ClassModelTemporarilyUnavailable {
			t.Fatalf("model_unavailable class=%q want %q: %+v", cls.Class, ClassModelTemporarilyUnavailable, cls)
		}
		p := cls.Policy()
		if !p.Failover || !p.QuarantineDeployment || p.SignalProvider || p.HardCooldown {
			t.Fatalf("temporary unavailable policy=%+v", p)
		}
	}
}

func TestGenericCallerBadRequestRemainsCallerError(t *testing.T) {
	body := []byte(`{"error":{"message":"invalid request body: messages is required","type":"invalid_request_error"}}`)
	cls := ClassifyUpstreamError(http.StatusBadRequest, body)
	if !cls.CallerError || cls.Policy().Failover {
		t.Fatalf("true caller error must not fan out across providers: %+v policy=%+v", cls, cls.Policy())
	}
}
