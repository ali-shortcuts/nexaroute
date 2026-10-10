package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func captureGatewayStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSecretsCLIStatusVerifyAndGuardedDecrypt(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{ID: "private", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "cli-output-canary", Enabled: false}}
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	status := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"status", "--config", path}); err != nil {
			t.Errorf("status: %v", err)
		}
	})
	if !strings.Contains(status, "encrypted_fields=1") || strings.Contains(status, "cli-output-canary") {
		t.Fatalf("unsafe or incomplete status output: %q", status)
	}
	verified := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"verify", "--config", path}); err != nil {
			t.Errorf("verify: %v", err)
		}
	})
	if !strings.Contains(verified, "verified_fields=1") || strings.Contains(verified, "cli-output-canary") {
		t.Fatalf("unsafe or incomplete verify output: %q", verified)
	}
	if err := runSecrets([]string{"decrypt", "--config", path, "--to-stdout"}); err == nil {
		t.Fatal("decrypt accepted missing --allow-plaintext")
	}
	if err := runSecrets([]string{"decrypt", "--config", path, "--allow-plaintext"}); err == nil {
		t.Fatal("decrypt accepted missing --to-stdout")
	}
	decrypted := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"decrypt", "--config", path, "--to-stdout", "--allow-plaintext"}); err != nil {
			t.Errorf("explicit decrypt: %v", err)
		}
	})
	if !strings.Contains(decrypted, "cli-output-canary") {
		t.Fatal("explicitly authorized plaintext output did not contain the decrypted config")
	}
}

func TestSecretsCLIRejectsMissingInputsAndExternalKeyRotation(t *testing.T) {
	if err := runSecrets(nil); err == nil {
		t.Fatal("missing subcommand accepted")
	}
	if err := runSecrets([]string{"unknown"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := runSecrets([]string{"status", "--config", ""}); err == nil {
		t.Fatal("empty config path accepted")
	}
	if err := runSecrets([]string{"status", "--config", filepath.Join(t.TempDir(), "absent.json")}); err == nil {
		t.Fatal("missing config path accepted")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "externally-managed-key")
	if err := runSecrets([]string{"rotate", "--config", "config.json"}); err == nil || !strings.Contains(err.Error(), "auto-managed") {
		t.Fatalf("external-key rotation was not refused clearly: %v", err)
	}
}

func TestWriteRotateFileIsAtomicPrivateAndReplacesPriorContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeRotateFile(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement=%q err=%v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("rotated file mode=%v err=%v", st, err)
	}
}
