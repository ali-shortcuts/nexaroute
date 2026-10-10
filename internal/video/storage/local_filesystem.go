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
	Root string
	MaxBytes int64
}

func NewLocal(root string, max int64) (*Local, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("storage root is required")
	}
	if max < 0 {
		return nil, fmt.Errorf("max asset size cannot be negative")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Local{Root: abs, MaxBytes: max}, nil
}

func (s *Local) Put(_ context.Context, a video.Asset, data []byte) (video.Asset, error) {
	if s.MaxBytes > 0 && int64(len(data)) > s.MaxBytes {
		return video.Asset{}, fmt.Errorf("asset exceeds size limit")
	}
	name := safeName.ReplaceAllString(filepath.Base(a.Filename), "_")
	if name == "" || name == "." || name == ".." {
		name = "asset.bin"
	}
	projectID := safeName.ReplaceAllString(a.Metadata["project_id"], "_")
	episodeID := safeName.ReplaceAllString(a.Metadata["episode_id"], "_")
	jobID := safeName.ReplaceAllString(a.Metadata["job_id"], "_")
	if strings.Trim(projectID, "._-") == "" || strings.Trim(jobID, "._-") == "" {
		return video.Asset{}, fmt.Errorf("project_id and job_id metadata are required")
	}
	dir := filepath.Join(s.Root, projectID, episodeID, jobID)
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
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return video.Asset{}, err
	}
	if err = os.Rename(tmpName, a.URI); err != nil {
		return video.Asset{}, err
	}
	meta, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return video.Asset{}, err
	}
	if err = writeMetadataAtomic(a.URI+".json", meta); err != nil {
		return video.Asset{}, err
	}
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return a, nil
}

func writeMetadataAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".meta-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *Local) Read(a video.Asset) ([]byte, error) {
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Clean(a.URI))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("asset path escapes storage root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	rel, err = filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("asset symlink escapes storage root")
	}
	return os.ReadFile(resolvedPath)
}

var _ video.AssetSink = (*Local)(nil)
