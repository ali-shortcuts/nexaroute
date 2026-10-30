package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Delete removes exactly the asset Put wrote — file plus sidecar
// metadata — and a second delete fails instead of silently succeeding.
func TestLocalDeleteRemovesAssetAndMetadata(t *testing.T) {
	root := t.TempDir()
	sink, err := NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := sink.Put(context.Background(), video.Asset{
		ID: "a-1", Kind: "video", Filename: "a.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, []byte("asset-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Delete(context.Background(), asset); err != nil {
		t.Fatalf("Delete error=%v", err)
	}
	if _, err := os.Stat(asset.URI); !os.IsNotExist(err) {
		t.Fatalf("asset file still present after Delete, stat err=%v", err)
	}
	if _, err := os.Stat(asset.URI + ".json"); !os.IsNotExist(err) {
		t.Fatalf("asset metadata still present after Delete, stat err=%v", err)
	}
	if err := sink.Delete(context.Background(), asset); err == nil {
		t.Fatal("second Delete unexpectedly succeeded on a removed asset")
	}
}

// Delete must refuse a URI outside the root and must not remove the
// outside file it was pointed at.
func TestLocalDeleteRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	sink, err := NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	asset := video.Asset{ID: "x-1", Filename: "outside.mp4", URI: outside}
	if err := sink.Delete(context.Background(), asset); err == nil {
		t.Fatal("Delete unexpectedly accepted an outside-root URI")
	}
	if !strings.Contains(asset.URI, "outside") {
		t.Fatal("test setup lost the outside path")
	}
	kept, err := os.ReadFile(outside)
	if err != nil || string(kept) != "do-not-delete" {
		t.Fatalf("outside file changed by refused Delete: %q err=%v", kept, err)
	}
	if err := sink.Delete(context.Background(), video.Asset{ID: "x-2"}); err == nil {
		t.Fatal("Delete unexpectedly accepted an empty URI")
	}
}
