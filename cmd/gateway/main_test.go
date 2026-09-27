package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := defaultConfigPath(), filepath.Join(home, ".config/nexaroute/config.json"); got != want {
		t.Fatalf("%q != %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if got, want := defaultConfigPath(), filepath.Join(home, "xdg/nexaroute/config.json"); got != want {
		t.Fatalf("%q != %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	if got := defaultConfigPath(); got != "" {
		t.Fatalf("relative XDG path accepted: %s", got)
	}
	t.Setenv("NEXAROUTE_CONFIG", "/explicit/config.json")
	if got := defaultConfigPath(); got != "/explicit/config.json" {
		t.Fatal(got)
	}
}

func TestEnsureConfigPreservesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private/config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	data, _ := os.ReadFile(path)
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(data) != string(after) {
		t.Fatal("config changed")
	}
}

func TestUIURL(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{{"0.0.0.0", "http://127.0.0.1:8080/"}, {"::", "http://[::1]:8080/"}, {"127.0.0.1", "http://127.0.0.1:8080/"}} {
		if got := uiURL(&net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 8080}); got != tc.want {
			t.Fatalf("got %s", got)
		}
	}
}
