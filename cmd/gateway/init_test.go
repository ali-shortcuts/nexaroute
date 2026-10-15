package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M6-5 wizard: flag mode asks nothing; piped answers drive the
// prompts; existing files are protected; the written config
// loads, validates, and carries no key value.

func TestInitFlagMode(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nexaroute.json")
	var stdout, stderr bytes.Buffer
	code := runInit([]string{
		"--out", out,
		"--base-url", "https://api.example.com",
		"--api-key-env", "EXAMPLE_API_KEY",
		"--model", "example-model-1",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	cfg, err := config.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].BaseURL != "https://api.example.com" {
		t.Fatalf("providers: %+v", cfg.Providers)
	}
	if cfg.Providers[0].APIKeyEnv != "EXAMPLE_API_KEY" || cfg.Providers[0].APIKey != "" {
		t.Fatalf("key handling wrong: env=%q key=%q", cfg.Providers[0].APIKeyEnv, cfg.Providers[0].APIKey)
	}
	if len(cfg.Providers[0].Models) != 1 || cfg.Providers[0].Models[0].Model != "example-model-1" {
		t.Fatalf("models: %+v", cfg.Providers[0].Models)
	}
	raw, _ := os.ReadFile(out)
	if strings.Contains(string(raw), "sk-") {
		t.Fatal("config contains a key value")
	}
}

func TestInitPipedAnswersAndDefaults(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nexaroute.json")
	var stdout, stderr bytes.Buffer
	// Answers: base URL, key env, model, listen — last one blank
	// (default). Blank first answer would also take the default;
	// here we customize the model.
	answers := "https://api.anthropic.com\nANTHROPIC_API_KEY\nclaude-sonnet-4-5\n\n"
	code := runInit([]string{"--out", out, "--type", "anthropic_compatible"}, strings.NewReader(answers), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s (stdout: %s)", code, stderr.String(), stdout.String())
	}
	cfg, err := config.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].Type != "anthropic_compatible" || cfg.Providers[0].Models[0].Model != "claude-sonnet-4-5" {
		t.Fatalf("piped answers not applied: %+v", cfg.Providers[0])
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Fatalf("blank listen answer should take the default, got %q", cfg.Listen)
	}
	// All-blank answers (just newlines / EOF) take every default.
	out2 := filepath.Join(t.TempDir(), "nexaroute.json")
	stdout.Reset()
	stderr.Reset()
	if code := runInit([]string{"--out", out2}, strings.NewReader("\n\n\n\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("defaults run exit %d: %s", code, stderr.String())
	}
	cfg2, err := config.Load(out2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Providers[0].Models[0].Model != "gpt-4o-mini" {
		t.Fatalf("default model: %+v", cfg2.Providers[0].Models)
	}
}

func TestInitRefusesOverwriteAndBadInput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nexaroute.json")
	if err := os.WriteFile(out, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runInit([]string{"--out", out, "--base-url", "https://x.example", "--api-key-env", "K", "--model", "m"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("overwrite without --force: exit %d, want 1", code)
	}
	if code := runInit([]string{"--out", out, "--force", "--base-url", "https://x.example", "--api-key-env", "K", "--model", "m"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("forced overwrite: exit %d: %s", code, stderr.String())
	}
	// Invalid provider type fails validation; nothing is written.
	out2 := filepath.Join(t.TempDir(), "nexaroute.json")
	if code := runInit([]string{"--out", out2, "--type", "bogus_type", "--base-url", "https://x.example", "--api-key-env", "K", "--model", "m"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("bogus type: exit %d, want 1", code)
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatal("invalid init wrote a config file")
	}
}
