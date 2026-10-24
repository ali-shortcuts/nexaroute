//go:build linux

package secrets

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxKeyringStoreUsesRequiredLabelAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	valuePath := filepath.Join(dir, "stored-value.txt")
	fakeSecretTool := filepath.Join(dir, "secret-tool")
	script := `#!/bin/sh
case "$1" in
store)
  printf '%s\n' "$@" > "$KEYRING_ARGS_FILE"
  cat > "$KEYRING_VALUE_FILE"
  ;;
lookup)
  cat "$KEYRING_VALUE_FILE"
  ;;
*)
  echo "unexpected secret-tool operation" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(fakeSecretTool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KEYRING_ARGS_FILE", argsPath)
	t.Setenv("KEYRING_VALUE_FILE", valuePath)

	key := []byte("01234567890123456789012345678901")
	ref := "nexaroute/master-key"
	provider := linuxKeyringProvider{}
	if err := provider.Store(context.Background(), ref, key); err != nil {
		t.Fatalf("Store: %v", err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	const wantArgs = "store\n--label\nNexaRoute master key\nservice\nnexaroute\naccount\nnexaroute/master-key\n"
	if string(args) != wantArgs {
		t.Fatalf("secret-tool store arguments mismatch:\n got %q\nwant %q", string(args), wantArgs)
	}
	stored, err := os.ReadFile(valuePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != base64.StdEncoding.EncodeToString(key)+"\n" {
		t.Fatalf("key must be base64 on stdin, got %q", string(stored))
	}

	got, err := provider.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load after Store: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("round trip mismatch: got %x, want %x", got, key)
	}
}
