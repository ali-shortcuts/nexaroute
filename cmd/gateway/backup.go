package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Backup bundle format "nexaroute-backup/1": a tar archive whose first
// entry is manifest.json, followed by the payload entries it describes.
// Every payload is SHA-256 hashed in the manifest and verified before a
// restore writes anything.
const (
	backupFormat        = "nexaroute-backup/1"
	backupEntryManifest = "manifest.json"
	backupEntryConfig   = "config.json"
	backupEntrySecurity = "security.db"
	backupEntryControl  = "controlplane.json"
	backupEntryKey      = "master.key"
)

// Bundle payload bounds for parsing (restore) and snapshotting
// (backup). They are variables so tests can lower them; the defaults
// are sized for the largest legitimate payload — a full audit store
// snapshot — while keeping a hostile or corrupt bundle from forcing
// multi-gigabyte in-memory payloads: a single entry may not exceed
// 2 GiB and all entries together may not exceed 4 GiB. Entry sizes
// are enforced from the tar headers BEFORE the entry body is read.
var (
	maxBackupEntryBytes int64 = 1 << 31 // 2 GiB per entry
	maxBackupTotalBytes int64 = 4 << 30 // 4 GiB aggregate
)

type backupManifestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type backupManifest struct {
	Format            string               `json:"format"`
	CreatedAt         time.Time            `json:"created_at"`
	GatewayVersion    string               `json:"gateway_version"`
	Files             []backupManifestFile `json:"files"`
	MasterKeyIncluded bool                 `json:"master_key_included"`
	// MasterKeySHA256 fingerprints the on-disk key at backup time even
	// when the key itself is not included, so a restore can warn when
	// the target machine's key does not pair with the backup.
	MasterKeySHA256 string `json:"master_key_sha256,omitempty"`
	Notes           string `json:"notes,omitempty"`
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// runBackup implements `nexaroute backup`: bundle the config (exactly as
// stored), a consistent snapshot of the security/audit store and the
// file-backend control-plane state into one hash-verified tar archive,
// written atomically with mode 0600. The master key is excluded unless
// --include-master-key is given.
//
// Two snapshot modes exist and are never mixed silently. Without
// --live, the CLI opens the security store itself: that requires the
// gateway to be stopped for the file-backed store, because a running
// gateway holds the store's exclusive lock (a locked open fails
// closed with an actionable error, never with a raw copy). With
// --live, the CLI asks the RUNNING gateway's admin endpoint for a
// snapshot taken from its own open store handle, authenticated with
// the operator's admin credential (see liveBackupOptions). Live mode
// requires emergency admin access (an admin API key, or loopback
// keyless emergency access where the gateway allows it); a gateway
// whose admin is OIDC-only cannot serve a CLI live snapshot, and
// that gateway must be backed up stopped.
func runBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config")
	out := fs.String("out", "", "output bundle path (required)")
	includeKey := fs.Bool("include-master-key", false, "include the master key file in the bundle (sensitive; default keeps it separate)")
	allowBrokenChain := fs.Bool("allow-broken-audit-chain", false, "back up even when the audit chain fails verification (the failure and this override are recorded in the bundle manifest)")
	live := fs.Bool("live", false, "take the security snapshot from a running gateway via its admin endpoint (requires admin access); without --live the gateway must be stopped for a file-backed security store")
	adminURL := fs.String("admin-url", "", "with --live, the gateway admin base URL (default: derived from the config listen address)")
	adminKey := fs.String("admin-key", "", "with --live, the admin API key (default: the config admin.api_key; empty means loopback keyless emergency access where the gateway allows it)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(stderr, "nexaroute backup: --out is required")
		return 2
	}
	liveOpts := liveBackupOptions{enabled: *live, adminURL: *adminURL, adminKey: *adminKey}
	if err := createBackup(*configPath, *out, *includeKey, *allowBrokenChain, stdout, liveOpts); err != nil {
		fmt.Fprintf(stderr, "nexaroute backup failed: %v\n", err)
		return 1
	}
	return 0
}

// liveBackupOptions carries the --live mode inputs. The admin key is
// handled like any operator credential in this process: it is sent
// only as an x-admin-key header to the configured admin URL and is
// never logged or written into the bundle.
type liveBackupOptions struct {
	enabled  bool
	adminURL string
	adminKey string
}

// readFileBounded reads a backup source without ever allocating more
// than the per-entry bundle limit: the size is checked from Stat
// BEFORE the read, and re-checked after it (a file that grew between
// the two is rejected, never truncated).
func readFileBounded(path, what string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBackupEntryBytes {
		return nil, fmt.Errorf("%s %s is %d bytes, exceeding the %d-byte bundle entry limit", what, path, info.Size(), maxBackupEntryBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBackupEntryBytes {
		return nil, fmt.Errorf("%s %s grew past the %d-byte bundle entry limit while being read", what, path, maxBackupEntryBytes)
	}
	return data, nil
}

// boundedBuffer is an io.Writer that fails rather than buffering more
// than its limit — used for the security-store snapshot, whose size is
// known only as it is produced.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if int64(w.buf.Len())+int64(len(p)) > w.limit {
		return 0, fmt.Errorf("snapshot exceeds the %d-byte bundle entry limit", w.limit)
	}
	return w.buf.Write(p)
}

