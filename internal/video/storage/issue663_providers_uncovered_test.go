package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func TestIssue663ProvidersPutValidationAndUnsafeSegments(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Missing project/job metadata must be rejected before any file is written.
	if _, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"episode_id": "e1"}}, []byte("x")); err == nil {
		t.Fatal("Put without project_id/job_id was accepted")
	}
	// An episode segment that sanitizes to ".." is an unsafe path segment.
	if _, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "p1", "episode_id": "..", "job_id": "j1"}}, []byte("x")); err == nil || !strings.Contains(err.Error(), "unsafe path segment") {
		t.Fatalf("Put with episode '..' err=%v, want unsafe path segment", err)
	}
	// A project segment that trims to nothing is the missing-metadata error.
	if _, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "..", "job_id": "j1"}}, []byte("x")); err == nil {
		t.Fatal("Put with project '..' was accepted")
	}
	// Oversize data is rejected.
	if _, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "p1", "job_id": "j1"}}, make([]byte, 65)); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize Put err=%v, want size limit", err)
	}
}

func TestIssue663ProvidersAssetDirectorySymlinkAndFileSegments(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A symlink directory segment must not let an asset escape the root.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "linked", "job_id": "j1"}}, []byte("x")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Put through symlinked project dir err=%v, want symlink rejection", err)
	}
	// A regular file where a directory segment belongs is rejected.
	if err := os.WriteFile(filepath.Join(s.Root, "afile"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "afile", "job_id": "j1"}}, []byte("x")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Put through file segment err=%v, want not-a-directory rejection", err)
	}
	// An empty episode segment is skipped, not created as a directory.
	stored, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "p1", "job_id": "j1"}}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(stored.URI); got != filepath.Join(s.Root, "p1", "j1") {
		t.Fatalf("empty episode dir = %q, want %q", got, filepath.Join(s.Root, "p1", "j1"))
	}
}

func TestIssue663ProvidersOpenRejectsOutsideRootAndSymlinkEscape(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// A path outside the root is an outside-root rejection, not a generic error.
	if _, err := s.Open(video.Asset{URI: filepath.Join(s.Root, "..", "escape.mp4")}); !errors.Is(err, ErrAssetOutsideRoot) {
		t.Fatalf("outside-root Open err=%v, want ErrAssetOutsideRoot", err)
	}
	if !IsOutsideRoot(ErrAssetOutsideRoot) || !IsOutsideRoot(ErrAssetSymlinkOutsideRoot) || IsOutsideRoot(errors.New("other")) {
		t.Fatal("IsOutsideRoot classification is wrong")
	}
	// A symlink inside the root pointing outside is the symlink variant.
	outside := filepath.Join(t.TempDir(), "real.mp4")
	if err := os.WriteFile(outside, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.Root, "link.mp4")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(video.Asset{URI: link}); !errors.Is(err, ErrAssetSymlinkOutsideRoot) {
		t.Fatalf("symlink-escape Open err=%v, want ErrAssetSymlinkOutsideRoot", err)
	}
	// A directory inside the root is not a regular asset file.
	subdir := filepath.Join(s.Root, "adir")
	if err := os.MkdirAll(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(video.Asset{URI: subdir}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory Open err=%v, want not-a-regular-file", err)
	}
	// A nil store and an unconfigured root fail closed.
	var nilStore *Local
	if _, err := nilStore.Open(video.Asset{URI: "x"}); err == nil {
		t.Fatal("nil store Open was accepted")
	}
	if _, err := (&Local{}).Open(video.Asset{URI: "x"}); err == nil {
		t.Fatal("unconfigured store Open was accepted")
	}
}

func TestIssue663ProvidersOpenAndReadEnforceSizeAndChecksum(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stored, err := s.Put(ctx, video.Asset{Filename: "a.mp4", Metadata: map[string]string{"project_id": "p1", "job_id": "j1"}}, []byte("1234"))
	if err != nil {
		t.Fatal(err)
	}
	// The stored file exceeds a tighter limit when read through a capped store.
	capped, err := NewLocal(s.Root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capped.Open(stored); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("capped Open err=%v, want size limit", err)
	}
	// A tampered record size is caught by Read, not silently served.
	tampered := stored
	tampered.SizeBytes = 99
	if _, err := s.Read(tampered); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("size-tampered Read err=%v, want size mismatch", err)
	}
	// A tampered checksum is caught by Read.
	tampered = stored
	tampered.SHA256 = strings.Repeat("0", 64)
	if _, err := s.Read(tampered); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum-tampered Read err=%v, want checksum mismatch", err)
	}
	// A legacy record without a checksum still reads.
	legacy := stored
	legacy.SHA256 = ""
	legacy.SizeBytes = 0
	got, err := s.Read(legacy)
	if err != nil || string(got) != "1234" {
		t.Fatalf("legacy Read=%q err=%v", got, err)
	}
}
