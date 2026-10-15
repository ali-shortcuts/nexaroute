package config

import "testing"

func TestTelemetryConfigValidation(t *testing.T) {
	cfg := Default()
	cfg.Telemetry.OTLPEndpointEnv = "not a valid env name!"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for an invalid env var name")
	}
	cfg.Telemetry.OTLPEndpointEnv = "NEXAROUTE_OTLP_ENDPOINT"
	cfg.Telemetry.ServiceName = "gateway-a"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid telemetry config rejected: %v", err)
	}
	cfg.Telemetry.ServiceName = string(make([]byte, 129))
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for an overlong service name")
	}
}