func createBackup(configPath, outPath string, includeKey bool, allowBrokenChain bool, stdout io.Writer, live liveBackupOptions) error {
	configBytes, err := readFileBounded(configPath, "config")
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.LoadBase(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if live.enabled {
		if live.adminURL, err = resolveLiveAdminURL(live.adminURL, cfg.Listen); err != nil {
			return err
		}
		if strings.TrimSpace(live.adminKey) == "" {
			live.adminKey = cfg.Admin.APIKey
		}
	}

	// Resolve every source path up front. The output must never alias
	// a source: writeBundle atomically REPLACES the output path, so an
	// output equal to (or hard-/sym-linked to) the config, the master
	// key, the security store or the control-plane state would destroy
	// the very data being backed up.
	securityPath, err := resolveSecurityStorePath(cfg.Admin.SecurityStorePath, configPath)
	if err != nil {
		return err
	}
	keyPath := configPath + ".key"
	cpPath := ""
	if cfg.ControlPlane.Enabled && (cfg.ControlPlane.Backend == "" || cfg.ControlPlane.Backend == "file") {
		cpPath = cfg.ControlPlane.StatePath
		if cpPath == "" {
			cpPath = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(cpPath) {
			cpPath = filepath.Join(filepath.Dir(configPath), cpPath)
		}
	}
	sources := []string{configPath, keyPath, securityPath}
	if cpPath != "" {
		sources = append(sources, cpPath)
	}
	if err := refuseOutputCollision(outPath, sources); err != nil {
		return err
	}

	stage, err := os.MkdirTemp("", "nexaroute-backup-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	manifest := backupManifest{Format: backupFormat, CreatedAt: time.Now().UTC(), GatewayVersion: version}
	// The same per-entry and aggregate limits restore enforces are
	// applied while staging, so a backup can never produce a bundle
	// its own restore would reject for size.
	var stagedTotal int64
	add := func(name string, data []byte) error {
		if int64(len(data)) > maxBackupEntryBytes {
			return fmt.Errorf("backup entry %s is %d bytes, exceeding the %d-byte bundle entry limit", name, len(data), maxBackupEntryBytes)
		}
		stagedTotal += int64(len(data))
		if stagedTotal > maxBackupTotalBytes {
			return fmt.Errorf("backup payload exceeds the aggregate bundle limit of %d bytes at entry %s", maxBackupTotalBytes, name)
		}
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, backupManifestFile{Name: name, SHA256: sha256Hex(data), Size: int64(len(data))})
		return nil
	}
	if err := add(backupEntryConfig, configBytes); err != nil {
		return err
	}

	// Security/audit store: consistent snapshot when present.
	// Absence (ErrNotExist) is a legitimate state and is recorded in
	// the manifest; ANY other stat error (permissions, a path component
	// that is not a directory, I/O) fails the backup — silently
	// omitting an unreadable audit store would produce a backup that
	// looks complete but is not.
	//
	// Live mode (--live) never opens the store file itself: the
	// snapshot comes from the running gateway's admin endpoint,
	// taken from its own open handle, and is chain-verified here
	// from the received bytes before bundling (the gateway verifies
	// the chain in-process before streaming, and this second check
	// catches transport corruption). A live fetch failure is final:
	// there is no fallback to a stopped open (which would fail on
	// the lock anyway) and never a raw file copy.
	if live.enabled {
		snapBytes, err := fetchLiveSecuritySnapshot(live.adminURL, live.adminKey)
		if err != nil {
			return err
		}
		probePath := filepath.Join(stage, "security-probe.db")
		if err := os.WriteFile(probePath, snapBytes, 0o600); err != nil {
			return fmt.Errorf("stage live security snapshot for verification: %w", err)
		}
		probe, err := securitystore.OpenReadOnly(probePath)
		if err != nil {
			return fmt.Errorf("open live security snapshot for verification: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		report, verr := probe.VerifyAuditChain(ctx)
		cancel()
		_ = probe.Close()
		if verr != nil {
			return fmt.Errorf("verify live security snapshot chain: %w", verr)
		}
		if !report.Verified {
			first := report.Problems[0]
			if !allowBrokenChain {
				return fmt.Errorf("live security snapshot audit chain verification failed (%s); refusing to back up a tampered audit store — re-run with --allow-broken-audit-chain to override (note: the gateway itself refuses live snapshots of a broken chain, so this override only applies when the gateway streams one)", first)
			}
			fmt.Fprintf(stdout, "WARNING: live security snapshot audit chain FAILED verification (%s); backing up anyway under --allow-broken-audit-chain — this override is recorded in the bundle manifest\n", first)
			manifest.Notes += fmt.Sprintf("live security snapshot audit chain FAILED verification at backup time (%s); backed up under --allow-broken-audit-chain override; ", first)
		}
		manifest.Notes += "security snapshot taken live from the running gateway via its admin endpoint; "
		if err := add(backupEntrySecurity, snapBytes); err != nil {
			return err
		}
	} else if _, statErr := os.Stat(securityPath); statErr == nil {
		store, err := securitystore.OpenReadOnly(securityPath)
		if err != nil {
			if securitystore.IsLockedError(err) {
				return fmt.Errorf("open security store for snapshot: %w; the file-backed security store is locked by another process — usually a running gateway holding the writer lock. Stop the gateway cleanly and re-run backup, or use --live for an in-process snapshot from the running gateway (requires admin access)", err)
			}
			return fmt.Errorf("open security store for snapshot: %w", err)
		}
		snap := &boundedBuffer{limit: maxBackupEntryBytes}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		// The audit chain is verified before it is snapshotted (#428,
		// audit F4): a backup of a tampered store would otherwise look
		// complete while carrying a broken trail. Fail closed; the
		// explicit override is recorded in the manifest notes, which
		// are part of the bundle.
		report, verr := store.VerifyAuditChain(ctx)
		if verr != nil {
			cancel()
			_ = store.Close()
			return fmt.Errorf("verify audit chain for backup: %w", verr)
		}
		if !report.Verified {
			first := report.Problems[0]
			if !allowBrokenChain {
				cancel()
				_ = store.Close()
				return fmt.Errorf("audit chain verification failed (%s); refusing to back up a tampered audit store — re-run with --allow-broken-audit-chain to override", first)
			}
			fmt.Fprintf(stdout, "WARNING: audit chain FAILED verification (%s); backing up anyway under --allow-broken-audit-chain — this override is recorded in the bundle manifest\n", first)
			manifest.Notes += fmt.Sprintf("audit chain FAILED verification at backup time (%s); backed up under --allow-broken-audit-chain override; ", first)
		}
		err = store.BackupTo(ctx, snap)
		cancel()
		_ = store.Close()
		if err != nil {
			return fmt.Errorf("snapshot security store: %w", err)
		}
		if err := add(backupEntrySecurity, snap.buf.Bytes()); err != nil {
			return err
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		manifest.Notes = "security store absent at backup time; "
	} else {
		return fmt.Errorf("inspect security store %s: %w", securityPath, statErr)
	}

	// File-backend control-plane state when present. Same rule: a
	// missing state file is fine (nothing was ever persisted), an
	// unreadable one fails the backup instead of being dropped.
	if cpPath != "" {
		data, err := readFileBounded(cpPath, "control-plane state")
		if err == nil {
			if err := add(backupEntryControl, data); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read control-plane state %s: %w", cpPath, err)
		}
	}

	// Master key: fingerprint always (when file-backed); bytes only on
	// explicit opt-in. An unreadable key file (permissions, a directory
	// in its place) fails the backup rather than being misreported as
	// "no master key file".
	if keyBytes, err := readFileBounded(keyPath, "master key file"); err == nil {
		manifest.MasterKeySHA256 = sha256Hex(keyBytes)
		if includeKey {
			if err := add(backupEntryKey, keyBytes); err != nil {
				return err
			}
			manifest.MasterKeyIncluded = true
		}
	} else if errors.Is(err, os.ErrNotExist) {
		manifest.Notes += "no master key file (external key provider or plaintext config)"
	} else {
		return fmt.Errorf("read master key file %s: %w", keyPath, err)
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeBundle(outPath, manifestBytes, manifest.Files, stage); err != nil {
		return err
	}
	for _, f := range manifest.Files {
		fmt.Fprintf(stdout, "backed up %s (%d bytes)\n", f.Name, f.Size)
	}
	if manifest.MasterKeyIncluded {
		fmt.Fprintln(stdout, "WARNING: the bundle contains the master key; store it like a secret")
	} else if manifest.MasterKeySHA256 != "" {
		fmt.Fprintln(stdout, "master key not included; keep the key file paired with this backup (fingerprint recorded in the manifest)")
	}
	fmt.Fprintf(stdout, "backup written to %s\n", outPath)
	return nil
}

// resolveLiveAdminURL derives the gateway admin base URL for --live
// backups: an explicit --admin-url wins, otherwise the config listen
// address (the admin API shares the gateway listener). A bare ":port"
// listen binds all interfaces, so loopback is used for the client.
// Anything that is not a host:port (a socket path, a URL with a path)
// is refused rather than guessed at.
func resolveLiveAdminURL(flagURL, listen string) (string, error) {
	if strings.TrimSpace(flagURL) != "" {
		return strings.TrimRight(strings.TrimSpace(flagURL), "/"), nil
	}
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "", fmt.Errorf("live backup requires --admin-url or a listen address in the gateway config")
	}
	if strings.Contains(listen, "://") {
		return strings.TrimRight(listen, "/"), nil
	}
	if strings.Contains(listen, "/") {
		return "", fmt.Errorf("live backup cannot derive an admin URL from listen address %q; pass --admin-url", listen)
	}
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return "http://" + host, nil
}

// fetchLiveSecuritySnapshot fetches one security-store snapshot from
// the running gateway's admin endpoint. The admin key, when present,
// is sent only as an x-admin-key header; an empty key relies on the
// gateway's loopback keyless emergency rule, which the SERVER
// enforces (this client never decides that itself). Every failure is
// final and explicit: there is no fallback to opening the store file
// (which would fail on the lock) and no raw copy. Error bodies are
// truncated and never contain the key (the server's error envelope
// carries no secrets).
func fetchLiveSecuritySnapshot(adminURL, adminKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/admin/api/security-snapshot", nil)
	if err != nil {
		return nil, fmt.Errorf("build live snapshot request: %w", err)
	}
	if strings.TrimSpace(adminKey) != "" {
		req.Header.Set("x-admin-key", strings.TrimSpace(adminKey))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the running gateway admin API at %s for a live snapshot (is the gateway running? no fallback to a stopped snapshot was attempted): %w", adminURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("live snapshot rejected by gateway admin authentication (HTTP %d): check --admin-key; if the gateway's admin is OIDC-only, or the gateway is too old to offer live snapshots (an old gateway denies the unmapped admin path), stop the gateway and run backup without --live: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
		case http.StatusNotFound:
			return nil, fmt.Errorf("live snapshot endpoint not found on the gateway (HTTP 404): the running gateway is too old to offer live snapshots — stop the gateway and run backup without --live")
		case http.StatusNotImplemented:
			return nil, fmt.Errorf("live snapshot not supported by the gateway's security store backend (HTTP 501): use backend-native backup tooling, or stop the gateway and run backup without --live if the backend is file-backed")
		case http.StatusServiceUnavailable:
			return nil, fmt.Errorf("gateway refused the live snapshot (HTTP 503): %s", strings.TrimSpace(string(detail)))
		default:
			return nil, fmt.Errorf("live snapshot request failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(detail)))
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBackupEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read live security snapshot: %w", err)
	}
	if int64(len(data)) > maxBackupEntryBytes {
		return nil, fmt.Errorf("live security snapshot exceeds the %d-byte bundle entry limit; refusing to bundle a snapshot this backup could not restore", maxBackupEntryBytes)
	}
	return data, nil
}

// refuseOutputCollision rejects a backup output path that aliases any
// source file. Aliasing is detected three ways, because each covers a
// case the others miss: canonical-path equality (relative spellings,
// "..", symlinked parents, a symlinked output itself), os.SameFile on
// existing files (hard links and same-path), and resolution of a
// symlinked output's target. The check runs before anything is read
// for the bundle body or written anywhere near the sources.
func refuseOutputCollision(outPath string, sources []string) error {
	outCanonical, err := canonicalPath(outPath)
	if err != nil {
		return fmt.Errorf("resolve backup output path: %w", err)
	}
	outInfo, outStatErr := os.Stat(outPath)
	for _, src := range sources {
		srcCanonical, err := canonicalPath(src)
		if err != nil {
			return fmt.Errorf("resolve backup source path %s: %w", src, err)
		}
		if outCanonical == srcCanonical {
			return fmt.Errorf("backup output %s would overwrite its source %s; choose a different --out path", outPath, src)
		}
		if outStatErr == nil {
			if srcInfo, err := os.Stat(src); err == nil && os.SameFile(outInfo, srcInfo) {
				return fmt.Errorf("backup output %s is the same file as its source %s; choose a different --out path", outPath, src)
			}
		}
	}
	return nil
}

// canonicalPath returns an absolute, symlink-resolved form of p. When
// p itself does not exist, its parent directory is resolved instead so
// that a not-yet-created output inside a symlinked directory still
// compares correctly against existing sources. A symlinked final
// element is resolved through its target even when the target is
// missing (EvalSymlinks fails on a dangling link).
// canonicalPath resolves p to a canonical absolute path with a
// component-wise walk (realpath semantics):
//
//   - "." and ".." are applied PHYSICALLY, against the resolved
//     prefix — never lexically pre-collapsed across a symlink the way
//     filepath.Abs/Clean would, because a symlinked component changes
//     what ".." means.
//   - Every existing component is Lstat'd; symlinks are expanded in
//     place (relative targets resolve against the link's directory),
//     with a resolution cap so symlink loops error out.
//   - The walk stops at the first component that does not exist:
//     nothing below it can exist either, so the remaining tail is
//     appended lexically. This is the case the previous implementation
//     got wrong — it resolved only the immediate parent, so
//     link/new/file and real/new/file (link → real, new/ missing)
//     canonicalized to DIFFERENT strings and alias detection missed
//     the collision entirely.
//   - Errors other than genuine non-existence (permission, a
//     non-directory component, I/O) are PROPAGATED. They are never
//     mistaken for a missing path and never papered over with an
//     unresolved fallback: a destination that cannot be canonicalized
//     cannot be proven distinct, so callers must refuse it.
func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(p) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = cwd + string(os.PathSeparator) + p
	}
	const maxSymlinkResolutions = 40
	resolved := string(os.PathSeparator)
	rest := splitPathComponents(p)
	resolutions := 0
	for len(rest) > 0 {
		comp := rest[0]
		rest = rest[1:]
		switch comp {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, comp)
		info, err := os.Lstat(candidate)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink == 0 {
				resolved = candidate
				continue
			}
			resolutions++
			if resolutions > maxSymlinkResolutions {
				return "", fmt.Errorf("canonicalize %s: too many symlink resolutions", p)
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				resolved = string(os.PathSeparator)
			}
			// A relative target resolves against the link's own
			// directory, which is exactly the current resolved
			// prefix — so in both cases the target's components are
			// simply queued next.
			rest = append(splitPathComponents(target), rest...)
		case errors.Is(err, os.ErrNotExist):
			tail := append([]string{comp}, rest...)
			return filepath.Join(append([]string{resolved}, tail...)...), nil
		default:
			return "", err
		}
	}
	return resolved, nil
}

func splitPathComponents(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == os.PathSeparator })
}

// writeBundle writes manifest + staged payloads into a tar at outPath,
// atomically (temp file in the same directory, fsync, rename, 0600).
func writeBundle(outPath string, manifestBytes []byte, files []backupManifestFile, stage string) error {
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".nexaroute-backup-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	tw := tar.NewWriter(tmp)
	writeEntry := func(name string, r io.Reader, size int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	if err := writeEntry(backupEntryManifest, bytes.NewReader(manifestBytes), int64(len(manifestBytes))); err != nil {
		_ = tmp.Close()
		return err
	}
	for _, f := range files {
		fh, err := os.Open(filepath.Join(stage, f.Name))
		if err != nil {
			_ = tmp.Close()
			return err
		}
		err = writeEntry(f.Name, fh, f.Size)
		_ = fh.Close()
		if err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, outPath)
}

// runRestore implements `nexaroute restore`: verify the whole bundle
// (format, hashes, bundled config parses) BEFORE writing anything, then
// install atomically. Existing targets require --force.
func runRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bundle := fs.String("bundle", "", "backup bundle path (required)")
	configPath := fs.String("config", defaultConfigPath(), "target config path")
	force := fs.Bool("force", false, "overwrite existing files at the target")
	allowBrokenChain := fs.Bool("allow-broken-audit-chain", false, "restore even when the bundled audit chain fails verification (a durable override record is written next to the restored store)")
	recoverDir := fs.String("recover", "", "recover an interrupted restore from its journal directory, then exit")
	verify := fs.Bool("verify", false, "isolated verification: verify the bundle without writing to any restore target")
	into := fs.String("into", "", "with --verify, also materialize the bundle into this empty disposable directory")
	report := fs.String("report", "", "with --verify, write the machine-readable JSON report to this path")
	maxAge := fs.Duration("max-age", 0, "with --verify, fail a bundle older than this duration (e.g. 24h); 0 disables the freshness check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*recoverDir) != "" {
		if err := recoverRestoreJournal(*recoverDir, stdout); err != nil {
			fmt.Fprintf(stderr, "nexaroute restore --recover failed: %v\n", err)
			return 1
		}
		return 0
	}
	if *verify || strings.TrimSpace(*into) != "" {
		return runRestoreVerify(*bundle, *configPath, strings.TrimSpace(*into), *report, *maxAge, stdout, stderr)
	}
	if strings.TrimSpace(*report) != "" {
		fmt.Fprintln(stderr, "nexaroute restore: --report requires --verify")
		return 2
	}
	if strings.TrimSpace(*bundle) == "" {
		fmt.Fprintln(stderr, "nexaroute restore: --bundle is required")
		return 2
	}
	for _, dir := range findInterruptedRestoreJournals() {
		fmt.Fprintf(stderr, "WARNING: an interrupted restore journal exists at %s; its transaction did not complete — run `nexaroute restore --recover %s` to roll it back before relying on the installation\n", dir, dir)
	}
	if err := restoreBundle(*bundle, *configPath, *force, *allowBrokenChain, stdout); err != nil {
		fmt.Fprintf(stderr, "nexaroute restore failed: %v\n", err)
		return 1
	}
	return 0
}

func restoreBundle(bundlePath, configPath string, force bool, allowBrokenChain bool, stdout io.Writer) error {
	stage, manifest, payloads, err := readAndVerifyBundle(bundlePath)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	// The bundled config must load through the real loader (defaults +
	// validation) before it can drive target resolution. The staged copy
	// pairs it with the bundled key when present; when the loader cannot
	// run (e.g. an encrypted config restored before its key), fall back
	// to a structural parse with a loud warning — doctor verifies the
	// installation afterwards either way.
	bundled, loadNote, err := validateBundledConfig(stage, payloads)
	if err != nil {
		return err
	}
	if loadNote != "" {
		fmt.Fprintf(stdout, "WARNING: %s\n", loadNote)
	}

	// Install plan in a FIXED order (config, security store,
	// control-plane state, master key) so failure behavior is
	// deterministic and testable.
	var items []installItem
	var securityTarget string
	items = append(items, installItem{backupEntryConfig, configPath})
	if _, ok := payloads[backupEntrySecurity]; ok {
		securityPath, err := resolveSecurityStorePath(bundled.Admin.SecurityStorePath, configPath)
		if err != nil {
			return err
		}
		securityTarget = securityPath
		items = append(items, installItem{backupEntrySecurity, securityPath})
	}
	if _, ok := payloads[backupEntryControl]; ok {
		cpPath := bundled.ControlPlane.StatePath
		if cpPath == "" {
			cpPath = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(cpPath) {
			cpPath = filepath.Join(filepath.Dir(configPath), cpPath)
		}
		items = append(items, installItem{backupEntryControl, cpPath})
	}
	if _, ok := payloads[backupEntryKey]; ok {
		items = append(items, installItem{backupEntryKey, configPath + ".key"})
	}

	// Bundle/config consistency: a payload the bundled configuration
	// does not describe a supported backend for is an inconsistent
	// bundle (createBackup never produces one) — refuse it rather
	// than installing state the gateway would fail closed on.
	if _, ok := payloads[backupEntrySecurity]; ok {
		switch bundled.Admin.SecurityStoreBackend {
		case "", "bbolt":
		default:
			return fmt.Errorf("bundle contains a security store but the bundled config selects security store backend %q, which this build cannot restore (its adapters are M4-2 scope); refusing to restore", bundled.Admin.SecurityStoreBackend)
		}
	}
	if _, ok := payloads[backupEntryControl]; ok {
		if !bundled.ControlPlane.Enabled || (bundled.ControlPlane.Backend != "" && bundled.ControlPlane.Backend != "file") {
			return fmt.Errorf("bundle contains control-plane state but the bundled config does not enable the file control-plane backend; refusing to restore an inconsistent bundle")
		}
	}

	// Target collision prevention, BEFORE any filesystem write: two
	// payloads installing to the same destination would silently
	// overwrite each other in install order (e.g. a security store
	// path that resolves onto the config file). Canonical-path
	// equality catches relative spellings, "..", and symlinked paths;
	// os.SameFile catches hard-linked targets whose canonical paths
	// still differ.
	seen := map[string]string{}
	for _, it := range items {
		canon, err := canonicalPath(it.target)
		if err != nil {
			return fmt.Errorf("resolve restore target %s: %w", it.target, err)
		}
		if prev, dup := seen[canon]; dup {
			return fmt.Errorf("restore targets collide: bundle entries %q and %q both resolve to %s; refusing to restore", prev, it.name, canon)
		}
		seen[canon] = it.name
	}
	for i := range items {
		infoI, errI := os.Stat(items[i].target)
		if errI != nil {
			continue
		}
		for j := i + 1; j < len(items); j++ {
			if infoJ, errJ := os.Stat(items[j].target); errJ == nil && os.SameFile(infoI, infoJ) {
				return fmt.Errorf("restore targets collide: bundle entries %q and %q are the same file (%s / %s); refusing to restore", items[i].name, items[j].name, items[i].target, items[j].target)
			}
		}
	}

	// Guards: never clobber an existing, different file without --force.
	var installs []installItem
	for _, it := range items {
		existing, err := os.ReadFile(it.target)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect restore target %s: %w", it.target, err)
			}
			installs = append(installs, it) // absent: will be created
			continue
		}
		if sha256Hex(existing) == sha256Hex(payloads[it.name]) {
			fmt.Fprintf(stdout, "unchanged %s\n", it.target)
			continue // identical: idempotent skip
		}
		if !force {
			return fmt.Errorf("%s already exists and differs; re-run with --force to overwrite", it.target)
		}
		if it.name == backupEntryKey {
			fmt.Fprintf(stdout, "WARNING: overwriting the existing master key at %s; the previous key may be the only copy able to decrypt other backups\n", it.target)
		}
		installs = append(installs, it)
	}

	// The bundled audit chain is verified BEFORE anything is installed
	// (#428, audit F4): restoring a tampered store would otherwise
	// silently re-entomb a broken trail. Fail closed; the explicit
	// override proceeds loudly and leaves a durable record next to the
	// restored store (an audit event cannot be appended — the store
	// refuses to extend a broken chain by design).
	var brokenChainProblems []string
	if secPayload, ok := payloads[backupEntrySecurity]; ok {
		probeDir := filepath.Join(stage, "audit-verify")
		if err := os.MkdirAll(probeDir, 0o700); err != nil {
			return fmt.Errorf("stage bundled security store for audit verification: %w", err)
		}
		probePath := filepath.Join(probeDir, "security.db")
		if err := os.WriteFile(probePath, secPayload, 0o600); err != nil {
			return fmt.Errorf("stage bundled security store for audit verification: %w", err)
		}
		probe, err := securitystore.OpenReadOnly(probePath)
		if err != nil {
			return fmt.Errorf("open bundled security store for audit verification: %w", err)
		}
		report, verr := probe.VerifyAuditChain(context.Background())
		_ = probe.Close()
		if verr != nil {
			return fmt.Errorf("verify bundled audit chain: %w", verr)
		}
		if !report.Verified {
			if !allowBrokenChain {
				return fmt.Errorf("bundled audit chain verification failed (%s); refusing to restore a tampered audit store — re-run with --allow-broken-audit-chain to override", report.Problems[0])
			}
			brokenChainProblems = report.Problems
			fmt.Fprintf(stdout, "WARNING: bundled audit chain FAILED verification (%s); restoring anyway under --allow-broken-audit-chain — a durable override record will be written next to the restored store\n", report.Problems[0])
		}
	}

	if err := installWithRollback(installs, payloads, stdout); err != nil {
		return err
	}
	if len(brokenChainProblems) > 0 && securityTarget != "" {
		record, merr := json.MarshalIndent(map[string]any{
			"restored_at": time.Now().UTC(),
			"bundle":      filepath.Base(bundlePath),
			"override":    "--allow-broken-audit-chain",
			"problems":    brokenChainProblems,
			"note":        "the bundled audit chain failed verification and was restored under an explicit operator override; the store refuses to append to a broken chain, so this file is the audit record of the override",
		}, "", "  ")
		if merr == nil {
			sidecar := securityTarget + ".restore-override.json"
			if werr := os.WriteFile(sidecar, append(record, '\n'), 0o600); werr == nil {
				fmt.Fprintf(stdout, "audit-chain override record written to %s\n", sidecar)
			} else {
				fmt.Fprintf(stdout, "WARNING: could not write the audit-chain override record next to %s: %v\n", securityTarget, werr)
			}
		}
	}
	if manifest.MasterKeySHA256 != "" && !manifest.MasterKeyIncluded {
		if keyBytes, err := os.ReadFile(configPath + ".key"); err == nil && sha256Hex(keyBytes) != manifest.MasterKeySHA256 {
			fmt.Fprintln(stdout, "WARNING: the master key on this machine does not match the backup's fingerprint; encrypted values will not decrypt until the paired key is restored")
		}
	}
	fmt.Fprintln(stdout, "restore complete; verify with `nexaroute doctor`, `nexaroute audit verify` and `nexaroute secrets verify` before starting the gateway")
	return nil
}

