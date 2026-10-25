package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
)

func TestResolveJobStoreURLBranches(t *testing.T) {
	t.Run("empty is rejected", func(t *testing.T) {
		if _, err := resolveJobStoreURL("  "); err == nil || !strings.Contains(err.Error(), "job_store_url is required") {
			t.Fatalf("resolveJobStoreURL(empty) error=%v", err)
		}
	})
	t.Run("literal DSN is trimmed", func(t *testing.T) {
		got, err := resolveJobStoreURL("  postgres://user:pass@localhost/db  ")
		if err != nil || got != "postgres://user:pass@localhost/db" {
			t.Fatalf("resolveJobStoreURL(literal) = %q, %v", got, err)
		}
	})
	t.Run("missing environment variable is rejected", func(t *testing.T) {
		t.Setenv("NEXAROUTE_RUNTIME_TEST_JOB_DSN", "")
		if _, err := resolveJobStoreURL("env:NEXAROUTE_RUNTIME_TEST_JOB_DSN"); err == nil || !strings.Contains(err.Error(), "unset or empty") {
			t.Fatalf("resolveJobStoreURL(unset env) error=%v", err)
		}
	})
	t.Run("environment indirection returns the configured value", func(t *testing.T) {
		t.Setenv("NEXAROUTE_RUNTIME_TEST_JOB_DSN", "postgres://runtime-test/db")
		got, err := resolveJobStoreURL("env: NEXAROUTE_RUNTIME_TEST_JOB_DSN ")
		if err != nil || got != "postgres://runtime-test/db" {
			t.Fatalf("resolveJobStoreURL(env) = %q, %v", got, err)
		}
	})
}

func TestOpenJobStoreBackendSelectionAndFailurePaths(t *testing.T) {
	t.Run("default file backend", func(t *testing.T) {
		root := t.TempDir()
		store, err := openJobStore(video.Config{}, root)
		if err != nil || store == nil {
			t.Fatalf("openJobStore(default) = %v, %v", store, err)
		}
		// The file store creates its file lazily on the first
		// mutating operation (NewFileStore tolerates a missing file),
		// so persist one job before asserting the resolved path.
		if err := store.Create(context.Background(), video.VideoJob{JobID: "branch-test-job"}); err != nil {
			t.Fatalf("store.Create(default) = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "video-jobs.json")); err != nil {
			t.Fatalf("default store file was not created: %v", err)
		}
	})
	t.Run("explicit relative file path", func(t *testing.T) {
		root := t.TempDir()
		store, err := openJobStore(video.Config{JobStoreBackend: "file", StorePath: "nested/jobs.json"}, root)
		if err != nil || store == nil {
			t.Fatalf("openJobStore(file) = %v, %v", store, err)
		}
		// Same lazy-creation behavior as the default backend: the
		// nested directory is created by persist, not by open.
		if err := store.Create(context.Background(), video.VideoJob{JobID: "branch-test-job"}); err != nil {
			t.Fatalf("store.Create(file) = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "nested", "jobs.json")); err != nil {
			t.Fatalf("relative store path not resolved under base directory: %v", err)
		}
	})
	t.Run("postgres without URL fails before connecting", func(t *testing.T) {
		if _, err := openJobStore(video.Config{JobStoreBackend: "postgres"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "job_store_url is required") {
			t.Fatalf("openJobStore(postgres without URL) error=%v", err)
		}
	})
	t.Run("unsupported backend fails closed", func(t *testing.T) {
		if _, err := openJobStore(video.Config{JobStoreBackend: "redis"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "must be file or postgres") {
			t.Fatalf("openJobStore(unsupported) error=%v", err)
		}
	})
}

func TestOpenCostLedgerBackendSelectionAndFailurePaths(t *testing.T) {
	t.Run("file backend uses the in-memory legacy ledger", func(t *testing.T) {
		ledger, closer, err := openCostLedger(video.Config{JobStoreBackend: "file"})
		if err != nil || closer != nil {
			t.Fatalf("openCostLedger(file) closer=%v err=%v", closer, err)
		}
		if _, ok := ledger.(*cost.Ledger); !ok {
			t.Fatalf("openCostLedger(file) type=%T, want *cost.Ledger", ledger)
		}
	})
	t.Run("postgres without URL fails before connecting", func(t *testing.T) {
		ledger, closer, err := openCostLedger(video.Config{JobStoreBackend: "postgres"})
		if err == nil || ledger != nil || closer != nil || !strings.Contains(err.Error(), "job_store_url is required") {
			t.Fatalf("openCostLedger(postgres without URL) = %T, %v, %v", ledger, closer, err)
		}
	})
	t.Run("unsupported backend fails closed", func(t *testing.T) {
		ledger, closer, err := openCostLedger(video.Config{JobStoreBackend: "sqlite"})
		if err == nil || ledger != nil || closer != nil || !strings.Contains(err.Error(), "must be file or postgres") {
			t.Fatalf("openCostLedger(unsupported) = %T, %v, %v", ledger, closer, err)
		}
	})
}

func TestRuntimeNewRejectsInvalidOptionalProviderAndAssetConfig(t *testing.T) {
	const tokenEnv = "NEXAROUTE_RUNTIME_BRANCH_TEST_TOKEN"
	t.Setenv(tokenEnv, "runtime-branch-test-token")

	t.Run("no provider configured", func(t *testing.T) {
		_, err := New(video.Config{Enabled: true, AuthTokenEnv: tokenEnv}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "no provider is configured") {
			t.Fatalf("New(no provider) error=%v", err)
		}
	})
	t.Run("negative asset size is rejected", func(t *testing.T) {
		_, err := New(video.Config{
			Enabled: true, AuthTokenEnv: tokenEnv, DevelopmentFakeProvider: true,
			MaxAssetBytes: -1,
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "max_asset_bytes must be greater than zero") {
			t.Fatalf("New(negative max asset size) error=%v", err)
		}
	})
	t.Run("malformed ComfyUI URL is rejected", func(t *testing.T) {
		_, err := New(video.Config{
			Enabled: true, AuthTokenEnv: tokenEnv, DevelopmentFakeProvider: true,
			ComfyUIEnabled: true, ComfyUIBaseURL: "file:///tmp/workflow",
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "absolute HTTP(S) URL") {
			t.Fatalf("New(invalid ComfyUI URL) error=%v", err)
		}
	})
	t.Run("remote cleartext ComfyUI URL is rejected", func(t *testing.T) {
		_, err := New(video.Config{
			Enabled: true, AuthTokenEnv: tokenEnv, DevelopmentFakeProvider: true,
			ComfyUIEnabled: true, ComfyUIBaseURL: "http://example.com",
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "only allowed on loopback") {
			t.Fatalf("New(remote cleartext ComfyUI URL) error=%v", err)
		}
	})
	t.Run("missing Piper model path is rejected", func(t *testing.T) {
		_, err := New(video.Config{
			Enabled: true, AuthTokenEnv: tokenEnv, DevelopmentFakeProvider: true,
			PiperEnabled: true,
		}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "piper_model_path is required") {
			t.Fatalf("New(Piper without model path) error=%v", err)
		}
	})
}
