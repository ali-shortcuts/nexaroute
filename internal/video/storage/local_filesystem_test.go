package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func TestNewLocalValidatesRootAndMaxSize(t *testing.T) {
	if _, err := NewLocal("", 1); err == nil {
		t.Fatal("empty root was accepted")
	}
	if _, err := NewLocal("   ", 1); err == nil {
		t.Fatal("whitespace root was accepted")
	}
	if _, err := NewLocal(t.TempDir(), -1); err == nil {
		t.Fatal("negative max size was accepted")
	}
	root := filepath.Join(t.TempDir(), "new", "root")
	s, err := NewLocal(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.Root) {
		t.Fatalf("Root=%q is not absolute", s.Root)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root was not created: %v", err)
	}
}

func TestLocalPutPersistsSanitizedAssetAndMetadata(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("video bytes")
	asset, err := s.Put(context.Background(), video.Asset{
		ID: "asset-1", Kind: "video", Filename: "../../clip name?.mp4",
		ContentType: "video/mp4", Metadata: map[string]string{"project_id": "project/one", "episode_id": "episode 1", "job_id": "job:1"},
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(data)
	if asset.SHA256 != hex.EncodeToString(wantSum[:]) || asset.SizeBytes != int64(len(data)) {
		t.Fatalf("checksum/size not recorded: %#v", asset)
	}
	if strings.Contains(filepath.Base(asset.URI), " ") || strings.Contains(filepath.Base(asset.URI), "/") {
		t.Fatalf("filename was not sanitized: %q", asset.Filename)
	}
	if !strings.HasPrefix(asset.URI, s.Root+string(os.PathSeparator)) {
		t.Fatalf("asset URI escaped root: %q", asset.URI)
	}
	read, err := s.Read(asset)
	if err != nil || string(read) != string(data) {
		t.Fatalf("Read=%q err=%v", read, err)
	}
	metaBytes, err := os.ReadFile(asset.URI + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var saved video.Asset
	if err := json.Unmarshal(metaBytes, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.URI != asset.URI || saved.SHA256 != asset.SHA256 || saved.Metadata["project_id"] != "project/one" {
		t.Fatalf("metadata mismatch: %#v", saved)
	}
	entries, err := os.ReadDir(filepath.Dir(asset.URI))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") || strings.HasPrefix(entry.Name(), ".meta-") {
			t.Fatalf("temporary file was left behind: %s", entry.Name())
		}
	}
}

func TestLocalPutValidatesSizeAndMetadataAndFallbackFilename(t *testing.T) {
	s, err := NewLocal(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	base := video.Asset{Filename: "ok.bin", Metadata: map[string]string{"project_id": "p", "job_id": "j"}}
	if _, err := s.Put(context.Background(), base, []byte("1234")); err == nil {
		t.Fatal("oversized asset was accepted")
	}
	for _, metadata := range []map[string]string{
		nil,
		{"project_id": "...", "job_id": "j"},
		{"project_id": "p", "job_id": "---"},
	} {
		asset := base
		asset.Metadata = metadata
		if _, err := s.Put(context.Background(), asset, []byte("x")); err == nil {
			t.Fatalf("missing/invalid metadata accepted: %#v", metadata)
		}
	}
	unlimited, err := NewLocal(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := unlimited.Put(context.Background(), video.Asset{Filename: "../", Metadata: map[string]string{"project_id": "p", "job_id": "j"}}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if asset.Filename != "asset.bin" {
		t.Fatalf("fallback filename=%q", asset.Filename)
	}
}

func TestLocalReadRejectsOutsideAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocal(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(video.Asset{URI: outside}); err == nil || !strings.Contains(err.Error(), "escapes storage root") {
		t.Fatalf("outside read error=%v", err)
	}
	insideDir := filepath.Join(root, "p", "j")
	if err := os.MkdirAll(insideDir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(insideDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(video.Asset{URI: link}); err == nil || !strings.Contains(err.Error(), "symlink escapes storage root") {
		t.Fatalf("symlink read error=%v", err)
	}
	if _, err := s.Read(video.Asset{URI: root}); err == nil || !strings.Contains(err.Error(), "escapes storage root") {
		t.Fatalf("root directory read error=%v", err)
	}
	if _, err := s.Read(video.Asset{URI: filepath.Join(root, "missing")}); err == nil {
		t.Fatal("missing asset unexpectedly read")
	} else if errors.Is(err, os.ErrNotExist) {
		// EvalSymlinks reports the expected not-found error before os.ReadFile.
	} else {
		t.Fatalf("unexpected missing asset error=%v", err)
	}
}
