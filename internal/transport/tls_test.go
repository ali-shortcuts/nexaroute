package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func makeTestPair(t *testing.T, dir string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := os.Create(filepath.Join(dir, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(cert, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	if err := cert.Close(); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestServerTLSConfigValidatesAndReloadsCertificate(t *testing.T) {
	dir := t.TempDir()
	makeTestPair(t, dir, 1)
	cfg := config.TLSConfig{Enabled: true, CertFile: "server.crt", KeyFile: "server.key"}
	tlsConfig, err := ServerTLSConfig(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("minimum version=%d", tlsConfig.MinVersion)
	}
	first, err := tlsConfig.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	firstCert, err := x509.ParseCertificate(first.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if firstCert.SerialNumber.Int64() != 1 {
		t.Fatalf("initial certificate serial=%s", firstCert.SerialNumber)
	}

	makeTestPair(t, dir, 2)
	second, err := tlsConfig.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	secondCert, err := x509.ParseCertificate(second.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if secondCert.SerialNumber.Int64() != 2 {
		t.Fatalf("certificate was not reloaded: serial=%s", secondCert.SerialNumber)
	}
}

func TestServerTLSConfigRequiresValidMaterialAndCA(t *testing.T) {
	if got, err := ServerTLSConfig(config.TLSConfig{}, t.TempDir()); got != nil || err != nil {
		t.Fatalf("disabled TLS returned config=%v err=%v", got, err)
	}
	dir := t.TempDir()
	cfg := config.TLSConfig{Enabled: true, CertFile: "missing.crt", KeyFile: "missing.key"}
	if _, err := ServerTLSConfig(cfg, dir); err == nil {
		t.Fatal("missing server certificate unexpectedly accepted")
	}
	makeTestPair(t, dir, 7)
	cfg = config.TLSConfig{Enabled: true, CertFile: "server.crt", KeyFile: "server.key", ClientCAFile: "bad-ca.pem", RequireClientCertAdmin: true}
	if err := os.WriteFile(filepath.Join(dir, "bad-ca.pem"), []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerTLSConfig(cfg, dir); err == nil {
		t.Fatal("invalid client CA unexpectedly accepted")
	}
}

func TestServerTLSConfigEnablesClientCertificateVerification(t *testing.T) {
	dir := t.TempDir()
	makeTestPair(t, dir, 99)
	cfg := config.TLSConfig{Enabled: true, CertFile: "server.crt", KeyFile: "server.key", ClientCAFile: "server.crt", RequireClientCertAdmin: true}
	base, err := ServerTLSConfig(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	perHandshake, err := base.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if perHandshake.ClientAuth != tls.VerifyClientCertIfGiven || perHandshake.ClientCAs == nil {
		t.Fatalf("client verification not configured: mode=%v pool=%v", perHandshake.ClientAuth, perHandshake.ClientCAs)
	}
}
