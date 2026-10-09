package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestMainVersionAndSecretsSubcommand(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"nexaroute", "--version"}
	versionOutput := captureGatewayStdout(t, main)
	if !strings.Contains(versionOutput, "NexaRoute v") {
		t.Fatalf("version output=%q", versionOutput)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveAtomic(path, config.Default()); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"nexaroute", "secrets", "status", "--config", path}
	statusOutput := captureGatewayStdout(t, main)
	if !strings.Contains(statusOutput, "encrypted_fields=0") || strings.Contains(statusOutput, "failed") {
		t.Fatalf("secrets subcommand output=%q", statusOutput)
	}
}

func TestWriteRotateFileRejectsUnwritableParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "key")
	if err := writeRotateFile(path, []byte("secret")); err == nil {
		t.Fatal("write succeeded without a parent directory")
	}
}
