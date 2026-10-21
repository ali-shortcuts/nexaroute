package config

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseTrustedProxyCIDRs(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr string
	}{
		{name: "empty", input: nil},
		{
			name:  "cidr and bare addresses",
			input: []string{"127.0.0.1/32", "10.0.0.0/8", "::1", "2001:db8::/32"},
			want:  []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128", "2001:db8::/32"},
		},
		{
			name:  "duplicates are removed",
			input: []string{"10.0.0.0/8", "10.1.2.3/8"},
			want:  []string{"10.0.0.0/8"},
		},
		{
			name:    "empty entry",
			input:   []string{"127.0.0.1/32", " "},
			wantErr: "must not be empty",
		},
		{
			name:    "hostname is not a network",
			input:   []string{"proxy.internal"},
			wantErr: "is not an IP address or CIDR",
		},
		{
			name:    "all addresses rejected",
			input:   []string{"0.0.0.0/0"},
			wantErr: "trusts every address",
		},
		{
			name:    "all IPv6 addresses rejected",
			input:   []string{"::/0"},
			wantErr: "trusts every address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTrustedProxyCIDRs(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error=%v want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("prefixes=%v want %v", got, tt.want)
			}
			for i, raw := range tt.want {
				if got[i] != netip.MustParsePrefix(raw) {
					t.Fatalf("prefix[%d]=%v want %v", i, got[i], raw)
				}
			}
		})
	}
}

func TestParseTrustedProxyCIDRsRejectsOversizedList(t *testing.T) {
	input := make([]string, 65)
	for i := range input {
		input[i] = "127.0.0.1/32"
	}
	if _, err := ParseTrustedProxyCIDRs(input); err == nil || !strings.Contains(err.Error(), "safe limit") {
		t.Fatalf("expected safe-limit error, got %v", err)
	}
}

func TestTLSValidateRejectsInvalidTrustedProxy(t *testing.T) {
	cfg := TLSConfig{TrustedProxyCIDRs: []string{"not-a-network"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "trusted_proxy_cidrs") {
		t.Fatalf("expected trusted_proxy_cidrs error, got %v", err)
	}
	cfg = TLSConfig{TrustedProxyCIDRs: []string{"127.0.0.1"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid bare address rejected: %v", err)
	}
}
