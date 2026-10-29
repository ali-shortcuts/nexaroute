package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Mission-95 gap coverage: filesystem failure and validation branches of
// the local asset store that the earlier suite did not reach. Every test
// asserts the failure mode (or the surviving record), not just execution.

func TestMission95NewLocalRejectsRootUnderAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(filepath.Join(blocker, "sub"), 0); err == nil {
		t.Fatal("root below a regular file was accepted")
	}
}

func TestMission95PutRenameFailureLeavesNoAssetFile(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root, "p", "j")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Pre-create a directory exactly where the asset file would land:
	// renaming the temp file onto it must fail.
	if err := os.Mkdir(filepath.Join(dir, "clip.mp4"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, []byte("clip"))
	if err == nil {
		t.Fatal("rename onto an existing directory succeeded")
	}
	if info, statErr := os.Stat(filepath.Join(dir, "clip.mp4")); statErr != nil || !info.IsDir() {
		t.Fatalf("pre-created directory was disturbed: info=%v err=%v", info, statErr)
	}
}

func TestMission95PutMetadataRenameFailure(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root, "p", "j")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// The asset file itself can land, but its sidecar metadata path is
	// blocked by a directory, so the metadata rename must fail the Put.
	if err := os.Mkdir(filepath.Join(dir, "clip.mp4.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, []byte("clip")); err == nil {
		t.Fatal("metadata rename onto an existing directory succeeded")
	}
}

func TestMission95PutRejectsFileAsDirectoryComponent(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "p"), []byte("file, not dir"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file-as-directory error=%v", err)
	}
}

func TestMission95PutRejectsOverlongPathComponent(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": strings.Repeat("a", 300), "job_id": "j"},
	}, []byte("x"))
	if err == nil {
		t.Fatal("overlong path component was accepted")
	}
}

func TestMission95OpenRejectsUnconfiguredAndDanglingRoots(t *testing.T) {
	var nilStore *Local
	if _, err := nilStore.Open(video.Asset{URI: "/x"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil store open error=%v", err)
	}
	if _, err := (&Local{}).Open(video.Asset{URI: "/x"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("empty store open error=%v", err)
	}
	root := t.TempDir()
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing-target"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Local{Root: dangling}).Open(video.Asset{URI: filepath.Join(dangling, "x")}); err == nil {
		t.Fatal("open through a dangling root symlink succeeded")
	}
}

func TestMission95ReadPropagatesReadErrors(t *testing.T) {
	// /proc/self/mem opens and stats as a regular file but every read
	// fails with EIO, which exercises Read's io.ReadAll error return
	// without any race: the store root is a symlink to /proc/self, so
	// the resolved asset stays inside the resolved root.
	root := t.TempDir()
	link := filepath.Join(root, "proclink")
	if err := os.Symlink("/proc/self", link); err != nil {
		t.Fatal(err)
	}
	s := &Local{Root: link}
	if _, err := s.Read(video.Asset{URI: filepath.Join(link, "mem")}); err == nil {
		t.Fatal("read from an unreadable regular file succeeded")
	}
}

func TestMission95OpenRejectsDirectoryAndOversizeFiles(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Put(context.Background(), video.Asset{
		Filename: "clip.mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, []byte("eleven bytes!"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(video.Asset{URI: filepath.Dir(stored.URI)}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory open error=%v", err)
	}
	limited := &Local{Root: s.Root, MaxBytes: 3}
	if _, err := limited.Open(stored); err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("oversize open error=%v", err)
	}
	// The same limit must not reject a file that fits it.
	fitting := &Local{Root: s.Root, MaxBytes: int64(len("eleven bytes!"))}
	f, err := fitting.Open(stored)
	if err != nil {
		t.Fatalf("fitting file rejected: %v", err)
	}
	_ = f.Close()
}
