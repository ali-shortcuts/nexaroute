package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoveragePhase2ValidationHelpers(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func() bool
		want bool
	}{
		{"empty id", func() bool { return validLocalID("") }, false},
		{"safe id", func() bool { return validLocalID("tenant-1.v2_ok") }, true},
		{"unsafe id", func() bool { return validLocalID("bad/id") }, false},
		{"empty header", func() bool { return validHeaderName("") }, false},
		{"valid header", func() bool { return validHeaderName("X-Test_1") }, true},
		{"invalid header", func() bool { return validHeaderName("X Test") }, false},
		{"valid value", func() bool { return validHeaderValue("ok\tvalue") }, true},
		{"invalid value", func() bool { return validHeaderValue("bad\x00value") }, false},
		{"valid env", func() bool { return validEnvName("NEXA_1") }, true},
		{"invalid env first digit", func() bool { return validEnvName("1NEXA") }, false},
		{"invalid env punctuation", func() bool { return validEnvName("NEXA-1") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	valid := DecisionPolicyWeights{RouterBaseline: 1}
	if err := validateDecisionPolicyWeights(valid, "weights"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), -1, 1000001} {
		bad := valid
		bad.Cost = v
		if err := validateDecisionPolicyWeights(bad, "weights"); err == nil {
			t.Fatalf("value %v accepted", v)
		}
	}
	if !hasPositiveWeight(valid) || hasPositiveWeight(DecisionPolicyWeights{}) {
		t.Fatal("positive-weight detection failed")
	}
}

func TestCoveragePhase2EnvironmentAndStrictConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_LISTEN", " 0.0.0.0:9000 ")
	t.Setenv("NEXAROUTE_CLIENT_BASE_URL", " https://client.example/ ")
	t.Setenv("NEXAROUTE_ADMIN_KEY", " admin-secret ")
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "false")
	t.Setenv("NEXAROUTE_LOG_FILE", " ")
	cfg := Default()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:9000" || cfg.ClientBaseURL != "https://client.example" || cfg.Admin.APIKey != "admin-secret" || cfg.Admin.BindLocalOnly || cfg.Logging.File != "off" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "not-bool")
	if err := cfg.ApplyEnvOverrides(); err == nil {
		t.Fatal("invalid bool accepted")
	}
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "true")
	ok, err := strictConfigEnabled()
	if err != nil || !ok {
		t.Fatalf("strict=%v err=%v", ok, err)
	}
	cfg = Config{}
	cfg.ApplyDefaults()
	if err := cfg.ValidateStrict(); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("strict defaults not rejected: %v", err)
	}
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "bad")
	if _, err := strictConfigEnabled(); err == nil {
		t.Fatal("invalid strict value accepted")
	}
}

func TestCoveragePhase2LoadBaseAndAtomicPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if _, err := LoadBase(path); err == nil {
		t.Fatal("missing config accepted")
	}
	data := `{"listen":"127.0.0.1:9999","logging":{"file":"off"}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("loaded config: %+v", cfg)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(path); err == nil {
		t.Fatal("malformed config accepted")
	}
}
