package composer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightRejectsMissingFFmpegExecutable(t *testing.T) {
	runner := New(filepath.Join(t.TempDir(), "missing-ffmpeg"), "")
	if err := runner.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Preflight error=%v, want missing executable error", err)
	}
}
