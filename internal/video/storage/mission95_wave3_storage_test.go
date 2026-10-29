package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Wave-3 storage coverage: direct assetDirectory escapes and
// failures, Open validation branches, NewLocal creation failure,
// and Put rename/metadata failures induced with pre-created
// directories (rename of a file onto a directory fails). The
// episode-".." Put case is already covered by the existing
// TestLocalPutRejectsDotDotMetadataAndSymlinkedDirectories, the
// /proc/self/mem read-error case by the wave-2 suite, and the
// race/TOCTOU/Getwd branches are deliberately not faked.

func wave3ProcSelfAvailable(t *testing.T) {
	t.Helper()
	if st, err := os.Stat("/proc/self"); err != nil || !st.IsDir() {
		t.Skip("read-only /proc layout required for this test is unavailable")
	}
}

func TestWave3NewLocalMkdirAllFails(t *testing.T) {
	wave3ProcSelfAvailable(t)
	if _, err := NewLocal("/proc/self/wave3-missing-dir", 0); err == nil {
		t.Fatal("NewLocal under a read-only tree must fail")
	}
}

func TestWave3AssetDirectoryDotDotEscapesRoot(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assetDirectory([]string{".."}); err == nil || !strings.Contains(err.Error(), "escapes storage root") {
		t.Fatalf("dot-dot asset directory error = %v", err)
	}
}

func TestWave3AssetDirectoryLstatError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewLocal(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Traversing through a regular file makes Lstat fail with
	// ENOTDIR, which is neither "not exist" nor success.
	if _, err := s.assetDirectory([]string{"blocker", "sub"}); err == nil {
		t.Fatal("asset directory through a regular file must fail")
	}
}

func TestWave3AssetDirectoryNotADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "afile"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewLocal(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assetDirectory([]string{"afile"}); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file-as-directory error = %v", err)
	}
}

func TestWave3AssetDirectoryDanglingRootSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), link); err != nil {
		t.Fatal(err)
	}
	// Bypass NewLocal (which requires a real directory) to reach the
	// EvalSymlinks branch with a dangling root.
	s := &Local{Root: link}
	if _, err := s.assetDirectory(nil); err == nil {
		t.Fatal("dangling root symlink must fail assetDirectory")
	}
}

func TestWave3OpenUnconfiguredRoot(t *testing.T) {
	s := &Local{}
	if _, err := s.Open(video.Asset{URI: "/tmp/x"}); err == nil || !strings.Contains(err.Error(), "storage root is not configured") {
		t.Fatalf("unconfigured root error = %v", err)
	}
}

func TestWave3OpenDanglingRoot(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), link); err != nil {
		t.Fatal(err)
	}
	s := &Local{Root: link}
	// The URI must be lexically under the root so the lexical escape
	// check passes and the EvalSymlinks root failure is reached.
	if _, err := s.Open(video.Asset{URI: filepath.Join(link, "x")}); err == nil {
		t.Fatal("dangling root must fail Open")
	}
}

func TestWave3OpenDanglingPathInsideRealRoot(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing-target"), link); err != nil {
		t.Fatal(err)
	}
	asset := video.Asset{Filename: "clip.mp4", Metadata: map[string]string{"project_id": "p1", "job_id": "j1"}, URI: link}
	if _, err := s.Open(asset); err == nil {
		t.Fatal("dangling asset path must fail Open")
	}
}

func TestWave3OpenDirectoryIsNotRegular(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "subdir")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(video.Asset{URI: sub}); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("directory asset error = %v", err)
	}
}

func TestWave3OpenExceedsSizeLimit(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "big.bin")
	if err := os.WriteFile(path, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(video.Asset{URI: path}); err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("oversize asset error = %v", err)
	}
}

func TestWave3WriteMetadataAtomicTempCreationFails(t *testing.T) {
	wave3ProcSelfAvailable(t)
	// CreateTemp returns the raw filesystem error (read-only tree).
	if err := writeMetadataAtomic("/proc/self/meta.json", []byte(`{"filename":"clip.mp4"}`)); err == nil {
		t.Fatal("metadata temp creation under a read-only tree must fail")
	}
}

func TestWave3WriteMetadataAtomicRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "meta.json")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadataAtomic(target, []byte(`{"filename":"clip.mp4"}`)); err == nil {
		t.Fatal("metadata rename onto a directory must fail")
	}
}

func TestWave3PutRenameOntoDirectoryFails(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	assetDir := filepath.Join(root, "p1", "j1")
	if err := os.MkdirAll(assetDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(assetDir, "clip.mp4"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p1", "job_id": "j1"},
	}, []byte("data"))
	if err == nil {
		t.Fatal("Put rename onto a directory must fail")
	}
}

func TestWave3PutMetadataRenameOntoDirectoryFails(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	assetDir := filepath.Join(root, "p1", "j1")
	if err := os.MkdirAll(assetDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(assetDir, "clip.mp4.json"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p1", "job_id": "j1"},
	}, []byte("data"))
	if err == nil {
		t.Fatal("Put metadata rename onto a directory must fail")
	}
}

func TestWave3PutUnderProcRootFails(t *testing.T) {
	wave3ProcSelfAvailable(t)
	// NewLocal itself succeeds on /proc/self (it exists), but Put
	// cannot create its asset directory there.
	s, err := NewLocal("/proc/self", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p1", "job_id": "j1"},
	}, []byte("data"))
	if err == nil {
		t.Fatal("Put under a read-only root must fail")
	}
}
