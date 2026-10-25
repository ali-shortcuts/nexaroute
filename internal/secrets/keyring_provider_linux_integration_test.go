//go:build linux && integration

package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRealLinuxKeyringStoreRoundTrips is intentionally opt-in. CI runs it
// inside a disposable D-Bus session with a real Secret Service implementation;
// ordinary unit tests keep using the hermetic secret-tool fixture.
func TestRealLinuxKeyringStoreRoundTrips(t *testing.T) {
	if os.Getenv("NEXAROUTE_TEST_REAL_KEYRING") != "1" {
		t.Skip("set NEXAROUTE_TEST_REAL_KEYRING=1 to use a real Secret Service")
	}
	if _, err := exec.LookPath("secret-tool"); err != nil {
		t.Fatalf("real secret-tool is required for this acceptance test: %v", err)
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Fatal("DBUS_SESSION_BUS_ADDRESS must refer to the disposable test session")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ref := fmt.Sprintf("nexaroute/acceptance-%d-%d", os.Getpid(), time.Now().UnixNano())
	key := []byte("nexaroute-real-keyring-acceptance-32")
	// Ensure this test does not leave its uniquely scoped test item behind.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "secret-tool", "clear", "service", "nexaroute", "account", ref).Run()
	})

	provider := linuxKeyringProvider{}
	if err := provider.Store(ctx, ref, key); err != nil {
		t.Fatalf("Store against real Secret Service: %v", err)
	}
	got, err := provider.Load(ctx, ref)
	if err != nil {
		t.Fatalf("Load after real Store: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("real Secret Service round-trip mismatch: got %x, want %x", got, key)
	}
}
