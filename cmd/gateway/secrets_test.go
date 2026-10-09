package main

import (
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestSecretsCommands(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	p := filepath.Join(t.TempDir(), "config.json")
	c := config.Default()
	c.Providers = []config.ProviderConfig{{ID: "private", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "cli-canary", Enabled: false}}
	if e := config.SaveAtomic(p, c); e != nil {
		t.Fatal(e)
	}
	if e := runSecrets([]string{"status", "--config", p}); e != nil {
		t.Fatal(e)
	}
	if e := runSecrets([]string{"verify", "--config", p}); e != nil {
		t.Fatal(e)
	}
	if e := runSecrets([]string{"decrypt", "--config", p}); e == nil {
		t.Fatal("decrypt without explicit flags succeeded")
	}
	old, _ := os.ReadFile(p + ".key")
	if e := runSecrets([]string{"rotate", "--config", p}); e != nil {
		t.Fatal(e)
	}
	now, _ := os.ReadFile(p + ".key")
	if string(old) == string(now) {
		t.Fatal("rotation did not replace master key")
	}
	if _, e := os.Stat(p + ".key.previous"); e != nil {
		t.Fatal("recovery key missing")
	}
	if e := runSecrets([]string{"verify", "--config", p}); e != nil {
		t.Fatal(e)
	}
	if e := runSecrets([]string{"unknown", "--config", p}); e == nil {
		t.Fatal("unknown command accepted")
	}
}
