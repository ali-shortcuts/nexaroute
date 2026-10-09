package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var safeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

type Local struct {
	Root     string
	MaxBytes int64
}

func NewLocal(root string, max int64) (*Local, error) {
	if root == "" {
		return nil, fmt.Errorf("storage root is required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Local{Root: root, MaxBytes: max}, nil
}
func (s *Local) Put(_ context.Context, a video.Asset, data []byte) (video.Asset, error) {
	if s.MaxBytes > 0 && int64(len(data)) > s.MaxBytes {
		return video.Asset{}, fmt.Errorf("asset exceeds size limit")
	}
	name := safeName.ReplaceAllString(filepath.Base(a.Filename), "_")
	if name == "" || name == "." || name == ".." {
		name = "asset.bin"
	}
	dir := filepath.Join(s.Root, safeName.ReplaceAllString(a.Metadata["project_id"], "_"), safeName.ReplaceAllString(a.Metadata["episode_id"], "_"), safeName.ReplaceAllString(a.Metadata["job_id"], "_"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return video.Asset{}, err
	}
	sum := sha256.Sum256(data)
	a.SHA256 = hex.EncodeToString(sum[:])
	a.SizeBytes = int64(len(data))
	a.Filename = name
	a.URI = filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return video.Asset{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return video.Asset{}, err
	}
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return video.Asset{}, err
	}
	if err = tmp.Close(); err != nil {
		return video.Asset{}, err
	}
	if err = os.Rename(tmpName, a.URI); err != nil {
		return video.Asset{}, err
	}
	meta, _ := json.MarshalIndent(a, "", "  ")
	if err = os.WriteFile(a.URI+".json", meta, 0600); err != nil {
		return video.Asset{}, err
	}
	return a, nil
}
func (s *Local) Read(a video.Asset) ([]byte, error) {
	clean := filepath.Clean(a.URI)
	root, _ := filepath.Abs(s.Root)
	path, _ := filepath.Abs(clean)
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return nil, fmt.Errorf("asset path escapes storage root")
	}
	return os.ReadFile(path)
}

var _ video.AssetSink = (*Local)(nil)