// installItem is one planned restore write: a bundle entry name and
// the destination path it installs to.
type installItem struct{ name, target string }

// Durable restore journal ("nexaroute-restore-journal/1").
//
// The journal is the restore transaction's recovery record, designed
// to survive the process: a directory (mode 0700) holding one saved
// copy per replaced regular file (mode 0600) plus manifest.json,
// which maps every saved copy to its destination and records
// everything needed to reconstruct the pre-restore state without the
// original process: destination (absolute), whether it existed, its
// permission bits, its symlink target when it was a link, and
// per-entry install progress. The manifest is rewritten atomically
// (temp + rename + fsync of file and directory) BEFORE the first
// install, after every install, and when the transaction completes,
// so a crash at any point leaves a journal whose state is truthful:
// state is "prepared" (nothing knowingly installed), "installing"
// (some installs may have landed — including one whose manifest
// update never ran, which is why recovery restores EVERY entry's
// journaled state rather than trusting the installed flags), or
// "completed" (all installs landed; the journal is then removed, and
// a leftover completed journal is never rolled back).
//
// Recovery is explicit: `nexaroute restore --recover <journal-dir>`
// first STRICTLY validates the journal (validateJournal: format,
// provenance, structure, and a verified SHA-256 per saved copy —
// unknown or inconsistent journals are refused, never replayed),
// rolls every destination back to its journaled state, and removes
// the journal only when every destination was recovered; otherwise
// the journal stays and the error names the destinations that still
// need recovery. A normal restore (and doctor) reports interrupted
// journals it finds, with the recovery command.
const restoreJournalFormat = "nexaroute-restore-journal/1"

