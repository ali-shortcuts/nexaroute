package config

import (
	"strings"
	"testing"
)

// A user can paste the secret itself into api_key_env; validation errors are
// logged, so they must never echo the offending value.
func TestDecisionProviderAPIKeyEnvErrorDoesNotEchoValue(t *testing.T) {
	const secret = "sk-live-NOT-A-REAL-KEY-1234567890"
	for _, bad := range []string{secret, "9STARTS_WITH_DIGIT"} {
		cfg := Default()
		cfg.DecisionProviders = []DecisionProviderConfig{{
			ID: "dp1", Type: "jev", BaseURL: "https://example.com", APIKeyEnv: bad, PrivacyMode: "metadata_only",
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("api_key_env %q must be rejected", bad)
		}
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("validation error leaks api_key_env value: %v", err)
		}
	}
}
