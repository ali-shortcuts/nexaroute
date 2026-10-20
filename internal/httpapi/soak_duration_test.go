package httpapi

import (
	"testing"
	"time"
)

// Regression test for the push-triggered Soak workflow failure (run
// 39366648199 on main 637cb39a): scripts/soak.sh defaults
// NEXAROUTE_SOAK_DURATION to "0s" ("0s disables"), but the duration helper
// rejected the "0s" spelling while accepting "" and "0", so
// TestSoakConcurrentTrafficAndHotReload failed before doing any work.
func TestParseSoakDurationZeroDisablesSustainedPhase(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"bare zero", "0", 0, false},
		{"zero seconds", "0s", 0, false},
		{"zero minutes", "0m", 0, false},
		{"surrounding whitespace", "  0s\t", 0, false},
		{"positive seconds", "30s", 30 * time.Second, false},
		{"positive hours", "2h", 2 * time.Hour, false},
		{"negative stays a config error", "-5s", 0, true},
		{"malformed stays a config error", "soon", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSoakDuration(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseSoakDuration(%q) = (%v, nil), want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSoakDuration(%q) error = %v, want nil", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseSoakDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