type restoreJournalEntry struct {
	Name        string      `json:"name"`                   // bundle entry name
	Target      string      `json:"target"`                 // absolute destination path
	Existed     bool        `json:"existed"`                // whether anything was there before
	Mode        os.FileMode `json:"mode,omitempty"`         // pre-restore permission bits (regular files)
	LinkTo      string      `json:"link_to,omitempty"`      // pre-restore symlink target
	Saved       string      `json:"saved,omitempty"`        // journal copy file name (regular files)
	SavedSHA256 string      `json:"saved_sha256,omitempty"` // SHA-256 of the journal copy; recovery verifies it before use
	Installed   bool        `json:"installed"`              // whether this install landed (progress marker)
}

type restoreJournalManifest struct {
	Format    string                `json:"format"`
	ID        string                `json:"id"`
	CreatedAt time.Time             `json:"created_at"`
	State     string                `json:"state"` // prepared | installing | completed
	Entries   []restoreJournalEntry `json:"entries"`
}

// persistJournalManifest is the seam through which installWithRollback
// records journal state; tests can fail a chosen transition (notably
// the final "completed" write). Production code uses
// writeJournalManifest.
var persistJournalManifest = writeJournalManifest

// writeJournalSavedCopy is the seam for writing one saved pre-restore
// copy; production uses writeSavedCopyDurable.
var writeJournalSavedCopy = writeSavedCopyDurable

