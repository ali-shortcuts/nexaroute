package secrets

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestEnvProviderFailClosedAndRoundTrip(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	name := "NEXAROUTE_TEST_CUSTODY_KEY"
	t.Setenv(name, base64.StdEncoding.EncodeToString(key))
	got, err := (EnvProvider{}).Load(context.Background(), name)
	if err != nil || string(got) != string(key) {
		t.Fatalf("load: %v", err)
	}
	t.Setenv(name, "short")
	if _, err := (EnvProvider{}).Load(context.Background(), name); err == nil {
		t.Fatal("accepted malformed environment key")
	}
	if err := (EnvProvider{}).Store(context.Background(), name, key); err == nil {
		t.Fatal("environment provider unexpectedly writable")
	}
}

func TestCommandProviderProtocolAndInvalidOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses POSIX executable")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "provider")
	key := []byte("01234567890123456789012345678901")
	encoded := base64.StdEncoding.EncodeToString(key)
	body := "#!/bin/sh\nread first second\nif [ \"$first\" = get ]; then printf '%s\\n' '" + encoded + "'; exit 0; fi\ncat >/dev/null\nprintf ok\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	p := CommandProvider{Command: script}
	got, err := p.Load(context.Background(), "ref-without-secret")
	if err != nil || string(got) != string(key) {
		t.Fatalf("command load: %v", err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\nprintf bad\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (CommandProvider{Command: bad}).Load(context.Background(), "ref"); err == nil {
		t.Fatal("accepted malformed command output")
	}
	if err := (CommandProvider{Command: script}).Store(context.Background(), "ref", []byte("short")); err == nil {
		t.Fatal("accepted short key for command store")
	}
}

func TestCommandProviderRejectsOversizedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses POSIX executable")
	}
	dir := t.TempDir()
	// A helper that floods stdout: the gateway path must stop at the
	// size bound instead of buffering arbitrary helper output in memory
	// and only then rejecting it on key length. The shipped Vault
	// helper caps its own Vault response at 1 MiB, so this test pins
	// the gateway-side bound for any command provider.
	mkFlood := func(t *testing.T, name string, bytes int) string {
		t.Helper()
		script := filepath.Join(dir, name)
		body := "#!/bin/sh\nread _\nhead -c " + strconv.Itoa(bytes) + " /dev/zero | tr '\\0' 'A'\n"
		if err := os.WriteFile(script, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		return script
	}
	over := mkFlood(t, "flood-over", (1<<20)+1)
	if _, err := (CommandProvider{Command: over}).Load(context.Background(), "ref"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized helper output was not rejected at the size bound: err=%v", err)
	}
	at := mkFlood(t, "flood-at", 1<<20)
	if _, err := (CommandProvider{Command: at}).Load(context.Background(), "ref"); err == nil || strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("output exactly at the bound must not trip the size bound: err=%v", err)
	}
}

func TestConfiguredProviderRejectsUnknownAndKeyringDoesNotFallback(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "unknown")
	if _, _, _, err := configuredProvider(); err == nil {
		t.Fatal("accepted unknown key provider")
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "keyring")
	p, ref, enabled, err := configuredProvider()
	if err != nil || !enabled || ref == "" || p == nil {
		t.Fatalf("keyring selection: provider=%T ref=%q enabled=%v err=%v", p, ref, enabled, err)
	}
	// A missing/unavailable OS keyring must be an error, never a file fallback.
	if _, err := p.Load(context.Background(), ref); err == nil {
		t.Fatal("keyring unexpectedly fell back or returned a key")
	}
}
