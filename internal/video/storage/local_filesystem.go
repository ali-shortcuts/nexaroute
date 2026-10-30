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

// StoredName returns the filename Local.Put will persist for a
// provider-supplied name: the sanitized basename, with empty/dot names
// mapped to asset.bin. Providers must use it when checking for
// collisions, because Put renames silently over an existing file with
// the same stored name — so two distinct supplied names can collide
// after sanitization (for example "a b.mp4" and "a_b.mp4").
func StoredName(name string) string {
	stored := safeName.ReplaceAllString(filepath.Base(name), "_")
	if stored == "" || stored == "." || stored == ".." {
		return "asset.bin"
	}
	return stored
}

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
	name := StoredName(a.Filename)
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
	defer func() { _ = os.Remove(tmpName) }()
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
	defer func() { _ = os.Remove(name) }()
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

// Delete removes an asset file previously persisted by Put, together
// with its sidecar metadata, after the same inside-root validation as
// Open. It removes the named path itself (never following a symlink),
// so a symlinked asset path can only delete the link, never a target
// outside the root. Callers use it to roll back a multi-asset download
// that failed part-way: the assets saved by that call are removed
// instead of being left as an orphaned partial set. It never deletes
// an asset it did not validate as inside the root.
func (s *Local) Delete(_ context.Context, a video.Asset) error {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return fmt.Errorf("storage root is not configured")
	}
	if strings.TrimSpace(a.URI) == "" {
		return fmt.Errorf("asset URI is required")
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(filepath.Clean(a.URI))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrAssetOutsideRoot
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return err
	}
	rel, err = filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ErrAssetSymlinkOutsideRoot
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("asset is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.Remove(path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrAssetOutsideRoot and ErrAssetSymlinkOutsideRoot report that an asset
// path (or its symlink-resolved form) escapes the configured storage root.
// Both are deterministic validation rejections, not transient storage
// outages; callers map them to 404, not to a retryable 503.
var (
	ErrAssetOutsideRoot        = errors.New("asset path escapes storage root")
	ErrAssetSymlinkOutsideRoot = errors.New("asset symlink escapes storage root")
)

// IsOutsideRoot reports whether err is an outside-root rejection.
func IsOutsideRoot(err error) bool {
	return errors.Is(err, ErrAssetOutsideRoot) || errors.Is(err, ErrAssetSymlinkOutsideRoot)
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
		return nil, ErrAssetOutsideRoot
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
		return nil, ErrAssetSymlinkOutsideRoot
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

// Read returns the asset's bytes after the same path, type and size
// validation as Open, plus integrity verification: when the asset
// record carries the SHA-256 that Put computed (or a size), the bytes
// on disk must match it. A mismatch means the stored object is corrupt
// or was replaced under the root, and the read fails instead of
// silently serving damaged media. Assets recorded without a checksum
// (written before integrity records existed) read as before.
func (s *Local) Read(a video.Asset) ([]byte, error) {
	file, err := s.Open(a)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	if a.SizeBytes > 0 && int64(len(data)) != a.SizeBytes {
		return nil, fmt.Errorf("asset size mismatch: record %d bytes, disk %d bytes", a.SizeBytes, len(data))
	}
	if a.SHA256 != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			return nil, fmt.Errorf("asset checksum mismatch: stored object is corrupt or was replaced")
		}
	}
	return data, nil
}

var _ video.AssetSink = (*Local)(nil)
