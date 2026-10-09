package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// ServerTLSConfig builds a TLS configuration that re-reads the certificate and
// client CA for every ClientHello. Operators can atomically replace PEM files
// and have new connections use them without restarting the gateway.
func ServerTLSConfig(cfg config.TLSConfig, configDir string) (*tls.Config, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	certPath := resolve(configDir, cfg.CertFile)
	keyPath := resolve(configDir, cfg.KeyFile)
	caPath := resolve(configDir, cfg.ClientCAFile)
	load := func() (*tls.Config, error) {
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate/key: %w", err)
		}
		result := &tls.Config{ // #nosec G402 -- TLS 1.2 is the documented compatibility floor.
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
			NextProtos:   []string{"http/1.1"},
		}
		if cfg.RequireClientCertAdmin || cfg.RequireClientCertDataPlane {
			pem, err := os.ReadFile(caPath)
			if err != nil {
				return nil, fmt.Errorf("read TLS client CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("TLS client CA file contains no valid certificates")
			}
			result.ClientCAs = pool
			result.ClientAuth = tls.VerifyClientCertIfGiven
		}
		return result, nil
	}
	// Fail fast on startup/config validate; this is followed by a fresh read
	// for every handshake, so later certificate replacement is still picked up.
	if _, err := load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return load()
		},
	}, nil
}

func resolve(base, path string) string {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if base == "" {
		return path
	}
	return filepath.Join(base, path)
}