// writeSavedCopyDurable writes a saved pre-restore copy through an
// open handle, checking every step and fsyncing the FILE ITSELF
// before returning: a recovery record whose bytes may still sit in a
// page cache when the first destination is installed is not durable,
// and a directory sync alone does not fix that.
func writeSavedCopyDurable(path string, data []byte) error {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		return err
	}
	if err := fh.Chmod(0o600); err != nil {
		_ = fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

// removeJournalDir deletes a journal and then syncs the PARENT
// directory (best effort) so the deletion itself is durable.
func removeJournalDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	_ = syncDir(filepath.Dir(dir))
	return nil
}

// writeJournalManifest atomically replaces the journal manifest and
// fsyncs both the file and its directory, so the on-disk state is as
// durable as the platform allows before the operation it describes.
func writeJournalManifest(journalDir string, m *restoreJournalManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(journalDir, ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(journalDir, "manifest.json")); err != nil {
		return err
	}
	return syncDir(journalDir)
}

func syncDir(dir string) error {
	fh, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	return fh.Sync()
}

func readJournalManifest(journalDir string) (*restoreJournalManifest, error) {
	data, err := os.ReadFile(filepath.Join(journalDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m restoreJournalManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("journal manifest does not parse: %w", err)
	}
	if m.Format != restoreJournalFormat {
		return nil, fmt.Errorf("unknown restore journal format %q (this build reads %q); refusing to replay it", m.Format, restoreJournalFormat)
	}
	return &m, nil
}

// findInterruptedRestoreJournals scans the journal base directory for
// leftover journals whose transaction did not complete — the durable
// trace of a crashed or killed restore.
func findInterruptedRestoreJournals() []string {
	interrupted, _ := scanRestoreJournals()
	return interrupted
}

// findUninspectableRestoreJournals returns journal directories whose
// manifest cannot be read at all. The interrupted scan skips them
// silently, which let doctor report a clean PASS while a
// manifest-less journal — saved pre-restore copies written before
// the first manifest persist, then a kill — sat undiscovered and
// restore --recover failed on it opaquely.
func findUninspectableRestoreJournals() []string {
	_, uninspectable := scanRestoreJournals()
	return uninspectable
}

func scanRestoreJournals() (interrupted, uninspectable []string) {
	base := os.TempDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, nil
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "nexaroute-restore-journal-") {
			continue
		}
		dir := filepath.Join(base, e.Name())
		m, err := readJournalManifest(dir)
		if err != nil {
			uninspectable = append(uninspectable, dir)
			continue
		}
		if m.State == "completed" {
			continue
		}
		interrupted = append(interrupted, dir)
	}
	return interrupted, uninspectable
}

