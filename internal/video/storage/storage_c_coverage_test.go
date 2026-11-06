package storage

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Agent C coverage: the long-path CreateTemp failure, the file-size
// write failure, the deleted-CWD path failures, the symlink-loop and
// outside-root deletions, and the sidecar removal failures.

func withDeletedCWD(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sub); err != nil {
		_ = os.Chdir(orig)
		t.Fatalf("remove current directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// longDir builds a directory path of exactly target bytes under
// t.TempDir, using nested components that each fit NAME_MAX.
func longDir(t *testing.T, target int) string {
	t.Helper()
	path := t.TempDir()
	for len(path) < target {
		need := target - len(path)
		comp := need - 1
		if comp > 200 {
			comp = 200
		}
		if comp < 1 {
			break
		}
		path += "/" + strings.Repeat("d", comp)
	}
	if len(path) != target {
		t.Fatalf("directory length = %d, want %d", len(path), target)
	}
	return path
}

func putTestAsset(t *testing.T, s *Local, filename string, data []byte) video.Asset {
	t.Helper()
	asset, err := s.Put(context.Background(), video.Asset{
		ID: "a-1", Kind: "video", Filename: filename, ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "episode_id": "e", "job_id": "j"},
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestLocalPutCreateTempFailsOnLongPath(t *testing.T) {
	// The asset directory is 4090 bytes (root 4084 + "/p/e/j"), so the
	// directory walk succeeds but the temp file (dir + "/.tmp-" +
	// random digits) exceeds PATH_MAX.
	root := longDir(t, 4084)
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		ID: "a-1", Kind: "video", Filename: "clip.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "episode_id": "e", "job_id": "j"},
	}, []byte("clip"))
	if err == nil || !strings.Contains(err.Error(), "file name too long") {
		t.Fatalf("long-path Put err = %v, want the file-name-too-long error", err)
	}
}

func TestLocalPutTempWriteFailsOnFileSizeLimit(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	_, err = s.Put(context.Background(), video.Asset{
		ID: "a-1", Kind: "video", Filename: "clip.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "episode_id": "e", "job_id": "j"},
	}, []byte("more than one byte of clip data"))
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("size-limit Put err = %v, want the file-too-large error", err)
	}
}

func TestWriteMetadataAtomicCreateTempFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadataAtomic(filepath.Join(blocker, "meta.json"), []byte("{}")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("writeMetadataAtomic err = %v, want the not-a-directory error", err)
	}
}

func TestWriteMetadataAtomicWriteFailsOnFileSizeLimit(t *testing.T) {
	dir := t.TempDir()
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	if err := writeMetadataAtomic(filepath.Join(dir, "meta.json"), []byte("more than one byte")); err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("writeMetadataAtomic err = %v, want the file-too-large error", err)
	}
}

func TestLocalDeleteNilRoot(t *testing.T) {
	var s *Local
	if err := s.Delete(context.Background(), video.Asset{URI: "x.mp4"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil Delete err = %v, want the not-configured error", err)
	}
}

func TestLocalDeleteAbsFailsWithDeletedCWD(t *testing.T) {
	withDeletedCWD(t)
	s := &Local{Root: "relative/root"}
	if err := s.Delete(context.Background(), video.Asset{URI: "a.mp4"}); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("deleted-CWD Delete root err = %v, want the Getwd failure", err)
	}
}

func TestLocalDeletePathAbsFailsWithDeletedCWD(t *testing.T) {
	root := t.TempDir()
	withDeletedCWD(t)
	s := &Local{Root: root}
	if err := s.Delete(context.Background(), video.Asset{URI: "relative/a.mp4"}); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("deleted-CWD Delete path err = %v, want the Getwd failure", err)
	}
}

func TestLocalDeleteEvalSymlinksRootFails(t *testing.T) {
	s := &Local{Root: filepath.Join(t.TempDir(), "missing")}
	asset := video.Asset{URI: filepath.Join(s.Root, "a.mp4")}
	if err := s.Delete(context.Background(), asset); err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing-root Delete err = %v, want the not-exist error", err)
	}
}

func TestLocalDeleteSymlinkLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "b"), filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	s := &Local{Root: root}
	asset := video.Asset{URI: filepath.Join(root, "a")}
	if err := s.Delete(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "too many links") {
		t.Fatalf("symlink-loop Delete err = %v, want the too-many-links error", err)
	}
}

func TestLocalDeleteSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "real.mp4")
	if err := os.WriteFile(outside, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	s := &Local{Root: root}
	asset := video.Asset{URI: filepath.Join(root, "link.mp4")}
	if err := s.Delete(context.Background(), asset); err == nil || !IsOutsideRoot(err) {
		t.Fatalf("outside-symlink Delete err = %v, want the outside-root error", err)
	}
}

func TestLocalDeleteRefusesDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Local{Root: root}
	asset := video.Asset{URI: dir}
	if err := s.Delete(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory Delete err = %v, want the not-a-regular-file error", err)
	}
}

func TestLocalDeleteRemoveFailsOnReadOnlyDir(t *testing.T) {
	// chmod does not affect root, so this only runs as a non-root user
	// (CI); it is verified locally by running the package tests as the
	// nobody user.
	if os.Geteuid() == 0 {
		t.Skip("chmod-based removal failure does not affect root")
	}
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	asset := putTestAsset(t, s, "clip.mp4", []byte("clip"))
	if err := os.Chmod(filepath.Dir(asset.URI), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(asset.URI), 0o700) })
	if err := s.Delete(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("read-only Delete err = %v, want the permission error", err)
	}
}

func TestLocalDeleteSidecarRemoveFails(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	asset := putTestAsset(t, s, "clip.mp4", []byte("clip"))
	// Replace the sidecar with a non-empty directory: the asset removal
	// succeeds, but removing the sidecar must fail.
	if err := os.Remove(asset.URI + ".json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(asset.URI+".json", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asset.URI+".json", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), asset); err == nil || !strings.Contains(err.Error(), "directory not empty") {
		t.Fatalf("sidecar Delete err = %v, want the directory-not-empty error", err)
	}
}

func TestLocalOpenAbsFailsWithDeletedCWD(t *testing.T) {
	withDeletedCWD(t)
	s := &Local{Root: "relative/root"}
	if _, err := s.Open(video.Asset{URI: "a.mp4"}); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("deleted-CWD Open root err = %v, want the Getwd failure", err)
	}
}

func TestLocalOpenPathAbsFailsWithDeletedCWD(t *testing.T) {
	root := t.TempDir()
	withDeletedCWD(t)
	s := &Local{Root: root}
	if _, err := s.Open(video.Asset{URI: "relative/a.mp4"}); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("deleted-CWD Open path err = %v, want the Getwd failure", err)
	}
}

func TestLocalOpenFailsOnUnreadableFile(t *testing.T) {
	// chmod does not affect root, so this only runs as a non-root user
	// (CI); it is verified locally by running the package tests as the
	// nobody user.
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadability does not affect root")
	}
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	asset := putTestAsset(t, s, "clip.mp4", []byte("clip"))
	if err := os.Chmod(asset.URI, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(asset); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unreadable Open err = %v, want the permission error", err)
	}
}
