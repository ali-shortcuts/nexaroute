package secrets

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
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
}
