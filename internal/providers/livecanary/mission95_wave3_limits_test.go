package livecanary

import (
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave-3 normalizeLimits coverage: defaults, preserved explicit
// values, every range error, the needed-calls exceed error, and the
// reasoning minimum-token rules (generic and anthropic_compatible).
// normalizeLimits returns (Limits, callTimeout, overallTimeout, err).

func TestWave3NormalizeLimitsDefaults(t *testing.T) {
	got, callTimeout, overall, err := normalizeLimits(Options{}, config.ProviderConfig{}, nil)
	if err != nil {
		t.Fatalf("zero options must normalize to defaults: %v", err)
	}
	if got.MaxCalls != DefaultMaxCalls || got.MaxOutputTokens != DefaultMaxOutputTokens ||
		got.MaxInputTokens != DefaultMaxInputTokens || got.MaxCostUSD != DefaultMaxCostUSD {
		t.Fatalf("defaults = %+v", got)
	}
	if callTimeout != DefaultCallTimeout || overall != DefaultOverallTimeout {
		t.Fatalf("timeouts = %v/%v, want %v/%v", callTimeout, overall, DefaultCallTimeout, DefaultOverallTimeout)
	}
	if got.CallTimeoutSeconds != int(DefaultCallTimeout/time.Second) ||
		got.OverallTimeoutSeconds != int(DefaultOverallTimeout/time.Second) {
		t.Fatalf("timeout seconds = %d/%d, want %d/%d", got.CallTimeoutSeconds, got.OverallTimeoutSeconds,
			int(DefaultCallTimeout/time.Second), int(DefaultOverallTimeout/time.Second))
	}
}

func TestWave3NormalizeLimitsExplicitValuesPreserved(t *testing.T) {
	got, callTimeout, overall, err := normalizeLimits(Options{
		MaxCalls: 4, MaxOutputTokens: 512, MaxInputTokens: 1024,
		CallTimeout: 45 * time.Second, OverallTimeout: 4 * time.Minute,
		MaxCostUSD: 0.05,
	}, config.ProviderConfig{}, nil)
	if err != nil {
		t.Fatalf("explicit values must normalize cleanly: %v", err)
	}
	if got.MaxCalls != 4 || got.MaxOutputTokens != 512 || got.MaxInputTokens != 1024 ||
		callTimeout != 45*time.Second || overall != 4*time.Minute || got.MaxCostUSD != 0.05 {
		t.Fatalf("explicit values not preserved: %+v / %v / %v", got, callTimeout, overall)
	}
}

func TestWave3NormalizeLimitsRangeErrors(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"max calls too high", Options{MaxCalls: MaxCalls + 1}, "--max-calls"},
		{"max calls negative", Options{MaxCalls: -1}, "--max-calls"},
		{"max output tokens too high", Options{MaxOutputTokens: MaxOutputTokens + 1}, "--max-output-tokens"},
		{"max input tokens too high", Options{MaxInputTokens: MaxInputTokens + 1}, "--max-input-tokens"},
		{"call timeout too short", Options{CallTimeout: 500 * time.Millisecond}, "--call-timeout"},
		{"call timeout too long", Options{CallTimeout: 3 * time.Minute}, "--call-timeout"},
		{"overall timeout too short", Options{OverallTimeout: 500 * time.Millisecond}, "--overall-timeout"},
		{"overall timeout too long", Options{OverallTimeout: 11 * time.Minute}, "--overall-timeout"},
		{"overall below call", Options{CallTimeout: 60 * time.Second, OverallTimeout: 30 * time.Second}, "--overall-timeout must be greater"},
		{"cost negative", Options{MaxCostUSD: -0.01}, "--max-cost-usd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := normalizeLimits(tc.opts, config.ProviderConfig{}, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("normalizeLimits error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestWave3NormalizeLimitsNeededCallsExceedsCap(t *testing.T) {
	scenarios := []scenario{
		{ID: "s1", Calls: 5},
		{ID: "s2", Calls: 3},
	}
	if _, _, _, err := normalizeLimits(Options{MaxCalls: 6}, config.ProviderConfig{}, scenarios); err == nil || !strings.Contains(err.Error(), "need 8 calls") {
		t.Fatalf("needed calls 8 > cap 6 must error: %v", err)
	}
}

func TestWave3NormalizeLimitsReasoningMinimums(t *testing.T) {
	reasoning := []scenario{{ID: "reasoning", Calls: 1}}
	cases := []struct {
		name      string
		provider  string
		maxTokens int
		wantErr   string
	}{
		{"generic below minimum", "openai_compatible", 256, ">= 512"},
		{"generic at minimum ok", "openai_compatible", 512, ""},
		{"anthropic below minimum", "anthropic_compatible", 1024, ">= 2048"},
		{"anthropic at minimum ok", "anthropic_compatible", 2048, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := config.ProviderConfig{Type: tc.provider}
			_, _, _, err := normalizeLimits(Options{MaxOutputTokens: tc.maxTokens}, p, reasoning)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("sufficient tokens must normalize: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
