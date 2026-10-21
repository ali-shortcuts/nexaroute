package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// testTLSMaterial builds a CA and a leaf certificate valid for localhost so a
// test server can prove which name the transport verifies against.
func testTLSMaterial(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "egress test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return pair, roots
}

// Regression: the provider egress transport used to pin TLSClientConfig.ServerName
// to the provider's *base* host. Any allow-listed absolute endpoint on a
// different HTTPS host (a documented operator feature) therefore negotiated
// SNI for—and verified the certificate of—the wrong name instead of the host
// actually being contacted.
func TestTransportUsesRequestHostForTLSVerification(t *testing.T) {
	pair, roots := testTLSMaterial(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.ServerName != "localhost" {
			t.Errorf("upstream observed SNI %q, want localhost", r.TLS.ServerName)
		}
		w.WriteHeader(http.StatusOK)
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	defer upstream.Close()

	tr, err := Policy{
		AllowedHosts:  []string{"base.invalid", "localhost"},
		AllowLoopback: true,
	}.NewTransport("https://base.invalid", "")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.RootCAs = roots
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	target.Host = "localhost:" + target.Port()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(target.String())
	if err != nil {
		t.Fatalf("request to allow-listed HTTPS host failed TLS verification: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
