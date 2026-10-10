package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestConfigCommandsLegacyMigrationAndEncryptedReadOnlyBehavior(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	raw := `{"listen":"127.0.0.1:8080","admin":{"api_key":"legacy-admin"},"providers":[]}`
	if err := os.WriteFile(legacy, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(legacy)
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"dry-run", "--config", legacy}, &stdout, &stderr); code != configExitOK {
		t.Fatalf("legacy dry-run exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	after, _ := os.ReadFile(legacy)
	if bytes.Equal(before, after) || !strings.Contains(string(after), "nxs1:") || strings.Contains(string(after), "legacy-admin") {
		t.Fatalf("legacy migration was not performed safely: %s", after)
	}
	encrypted := filepath.Join(dir, "encrypted.json")
	cfg := config.Default()
	cfg.Admin.APIKey = "encrypted-admin"
	if err := config.SaveAtomic(encrypted, cfg); err != nil {
		t.Fatal(err)
	}
	stable, _ := os.ReadFile(encrypted)
	stdout.Reset()
	stderr.Reset()
	if code := runConfig([]string{"diff", "--config", encrypted}, &stdout, &stderr); code != configExitOK {
		t.Fatalf("encrypted diff exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stableAfter, _ := os.ReadFile(encrypted)
	if !bytes.Equal(stable, stableAfter) || strings.Contains(stdout.String(), "encrypted-admin") {
		t.Fatalf("encrypted diff modified or disclosed config: %s", stdout.String())
	}
}

func writeTestTLSMaterial(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func makeEncryptedTLSConfig(t *testing.T, dir string) string {
	t.Helper()
	certFile, keyFile := writeTestTLSMaterial(t, dir)
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	cfg := config.Default()
	cfg.Listen = address
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Logging.File = "off"
	cfg.Logging.ConsoleMaxLinesPerMinute = 0
	cfg.TLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	cfg.Admin.APIKey = "encrypted-admin"
	path := filepath.Join(dir, "config.json")
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTLSAndEncryptedConfigLoadsTogether(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	path := makeEncryptedTLSConfig(t, t.TempDir())
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Admin.APIKey != "encrypted-admin" || !loaded.TLS.Enabled {
		t.Fatalf("encrypted TLS config did not load: %+v", loaded)
	}
}

func TestGatewayStartsWithTLSAndEncryptedConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "false")
	t.Setenv("NEXAROUTE_LISTEN", "")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "")
	t.Setenv("NEXAROUTE_LOG_FILE", "off")
	dir := t.TempDir()
	configPath := makeEncryptedTLSConfig(t, dir)
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Video.Enabled = true
	loaded.Video.DevelopmentFakeProvider = true
	loaded.Video.StorePath = filepath.Join(dir, "video-jobs.json")
	if err := config.SaveAtomic(configPath, loaded); err != nil {
		t.Fatal("save encrypted video/TLS config:", err)
	}
	loaded, err = config.Load(configPath)
	if err != nil {
		t.Fatal("reload encrypted video/TLS config:", err)
	}
	address := loaded.Listen
	oldArgs, oldStdout, oldNotify := os.Args, os.Stdout, notifyGatewayContext
	defer func() { os.Args, os.Stdout, notifyGatewayContext = oldArgs, oldStdout, oldNotify }()
	os.Args = []string{"nexaroute", "-config", configPath, "-no-browser"}
	stdout, err := os.CreateTemp(dir, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	os.Stdout = stdout
	cancelReady := make(chan context.CancelFunc, 1)
	notifyGatewayContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancelReady <- cancel
		return ctx, cancel
	}
	done := make(chan struct{})
	go func() { main(); close(done) }()
	cancel := <-cancelReady
	client := &http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // #nosec G402 -- test certificate is self-signed.
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, getErr := client.Get("https://" + address + "/healthz")
		if getErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		<-done
		t.Fatal("TLS gateway did not become healthy")
	}
	videoResp, videoErr := client.Get("https://" + address + "/v1/video/providers")
	if videoErr != nil {
		t.Fatal("video route over TLS failed:", videoErr)
	}
	videoBody, readErr := io.ReadAll(videoResp.Body)
	_ = videoResp.Body.Close()
	if readErr != nil || videoResp.StatusCode != http.StatusOK || !bytes.Contains(videoBody, []byte(`"fake"`)) {
		t.Fatalf("video providers over TLS status=%d body=%s err=%v", videoResp.StatusCode, videoBody, readErr)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS gateway did not shut down")
	}
}
