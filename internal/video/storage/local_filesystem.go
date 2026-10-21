package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	for _, part := range []string{projectID, episodeID, jobID} {
		if part == "." || part == ".." {
			return video.Asset{}, fmt.Errorf("asset metadata contains an unsafe path segment %q", part)
		}
	}
	dir, err := s.assetDirectory([]string{projectID, episodeID, jobID})
	if err != nil {
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

// assetDirectory creates and returns a directory below the storage root
// without following symlinks inside the root. Asset metadata comes from API
// requests, so a sanitized component such as ".." or a pre-existing symlink
// under the root must not let an otherwise valid-looking asset escape it.
func (s *Local) assetDirectory(parts []string) (string, error) {
	dir := s.Root
	for _, part := range parts {
		if part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0700); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return "", err
				}
				info, err = os.Lstat(dir)
				if err != nil {
					return "", err
				}
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return "", fmt.Errorf("asset directory %s is not a real directory", dir)
				}
			}
		case err != nil:
			return "", err
		case info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("asset directory %s escapes storage root through a symlink", dir)
		case !info.IsDir():
			return "", fmt.Errorf("asset directory %s is not a directory", dir)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(s.Root)
	if err != nil {
		return "", err
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("asset directory %s escapes storage root", dir)
	}
	return dir, nil
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

// Open returns a regular asset file after validating that its resolved path
// remains inside the configured storage root. The caller must close the file.
func (s *Local) Open(a video.Asset) (*os.File, error) {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return nil, fmt.Errorf("storage root is not configured")
	}
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
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("asset is not a regular file")
	}
	if s.MaxBytes > 0 && info.Size() > s.MaxBytes {
		return nil, fmt.Errorf("asset exceeds size limit")
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || (s.MaxBytes > 0 && openedInfo.Size() > s.MaxBytes) {
		_ = file.Close()
		return nil, fmt.Errorf("asset is not a permitted regular file")
	}
	return file, nil
}

func (s *Local) Read(a video.Asset) ([]byte, error) {
	file, err := s.Open(a)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

var _ video.AssetSink = (*Local)(nil)
