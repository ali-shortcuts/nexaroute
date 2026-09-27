package remote

// Test TLS server with a self-signed certificate for a chosen DNS name and a
// capture hook for the TLS SNI (ClientHello.ServerName). Used to verify that
// IP-pinned dialing preserves SNI and certificate verification, and that
// HTTP/2 keeps working over the hardened transport.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type testTLSServer struct {
	name string // the DNS name the certificate is issued for
	addr string // 127.0.0.1:port of the listener
	pool *x509.CertPool
	hits atomic.Int64
	sni  atomic.Value // string: last ClientHello ServerName
}

func (s *testTLSServer) port() int {
	_, p, err := net.SplitHostPort(s.addr)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return n
}

func (s *testTLSServer) sniSeen() string {
	v, _ := s.sni.Load().(string)
	return v
}

func generateServerCert(t *testing.T, dnsName string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dnsName},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, leaf
}

// startTestTLSServer serves HTTPS on loopback with a certificate for dnsName.
// The handler is wrapped with a request counter. ALPN offers h2 and http/1.1.
func startTestTLSServer(t *testing.T, dnsName string, handler http.HandlerFunc) *testTLSServer {
	t.Helper()
	cert, leaf := generateServerCert(t, dnsName)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tls server listen: %v", err)
	}
	srv := &testTLSServer{name: dnsName, addr: ln.Addr().String(), pool: pool}
	srv.sni.Store("")

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			srv.sni.Store(chi.ServerName)
			return nil, nil // use the default config
		},
	}
	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.hits.Add(1)
			handler(w, r)
		}),
	}
	go func() { _ = httpSrv.Serve(tls.NewListener(ln, tlsCfg)) }()
	t.Cleanup(func() { _ = httpSrv.Close() })
	return srv
}

// testTLSConfig returns a client TLS config trusting the given test server CA,
// with verification on (never InsecureSkipVerify) and TLS 1.2 minimum.
func testTLSConfig(pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}
}
