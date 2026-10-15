//go:build linux

package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Headless keyring contract (M4-7): the linux provider talks to
// `secret-tool` (libsecret). These tests stub secret-tool on PATH so
// the provider's protocol handling is exercised without a D-Bus
// session or keyring daemon: store/load round trip through the stub's
// state file, a missing entry fails closed, a malformed stored value
// is rejected, and a failing secret-tool surfaces an error — the
// provider must never fabricate or fall back to another key source.
const secretToolStub = `#!/bin/sh
# Minimal secret-tool emulation: state in $SECRET_TOOL_STUB_DIR.
dir="$SECRET_TOOL_STUB_DIR"
case "$1" in
  lookup)
    if [ -f "$dir/value" ]; then cat "$dir/value"; exit 0; fi
    exit 1 ;;
  store)
    cat > "$dir/value"; exit 0 ;;
esac
exit 2
`

func installSecretToolStub(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	stub := filepath.Join(binDir, "secret-tool")
	if err := os.WriteFile(stub, []byte(secretToolStub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_TOOL_STUB_DIR", stateDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stateDir
}

func TestKeyringProviderRoundTripViaSecretTool(t *testing.T) {
	installSecretToolStub(t)
	provider := newKeyringProvider()
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := provider.Store(ctx, "nexaroute/master-key", key); err != nil {
		t.Fatalf("store via secret-tool stub: %v", err)
	}
	got, err := provider.Load(ctx, "nexaroute/master-key")
	if err != nil {
		t.Fatalf("load via secret-tool stub: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("keyring round trip returned a different key")
	}
}

func TestKeyringProviderHeadlessFailuresAreClosed(t *testing.T) {
	stateDir := installSecretToolStub(t)
	provider := newKeyringProvider()
	ctx := context.Background()
	// No entry stored: lookup fails, Load must error (never a key).
	if _, err := provider.Load(ctx, "nexaroute/master-key"); err == nil {
		t.Fatal("load of a missing keyring entry must fail")
	}
	// A malformed stored value is rejected, not truncated or padded.
	garbage := base64.StdEncoding.EncodeToString([]byte("short"))
	if err := os.WriteFile(filepath.Join(stateDir, "value"), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Load(ctx, "nexaroute/master-key"); err == nil ||
		!strings.Contains(err.Error(), "invalid 32-byte key") {
		t.Fatalf("malformed keyring value: err=%v, want invalid-key rejection", err)
	}
	// Store rejects wrong-length keys before invoking secret-tool.
	if err := provider.Store(ctx, "nexaroute/master-key", []byte("short")); err == nil {
		t.Fatal("store of a non-32-byte key must fail")
	}
}

func TestKeyringProviderMissingSecretToolFailsClosed(t *testing.T) {
	// PATH contains nothing useful: exec must fail and surface.
	t.Setenv("PATH", t.TempDir())
	provider := newKeyringProvider()
	if _, err := provider.Load(context.Background(), "nexaroute/master-key"); err == nil {
		t.Fatal("load without secret-tool on PATH must fail closed")
	}
}