// recoverRestoreJournal rolls every destination in a journal back to
// its recorded pre-restore state (see the format comment above for why
// the per-entry installed flags are not trusted). The journal is
// removed only when every destination was recovered; otherwise it is
// preserved and the returned error names the destinations that still
// need recovery. A completed journal is cleaned up, never replayed.
// validateJournal strictly validates a journal BEFORE recovery
// mutates anything. A journal directory is untrusted input: a caller
// can point --recover at any directory, so a manifest that merely
// parses is never enough. The checks establish provenance and
// internal consistency:
//
//   - the journal directory is a real directory (not a symlink) with
//     no group/other access, owned by the current user where the
//     platform exposes ownership, and the manifest is a regular,
//     non-symlink file with no group/other access;
//   - the transaction ID is hex, the state is one of the known
//     states, and the entry list is non-empty, small, and uses only
//     known bundle entry names, without duplicates;
//   - every destination is an absolute path and no two destinations
//     canonicalize to the same file (duplicates and aliases refused);
//   - pre-state metadata is consistent: absent destinations carry no
//     saved state; symlink entries carry a link target and no saved
//     copy; regular-file entries carry a saved copy named exactly
//     entry-<n>, a permission-only mode, and a SHA-256 that VERIFIES
//     against the copy's content — and the copy itself is a regular,
//     non-symlink, private file within the size limit.
//
// What this cannot do: prove a journal was written by NexaRoute
// (there is no signature). A journal stays operator-trusted input —
// anyone who can fabricate a fully consistent journal in a private
// directory can direct recovery writes to its recorded destinations,
// so journal directories belong inside the same trust boundary as the
// config directory itself (documented in docs/OPERATIONS.md).
func validateJournal(journalDir string, m *restoreJournalManifest) error {
	dirInfo, err := os.Lstat(journalDir)
	if err != nil {
		return fmt.Errorf("stat journal directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return errors.New("journal path is not a real directory")
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("journal directory permissions %04o are broader than 0700", dirInfo.Mode().Perm())
	}
	if err := checkJournalOwnership(dirInfo); err != nil {
		return err
	}
	manInfo, err := os.Lstat(filepath.Join(journalDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("stat journal manifest: %w", err)
	}
	if manInfo.Mode()&os.ModeSymlink != 0 || !manInfo.Mode().IsRegular() {
		return errors.New("journal manifest is not a regular file")
	}
	if manInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("journal manifest permissions %04o grant group/other access", manInfo.Mode().Perm())
	}
	if err := checkJournalOwnership(manInfo); err != nil {
		return err
	}

	if m.Format != restoreJournalFormat {
		return fmt.Errorf("unsupported journal format %q", m.Format)
	}
	if len(m.ID) < 8 || len(m.ID) > 64 {
		return fmt.Errorf("journal transaction ID %q has an invalid length", m.ID)
	}
	for _, r := range m.ID {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("journal transaction ID %q is not hexadecimal", m.ID)
		}
	}
	switch m.State {
	case "prepared", "installing", "completed":
	default:
		return fmt.Errorf("journal has unknown transaction state %q", m.State)
	}
	if len(m.Entries) == 0 || len(m.Entries) > 4 {
		return fmt.Errorf("journal entry count %d is outside the valid range 1..4", len(m.Entries))
	}
	knownNames := map[string]bool{
		backupEntryConfig: true, backupEntrySecurity: true,
		backupEntryControl: true, backupEntryKey: true,
	}
	seenNames := map[string]bool{}
	seenTargets := map[string]string{}
	for i := range m.Entries {
		e := &m.Entries[i]
		if !knownNames[e.Name] {
			return fmt.Errorf("journal entry %d has unknown name %q", i, e.Name)
		}
		if seenNames[e.Name] {
			return fmt.Errorf("journal contains duplicate entries for %q", e.Name)
		}
		seenNames[e.Name] = true
		if !filepath.IsAbs(e.Target) || strings.ContainsRune(e.Target, 0) {
			return fmt.Errorf("journal entry %q target %q is not a valid absolute path", e.Name, e.Target)
		}
		canon, err := canonicalPath(e.Target)
		if err != nil {
			return fmt.Errorf("journal entry %q target cannot be canonicalized: %w", e.Name, err)
		}
		if prev, dup := seenTargets[canon]; dup {
			return fmt.Errorf("journal entries %q and %q target the same destination", prev, e.Name)
		}
		seenTargets[canon] = e.Name
		switch {
		case !e.Existed:
			if e.Saved != "" || e.SavedSHA256 != "" || e.LinkTo != "" || e.Mode != 0 {
				return fmt.Errorf("journal entry %q claims the destination was absent but carries saved-state metadata", e.Name)
			}
		case e.LinkTo != "":
			if e.Saved != "" || e.SavedSHA256 != "" {
				return fmt.Errorf("journal entry %q is a symlink entry but carries a saved copy", e.Name)
			}
			if strings.ContainsRune(e.LinkTo, 0) {
				return fmt.Errorf("journal entry %q has an invalid recorded link target", e.Name)
			}
		default: // pre-existing regular file
			if !restoreJournalSavedNameRe.MatchString(e.Saved) {
				return fmt.Errorf("journal entry %q has an invalid saved-copy name %q", e.Name, e.Saved)
			}
			if e.Mode&^os.FileMode(0o777) != 0 {
				return fmt.Errorf("journal entry %q mode %v is not a permission mask", e.Name, e.Mode)
			}
			if len(e.SavedSHA256) != 64 {
				return fmt.Errorf("journal entry %q is missing its saved-copy checksum", e.Name)
			}
			if _, err := hex.DecodeString(e.SavedSHA256); err != nil {
				return fmt.Errorf("journal entry %q has a malformed saved-copy checksum", e.Name)
			}
			savedPath := filepath.Join(journalDir, e.Saved)
			savedInfo, err := os.Lstat(savedPath)
			if err != nil {
				return fmt.Errorf("journal entry %q saved copy: %w", e.Name, err)
			}
			if savedInfo.Mode()&os.ModeSymlink != 0 || !savedInfo.Mode().IsRegular() {
				return fmt.Errorf("journal entry %q saved copy is not a regular file", e.Name)
			}
			if savedInfo.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("journal entry %q saved copy permissions %04o grant group/other access", e.Name, savedInfo.Mode().Perm())
			}
			if savedInfo.Size() > maxBackupEntryBytes {
				return fmt.Errorf("journal entry %q saved copy exceeds the entry size limit", e.Name)
			}
			data, err := readFileBounded(savedPath, "journal saved copy")
			if err != nil {
				return fmt.Errorf("journal entry %q saved copy: %w", e.Name, err)
			}
			if sha256Hex(data) != e.SavedSHA256 {
				return fmt.Errorf("journal entry %q saved copy fails checksum verification (corrupt or tampered)", e.Name)
			}
		}
	}
	return nil
}

// restoreJournalSavedNameRe is the only shape a saved-copy name may
// take: exactly the names installWithRollback mints.
var restoreJournalSavedNameRe = regexp.MustCompile(`^entry-[0-9]+$`)

// checkJournalOwnership compares a journal file's owner with the
// current user where the platform exposes it (Unix stat); elsewhere
// it is a no-op — Go's portable FileInfo cannot express ownership.
func checkJournalOwnership(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("journal file %s is owned by uid %d, not the current user", info.Name(), st.Uid)
	}
	return nil
}

func recoverRestoreJournal(journalDir string, stdout io.Writer) error {
	m, err := readJournalManifest(journalDir)
	if err != nil {
		return fmt.Errorf("read restore journal %s: %w", journalDir, err)
	}
	// The journal is untrusted input: validate provenance and every
	// entry BEFORE any destination (or the journal itself) changes.
	if err := validateJournal(journalDir, m); err != nil {
		return fmt.Errorf("restore journal %s failed validation; nothing was changed and the journal is preserved: %w", journalDir, err)
	}
	if m.State == "completed" {
		if err := removeJournalDir(journalDir); err != nil {
			return fmt.Errorf("clean up completed restore journal %s: %w", journalDir, err)
		}
		fmt.Fprintf(stdout, "restore transaction %s completed before the interruption; journal cleaned up, nothing rolled back\n", m.ID)
		return nil
	}
	var failures []string
	for i := len(m.Entries) - 1; i >= 0; i-- {
		entry := m.Entries[i]
		var rerr error
		switch {
		case entry.LinkTo != "":
			if rerr = os.Remove(entry.Target); rerr == nil || errors.Is(rerr, os.ErrNotExist) {
				rerr = os.Symlink(entry.LinkTo, entry.Target)
			}
		case entry.Existed:
			var data []byte
			if data, rerr = os.ReadFile(filepath.Join(journalDir, entry.Saved)); rerr == nil {
				rerr = restoreRollbackWriteFile(entry.Target, data, entry.Mode)
			}
		default:
			if rerr = os.Remove(entry.Target); errors.Is(rerr, os.ErrNotExist) {
				rerr = nil
			}
		}
		if rerr == nil {
			// The mutation landed; confirm its durability before
			// this destination counts as recovered. A failed sync
			// does NOT undo the mutation — it makes the outcome
			// unconfirmed, so the entry is reported and the journal
			// preserved for an idempotent retry.
			rerr = syncDestDir(entry.Target)
		}
		if rerr != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", entry.Target, rerr))
		} else {
			fmt.Fprintf(stdout, "recovered %s\n", entry.Target)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("recovery INCOMPLETE for journal %s — could not restore: %s; the journal is preserved at %s with the pre-restore copies; fix those destinations from it (or from backup) before starting the gateway", m.ID, strings.Join(failures, ", "), journalDir)
	}
	if err := removeJournalDir(journalDir); err != nil {
		return fmt.Errorf("destinations recovered but journal %s could not be removed: %w", journalDir, err)
	}
	fmt.Fprintf(stdout, "restore transaction %s rolled back; journal removed\n", m.ID)
	return nil
}

// restoreWriteFile installs one payload file during a restore, and
// restoreRollbackWriteFile puts a journaled file back during
// rollback. They are separate variables so tests can inject
// deterministic failures into either phase independently; in
// production both are writeFileAtomic.
var restoreWriteFile = writeFileAtomic
var restoreRollbackWriteFile = writeFileAtomic

// installWithRollback installs the planned files in order, journaling
// the previous bytes of every target it will replace BEFORE the first
// write. If any install step fails, every file already installed is
// rolled back — journaled bytes are restored atomically, files the
// restore created are removed — and the returned error reports both
// the failure and any rollback step that itself failed.
//
// Guarantee limits (also documented in docs/OPERATIONS.md): rollback
// is best-effort, not a filesystem transaction — per-file atomic
// rename cannot be made atomic ACROSS files, and a rollback write can
// itself fail (disk full, permissions changed mid-restore), in which
// case the error names every file that could not be rolled back. What
// the durable journal (see restoreJournalManifest) adds on top: if the
// PROCESS dies mid-install, the journal survives with the complete
// pre-restore record, and `nexaroute restore --recover` rolls the
// transaction back afterwards. Power-loss durability still depends on
// the filesystem and hardware honoring fsync.
// syncDirFn syncs a DESTINATION directory after a mutation; it is a
// seam so tests can inject directory-sync failures. Journal-internal
// syncs call syncDir directly and are not affected by the seam.
var syncDirFn = syncDir

