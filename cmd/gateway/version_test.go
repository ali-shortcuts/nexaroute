package main

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/buildinfo"
)

// The CLI version literal in main.go is scraped by the CI installer smoke job
// and is also what `nexaroute --version` prints. The HTTP surfaces report
// internal/buildinfo.Version instead, so this test is the guard that keeps the
// two from drifting apart (they did once: the CLI said 0.5.0 while /api/hello
// and the dashboard said 0.4.1).
func TestVersionMatchesBuildinfo(t *testing.T) {
	if version != buildinfo.Version {
		t.Fatalf("CLI version %q does not match buildinfo.Version %q", version, buildinfo.Version)
	}
}
