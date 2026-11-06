package transport

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Agent-D1 coverage: disabled TLS config, missing certificate/key at
// startup, and missing client CA when client certificates are required.

func TestAgentD1ServerTLSConfigValidationError(t *testing.T) {
	// Enabled without certificate/key paths fails Validate before
	// any loading is attempted.
	_, err := ServerTLSConfig(config.TLSConfig{Enabled: true}, "")
	if err == nil {
		t.Fatal("TLS config without cert/key should fail validation")
	}
	if !strings.Contains(err.Error(), "tls.cert_file and tls.key_file are required") {
		t.Fatalf("error = %v, want the cert/key validation error", err)
	}
}

func TestAgentD1ServerTLSConfigEmptyConfigDir(t *testing.T) {
	// With no config directory, relative certificate paths resolve
	// to themselves; the startup load then fails on the missing file.
	_, err := ServerTLSConfig(config.TLSConfig{
		Enabled:  true,
		CertFile: "relative.crt",
		KeyFile:  "relative.key",
	}, "")
	if err == nil {
		t.Fatal("startup load with a missing relative certificate should fail")
	}
	if !strings.Contains(err.Error(), "load TLS certificate/key") {
		t.Fatalf("error = %v, want it to mention loading the TLS certificate/key", err)
	}
}

func TestAgentD1ServerTLSConfigDisabledReturnsNil(t *testing.T) {
	cfg, err := ServerTLSConfig(config.TLSConfig{Enabled: false}, "")
	if err != nil {
		t.Fatalf("disabled TLS config should not error, got %v", err)
	}
	if cfg != nil {
		t.Fatalf("disabled TLS config should return nil config, got %+v", cfg)
	}
}

func TestAgentD1ServerTLSConfigMissingCertificateFailsFast(t *testing.T) {
	dir := t.TempDir()
	_, err := ServerTLSConfig(config.TLSConfig{
		Enabled:  true,
		CertFile: "missing.crt",
		KeyFile:  "missing.key",
	}, dir)
	if err == nil {
		t.Fatal("startup load with a missing certificate should fail")
	}
	if !strings.Contains(err.Error(), "load TLS certificate/key") {
		t.Fatalf("error = %v, want it to mention loading the TLS certificate/key", err)
	}
}

func TestAgentD1ServerTLSConfigMissingClientCAFailsFast(t *testing.T) {
	dir := t.TempDir()
	makeTestPair(t, dir, 1)
	_, err := ServerTLSConfig(config.TLSConfig{
		Enabled:                true,
		CertFile:               "server.crt",
		KeyFile:                "server.key",
		ClientCAFile:           "missing-ca.pem",
		RequireClientCertAdmin: true,
	}, dir)
	if err == nil {
		t.Fatal("startup load with a missing client CA should fail")
	}
	if !strings.Contains(err.Error(), "read TLS client CA") {
		t.Fatalf("error = %v, want it to mention reading the TLS client CA", err)
	}
}