// syncDestDir makes a destination mutation durable to the extent the
// platform allows: it syncs the destination's parent directory (the
// rename/removal/link creation lives there) and its grandparent
// (which holds the parent's own entry when the install created the
// parent directory).
func syncDestDir(target string) error {
	parent := filepath.Dir(target)
	if err := syncDirFn(parent); err != nil {
		return err
	}
	if grand := filepath.Dir(parent); grand != parent {
		return syncDirFn(grand)
	}
	return nil
}

// rollbackInstalled restores the first count journaled destinations
// to their pre-restore state, newest first, syncing each destination
// directory after its mutation. It returns a description of every
// destination that could not be FULLY restored — "fully" includes
// the directory sync: a rollback mutation whose sync failed DID land,
// but its durability is unconfirmed, so the destination is reported
// and the caller preserves the journal for an idempotent retry.
func rollbackInstalled(journalDir string, manifest *restoreJournalManifest, installs []installItem, count int) []string {
	var failures []string
	for j := count - 1; j >= 0; j-- {
		entry := manifest.Entries[j]
		target := installs[j].target
		var rerr error
		switch {
		case entry.LinkTo != "":
			if rerr = os.Remove(target); rerr == nil || errors.Is(rerr, os.ErrNotExist) {
				rerr = os.Symlink(entry.LinkTo, target)
			}
		case entry.Existed:
			var data []byte
			if data, rerr = os.ReadFile(filepath.Join(journalDir, entry.Saved)); rerr == nil {
				rerr = restoreRollbackWriteFile(target, data, entry.Mode)
			}
		default:
			if rerr = os.Remove(target); errors.Is(rerr, os.ErrNotExist) {
				rerr = nil
			}
		}
		if rerr == nil {
			rerr = syncDestDir(target)
		}
		if rerr != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", entry.Target, rerr))
		}
	}
	return failures
}

func installWithRollback(installs []installItem, payloads map[string][]byte, stdout io.Writer) error {
	if len(installs) == 0 {
		return nil
	}
	journalDir, err := os.MkdirTemp("", "nexaroute-restore-journal-*")
	if err != nil {
		return fmt.Errorf("create restore journal: %w", err)
	}
	// The journal (mode 0700, entries 0600) is removed once the
	// install succeeds or rollback completes. On INCOMPLETE rollback
	// it is preserved and its path reported: it holds the only
	// pre-restore copies of the files that could not be put back.
	preserveJournal := false
	defer func() {
		if !preserveJournal {
			_ = removeJournalDir(journalDir)
		}
	}()
	// Make the journal directory entry itself durable before relying
	// on it; if even that fails, no destination has been touched yet.
	if err := syncDir(filepath.Dir(journalDir)); err != nil {
		return fmt.Errorf("create restore journal: %w", err)
	}
	// failPrep handles failures BEFORE the first install: no
	// destination has been modified, and an unmanifested journal
	// cannot be recovered automatically, so it is removed — and if
	// THAT fails the error says exactly what was left behind.
	failPrep := func(err error) error {
		if rmErr := os.RemoveAll(journalDir); rmErr != nil {
			preserveJournal = true
			return fmt.Errorf("%w; additionally the incomplete journal at %s could not be removed (%v) — it holds saved copies but no usable manifest; remove it manually", err, journalDir, rmErr)
		}
		return err
	}

	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return failPrep(fmt.Errorf("create restore journal: %w", err))
	}
	manifest := &restoreJournalManifest{
		Format:    restoreJournalFormat,
		ID:        hex.EncodeToString(idBytes[:]),
		CreatedAt: time.Now().UTC(),
		State:     "prepared",
	}
	for i, it := range installs {
		abs, err := filepath.Abs(it.target)
		if err != nil {
			return failPrep(fmt.Errorf("journal restore target %s: %w", it.target, err))
		}
		entry := restoreJournalEntry{Name: it.name, Target: abs}
		info, lerr := os.Lstat(it.target)
		switch {
		case errors.Is(lerr, os.ErrNotExist):
			// Created by this restore; rollback removes it.
		case lerr != nil:
			return failPrep(fmt.Errorf("journal restore target %s: %w", it.target, lerr))
		case info.Mode()&os.ModeSymlink != 0:
			// Installing replaces the link itself with a regular
			// file; journal where it pointed so rollback can
			// recreate the link exactly.
			target, rerr := os.Readlink(it.target)
			if rerr != nil {
				return failPrep(fmt.Errorf("journal restore target %s: %w", it.target, rerr))
			}
			entry.Existed = true
			entry.LinkTo = target
		case info.Mode().IsRegular():
			data, rerr := os.ReadFile(it.target)
			if rerr != nil {
				return failPrep(fmt.Errorf("journal restore target %s: %w", it.target, rerr))
			}
			entry.Existed = true
			entry.Mode = info.Mode().Perm()
			entry.Saved = fmt.Sprintf("entry-%d", i)
			entry.SavedSHA256 = sha256Hex(data)
			if werr := writeJournalSavedCopy(filepath.Join(journalDir, entry.Saved), data); werr != nil {
				return failPrep(fmt.Errorf("journal restore target %s: %w", it.target, werr))
			}
			if serr := syncDir(journalDir); serr != nil {
				return failPrep(fmt.Errorf("journal restore target %s: %w", it.target, serr))
			}
		default:
			// Directories, devices, sockets and other non-file
			// targets cannot be journaled as bytes; refuse before
			// writing anything instead of destroying them.
			return failPrep(fmt.Errorf("restore target %s has an unsupported type (%s); refusing to replace it", it.target, info.Mode().Type()))
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	// The complete pre-restore record must be durable BEFORE the
	// first destination is touched: prepared first, then installing.
	// A failure here aborts with every destination still untouched.
	if err := persistJournalManifest(journalDir, manifest); err != nil {
		return failPrep(fmt.Errorf("write restore journal: %w", err))
	}
	manifest.State = "installing"
	if err := persistJournalManifest(journalDir, manifest); err != nil {
		return failPrep(fmt.Errorf("write restore journal: %w", err))
	}

	installed := 0
	for _, it := range installs {
		if err := restoreWriteFile(it.target, payloads[it.name], 0o600); err != nil {
			// Roll back everything installed so far, newest first.
			if rbFailures := rollbackInstalled(journalDir, manifest, installs, installed); len(rbFailures) > 0 {
				preserveJournal = true
				return fmt.Errorf("install %s failed: %w; rollback INCOMPLETE — could not restore: %s; the installation is in a mixed state. The pre-restore copies are preserved in the restore journal at %s — recover those targets with `nexaroute restore --recover %s` (or from backup) before starting the gateway", it.target, err, strings.Join(rbFailures, ", "), journalDir, journalDir)
			}
			return fmt.Errorf("install %s failed: %w; all previously installed files were rolled back to their pre-restore state", it.target, err)
		}
		// The rename LANDED. From here this destination counts as
		// installed on every failure path — in particular, a failed
		// directory sync below does NOT mean the install did not
		// happen, and the destination joins the rollback scope.
		installed++
		manifest.Entries[installed-1].Installed = true
		if err := persistJournalManifest(journalDir, manifest); err != nil {
			// The install landed but its progress could not be
			// recorded durably — stop and keep the journal so the
			// transaction can be recovered explicitly.
			preserveJournal = true
			return fmt.Errorf("installed %s but could not update the restore journal: %w; the journal is preserved at %s — run `nexaroute restore --recover %s` to roll back to the pre-restore state", it.target, err, journalDir, journalDir)
		}
		if err := syncDestDir(it.target); err != nil {
			// The install is on disk but its durability is
			// unconfirmed; the transaction must not be marked
			// completed. Roll back INCLUDING this destination.
			if rbFailures := rollbackInstalled(journalDir, manifest, installs, installed); len(rbFailures) > 0 {
				preserveJournal = true
				return fmt.Errorf("installed %s but syncing its directory failed: %w; rollback INCOMPLETE — could not restore: %s; the installation is in a mixed state. The pre-restore copies are preserved in the restore journal at %s — recover those targets with `nexaroute restore --recover %s` (or from backup) before starting the gateway", it.target, err, strings.Join(rbFailures, ", "), journalDir, journalDir)
			}
			return fmt.Errorf("installed %s but syncing its directory failed: %w; all installed files were rolled back to their pre-restore state", it.target, err)
		}
		fmt.Fprintf(stdout, "restored %s\n", it.target)
	}
	manifest.State = "completed"
	if err := persistJournalManifest(journalDir, manifest); err != nil {
		// Every install landed, but the completion could not be
		// recorded. Never report success while silently deleting the
		// only recovery record from an ambiguous state: preserve the
		// journal (still "installing" on disk) and fail loudly with
		// the exact situation and the operator's options.
		preserveJournal = true
		return fmt.Errorf("all destinations were installed, but the restore journal could not be marked completed: %w; the journal is preserved at %s with its on-disk state still \"installing\". The files on disk DO reflect the restored bundle; the transaction is merely not recorded as finished. Verify the installation with `nexaroute doctor`, then either roll it back with `nexaroute restore --recover %s` or remove the journal manually", err, journalDir, journalDir)
	}
	return nil
}

// readAndVerifyBundle extracts the tar into a staging directory and
// verifies structure, format and every payload hash. It returns the
// manifest and payload bytes keyed by entry name.
func readAndVerifyBundle(bundlePath string) (string, backupManifest, map[string][]byte, error) {
	fh, err := os.Open(bundlePath)
	if err != nil {
		return "", backupManifest{}, nil, err
	}
	defer func() { _ = fh.Close() }()
	stage, err := os.MkdirTemp("", "nexaroute-restore-*")
	if err != nil {
		return "", backupManifest{}, nil, err
	}
	fail := func(err error) (string, backupManifest, map[string][]byte, error) {
		_ = os.RemoveAll(stage)
		return "", backupManifest{}, nil, err
	}
	allowed := map[string]bool{
		backupEntryManifest: true, backupEntryConfig: true, backupEntrySecurity: true,
		backupEntryControl: true, backupEntryKey: true,
	}
	payloads := map[string][]byte{}
	var declaredTotal int64
	tr := tar.NewReader(fh)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("read bundle: %w", err))
		}
		if !allowed[hdr.Name] || hdr.Typeflag != tar.TypeReg {
			return fail(fmt.Errorf("bundle contains an unexpected entry %q", hdr.Name))
		}
		if hdr.Size < 0 || hdr.Size > maxBackupEntryBytes {
			return fail(fmt.Errorf("bundle entry %q has an unreasonable size", hdr.Name))
		}
		// Aggregate bound, enforced from the declared header sizes
		// BEFORE the entry body is read, so a bundle cannot force the
		// restore to buffer more than maxBackupTotalBytes in total.
		declaredTotal += hdr.Size
		if declaredTotal > maxBackupTotalBytes {
			return fail(fmt.Errorf("bundle payload exceeds the aggregate limit of %d bytes", maxBackupTotalBytes))
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBackupEntryBytes))
		if err != nil {
			return fail(fmt.Errorf("read bundle entry %q: %w", hdr.Name, err))
		}
		if int64(len(data)) != hdr.Size {
			return fail(fmt.Errorf("bundle entry %q is truncated", hdr.Name))
		}
		payloads[hdr.Name] = data
	}
	manifestBytes, ok := payloads[backupEntryManifest]
	if !ok {
		return fail(fmt.Errorf("bundle has no manifest"))
	}
	var manifest backupManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fail(fmt.Errorf("bundle manifest does not parse: %w", err))
	}
	if manifest.Format != backupFormat {
		return fail(fmt.Errorf("unsupported backup format %q (this build reads %q)", manifest.Format, backupFormat))
	}
	listed := map[string]backupManifestFile{}
	for _, f := range manifest.Files {
		listed[f.Name] = f
		data, ok := payloads[f.Name]
		if !ok {
			return fail(fmt.Errorf("bundle is missing the listed file %q", f.Name))
		}
		if int64(len(data)) != f.Size || sha256Hex(data) != f.SHA256 {
			return fail(fmt.Errorf("bundle file %q fails its manifest hash/size check; refusing to restore", f.Name))
		}
	}
	for name := range payloads {
		if name != backupEntryManifest {
			if _, ok := listed[name]; !ok {
				return fail(fmt.Errorf("bundle contains an unlisted file %q; refusing to restore", name))
			}
		}
	}
	if _, ok := payloads[backupEntryConfig]; !ok {
		return fail(fmt.Errorf("bundle has no config payload"))
	}
	return stage, manifest, payloads, nil
}

