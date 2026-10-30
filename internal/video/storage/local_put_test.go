package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Put must be atomic from the caller's perspective. If metadata writing
// fails after the video bytes are renamed into place, Put cannot return
// the asset, so a provider rollback cannot identify it. Leaving the
// bytes behind would orphan an asset whose size/checksum were never
// recorded. Force the sidecar path to be a directory so the metadata
// rename fails deterministically.
func TestLocalPutRollsBackVideoWhenMetadataWriteFails(t *testing.T) {
	root := t.TempDir()
	sink, err := NewLocal(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "p", "j")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	videoPath := filepath.Join(dir, "asset.mp4")
	if err := os.Mkdir(videoPath+".json", 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = sink.Put(context.Background(), video.Asset{
		ID: "a-1", Kind: "video", Filename: "asset.mp4", ContentType: "video/mp4",
		Metadata: map[string]string{"project_id": "p", "job_id": "j"},
	}, []byte("asset-bytes"))
	if err == nil {
		t.Fatal("Put unexpectedly succeeded although the metadata sidecar could not be written")
	}
	if _, statErr := os.Stat(videoPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed Put left orphaned video bytes behind: stat err=%v", statErr)
	}
}
