package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
)

func TestAcceptanceCredentialSourceCoversConfiguredSources(t *testing.T) {
	t.Setenv("NEXA_TEST_AWS_SECRET", "aws-secret")
	t.Setenv("NEXA_TEST_PROVIDER_KEY", "env-key")

	tests := []struct {
		name string
		p    config.ProviderConfig
		want string
	}{
		{
			name: "bedrock secret key from environment",
			p:    config.ProviderConfig{Type: "bedrock", SecretKeyEnv: "NEXA_TEST_AWS_SECRET"},
			want: "aws-env",
		},
		{
			name: "credential pool",
			p:    config.ProviderConfig{Credentials: []config.CredentialConfig{{Name: "primary", APIKey: "pooled-key", Enabled: true}}},
			want: "pool",
		},
		{
			name: "API key environment variable",
			p:    config.ProviderConfig{APIKeyEnv: "NEXA_TEST_PROVIDER_KEY"},
			want: "env",
		},
		{
			name: "literal API key",
			p:    config.ProviderConfig{APIKey: "inline-key"},
			want: "literal",
		},
		{
			name: "explicit no-auth mode",
			p:    config.ProviderConfig{AuthMode: "none"},
			want: "none",
		},
		{
			name: "unconfigured credentials",
			p:    config.ProviderConfig{},
			want: "none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acceptanceCredentialSource(tt.p); got != tt.want {
				t.Fatalf("acceptanceCredentialSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanaryExitCodeMapping(t *testing.T) {
	tests := []struct {
		outcome string
		want    int
	}{
		{livecanary.OutcomePass, 0},
		{livecanary.OutcomeNoEvidence, 0},
		{livecanary.OutcomePartial, 3},
		{livecanary.OutcomeCanceled, 130},
		{"unexpected-outcome", 1},
	}
	for _, tt := range tests {
		t.Run(tt.outcome, func(t *testing.T) {
			if got := canaryExitCode(tt.outcome); got != tt.want {
				t.Fatalf("canaryExitCode(%q) = %d, want %d", tt.outcome, got, tt.want)
			}
		})
	}
}

func TestRunConformanceCommandContracts(t *testing.T) {
	t.Run("missing subcommand is usage error", func(t *testing.T) {
		var out, errOut strings.Builder
		if got := runConformance(nil, &out, &errOut); got != 2 {
			t.Fatalf("exit=%d, want 2; stderr=%q", got, errOut.String())
		}
		if !strings.Contains(errOut.String(), "usage:") {
			t.Fatalf("missing usage on stderr: %q", errOut.String())
		}
	})
	t.Run("help succeeds", func(t *testing.T) {
		var out, errOut strings.Builder
		if got := runConformance([]string{"--help"}, &out, &errOut); got != 0 {
			t.Fatalf("exit=%d, want 0; stderr=%q", got, errOut.String())
		}
		if !strings.Contains(out.String(), "--acknowledge-cost") {
			t.Fatalf("help omits explicit cost acknowledgement: %q", out.String())
		}
	})
	t.Run("unknown subcommand is rejected", func(t *testing.T) {
		var out, errOut strings.Builder
		if got := runConformance([]string{"unknown"}, &out, &errOut); got != 2 {
			t.Fatalf("exit=%d, want 2", got)
		}
		if !strings.Contains(errOut.String(), "unknown conformance subcommand") {
			t.Fatalf("unexpected stderr: %q", errOut.String())
		}
	})
	t.Run("canary without options fails safely", func(t *testing.T) {
		var out, errOut strings.Builder
		if got := runConformance([]string{"canary"}, &out, &errOut); got != 2 {
			t.Fatalf("exit=%d, want usage error 2; stderr=%q", got, errOut.String())
		}
	})
}

func TestAcceptanceHTTPClientBoundsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if n < 4 {
			http.Redirect(w, r, fmt.Sprintf("/%d", n+1), http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := acceptanceHTTPClient(1200 * time.Millisecond)
	if client.Timeout != 1200*time.Millisecond {
		t.Fatalf("client timeout = %s", client.Timeout)
	}
	resp, err := client.Get(srv.URL + "/2")
	if err != nil {
		t.Fatalf("two redirects should be allowed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	_, err = client.Get(srv.URL + "/0")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "too many redirects") {
		t.Fatalf("redirect loop must be rejected with a bounded error, got %v", err)
	}
}