// writeFileAtomic writes data to path via a temp file in the same
// directory + fsync + rename, with the given mode; parent directories
// are created private (0700) when missing.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".nexaroute-restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// validateBundledConfig loads the bundled config from a staged copy
// (with the bundled master key beside it when the bundle carries one)
// through config.LoadBase.
//
// A loader failure is NOT a free pass. The only tolerated failure is
// a positively identified missing key (see keylessRestoreJustified):
// an encrypted config whose key neither travelled with the bundle nor
// is available from the environment, where decryption — and therefore
// validation — is impossible in the staging area. In that case the
// config is installed after a structural JSON parse only, with a loud
// warning, and full validation is deferred to `nexaroute doctor` on
// the target (where the paired key lives). Every other failure —
// invalid JSON, semantically invalid configuration, a bundled key
// that does not decrypt — rejects the restore before anything is
// written.
func validateBundledConfig(stage string, payloads map[string][]byte) (config.Config, string, error) {
	dir := filepath.Join(stage, "validate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return config.Config{}, "", err
	}
	stagedConfig := filepath.Join(dir, "config.json")
	if err := os.WriteFile(stagedConfig, payloads[backupEntryConfig], 0o600); err != nil {
		return config.Config{}, "", err
	}
	if key, ok := payloads[backupEntryKey]; ok {
		if err := os.WriteFile(stagedConfig+".key", key, 0o600); err != nil {
			return config.Config{}, "", err
		}
	}
	cfg, err := config.LoadBase(stagedConfig)
	if err == nil {
		return cfg, "", nil
	}
	if !keylessRestoreJustified(payloads) {
		return config.Config{}, "", fmt.Errorf("bundled config failed validation and the failure is not an identified missing master key; refusing to restore: %w", err)
	}
	var raw config.Config
	if jerr := json.Unmarshal(payloads[backupEntryConfig], &raw); jerr != nil {
		return config.Config{}, "", fmt.Errorf("bundled config does not parse: %w", jerr)
	}
	return raw, "bundled config is encrypted and its master key was not available for validation (keyless restore); installed after a structural parse only — run `nexaroute doctor` after restore", nil
}

// keylessRestoreJustified positively identifies the one loader failure
// a restore may proceed past: the bundled config carries encrypted
// (nxs1:) values, and NO key material was available to the staged
// load — the bundle carries no master key and the environment provides
// none. The loader consults a key only when the config contains
// secret envelopes (or legacy secret values, which it can always
// migrate with an auto-generated staging key), so under these
// conditions the failure can only be the missing key, never a
// semantically invalid config hiding behind it: without a key,
// decryption fails before validation is ever reached; with a key
// available (bundled or environment), this function returns false and
// any failure is fatal.
func keylessRestoreJustified(payloads map[string][]byte) bool {
	if !bytes.Contains(payloads[backupEntryConfig], []byte(secrets.Prefix)) {
		return false // plaintext config: the loader failure is not a missing key
	}
	if _, ok := payloads[backupEntryKey]; ok {
		return false // a key travelled with the bundle and was staged
	}
	for _, env := range []string{"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE", "NEXAROUTE_KEY_PROVIDER"} {
		if os.Getenv(env) != "" {
			return false // environment key material may have been used
		}
	}
	return true
}
