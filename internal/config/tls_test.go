package config

import (
	"strings"
	"testing"
)

func TestTLSConfigValidation(t *testing.T) {
	base := Default()
	base.ApplyDefaults()
	tests := []struct {
		name string
		tls  TLSConfig
		want string
	}{
		{name: "disabled by default"},
		{name: "missing certificate pair", tls: TLSConfig{Enabled: true}, want: "cert_file and tls.key_file"},
		{name: "mTLS needs enabled TLS", tls: TLSConfig{RequireClientCertAdmin: true}, want: "tls.enabled=true"},
		{name: "mTLS needs CA", tls: TLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem", RequireClientCertDataPlane: true}, want: "tls.client_ca_file"},
		{name: "valid HTTPS", tls: TLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem"}},
		{name: "valid mTLS", tls: TLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem", ClientCAFile: "ca.pem", RequireClientCertAdmin: true, RequireClientCertDataPlane: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.TLS = tc.tls
			err := cfg.Validate()
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}
