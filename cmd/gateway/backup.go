package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
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
	maxBackupEntryBytes = 1 << 31 // 2 GiB sanity cap per entry
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
func runBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config")
	out := fs.String("out", "", "output bundle path (required)")
	includeKey := fs.Bool("include-master-key", false, "include the master key file in the bundle (sensitive; default keeps it separate)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(stderr, "nexaroute backup: --out is required")
		return 2
	}
	if err := createBackup(*configPath, *out, *includeKey, stdout); err != nil {
		fmt.Fprintf(stderr, "nexaroute backup failed: %v\n", err)
		return 1
	}
	return 0
}

func createBackup(configPath, outPath string, includeKey bool, stdout io.Writer) error {
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.LoadBase(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	stage, err := os.MkdirTemp("", "nexaroute-backup-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	manifest := backupManifest{Format: backupFormat, CreatedAt: time.Now().UTC(), GatewayVersion: version}
	add := func(name string, data []byte) error {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, backupManifestFile{Name: name, SHA256: sha256Hex(data), Size: int64(len(data))})
		return nil
	}
	if err := add(backupEntryConfig, configBytes); err != nil {
		return err
	}

	// Security/audit store: consistent online snapshot when present.
	securityPath, err := resolveSecurityStorePath(cfg.Admin.SecurityStorePath, configPath)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(securityPath); statErr == nil {
		store, err := securitystore.OpenReadOnly(securityPath)
		if err != nil {
			return fmt.Errorf("open security store for snapshot: %w", err)
		}
		var buf bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err = store.BackupTo(ctx, &buf)
		cancel()
		_ = store.Close()
		if err != nil {
			return fmt.Errorf("snapshot security store: %w", err)
		}
		if err := add(backupEntrySecurity, buf.Bytes()); err != nil {
			return err
		}
	} else {
		manifest.Notes = "security store absent at backup time; "
	}

	// File-backend control-plane state when present.
	if cfg.ControlPlane.Enabled && (cfg.ControlPlane.Backend == "" || cfg.ControlPlane.Backend == "file") {
		cpPath := cfg.ControlPlane.StatePath
		if cpPath == "" {
			cpPath = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(cpPath) {
			cpPath = filepath.Join(filepath.Dir(configPath), cpPath)
		}
		if data, err := os.ReadFile(cpPath); err == nil {
			if err := add(backupEntryControl, data); err != nil {
				return err
			}
		}
	}

	// Master key: fingerprint always (when file-backed); bytes only on
	// explicit opt-in.
	keyPath := configPath + ".key"
	if keyBytes, err := os.ReadFile(keyPath); err == nil {
		manifest.MasterKeySHA256 = sha256Hex(keyBytes)
		if includeKey {
			if err := add(backupEntryKey, keyBytes); err != nil {
				return err
			}
			manifest.MasterKeyIncluded = true
		}
	} else {
		manifest.Notes += "no master key file (external key provider or plaintext config)"
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*bundle) == "" {
		fmt.Fprintln(stderr, "nexaroute restore: --bundle is required")
		return 2
	}
	if err := restoreBundle(*bundle, *configPath, *force, stdout); err != nil {
		fmt.Fprintf(stderr, "nexaroute restore failed: %v\n", err)
		return 1
	}
	return 0
}

func restoreBundle(bundlePath, configPath string, force bool, stdout io.Writer) error {
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

	targets := map[string]string{backupEntryConfig: configPath}
	if _, ok := payloads[backupEntrySecurity]; ok {
		securityPath, err := resolveSecurityStorePath(bundled.Admin.SecurityStorePath, configPath)
		if err != nil {
			return err
		}
		targets[backupEntrySecurity] = securityPath
	}
	if _, ok := payloads[backupEntryControl]; ok {
		cpPath := bundled.ControlPlane.StatePath
		if cpPath == "" {
			cpPath = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(cpPath) {
			cpPath = filepath.Join(filepath.Dir(configPath), cpPath)
		}
		targets[backupEntryControl] = cpPath
	}
	if _, ok := payloads[backupEntryKey]; ok {
		targets[backupEntryKey] = configPath + ".key"
	}

	// Guards: never clobber an existing, different file without --force.
	for name, target := range targets {
		existing, err := os.ReadFile(target)
		if err != nil {
			continue // absent is fine
		}
		if sha256Hex(existing) == sha256Hex(payloads[name]) {
			continue // identical: idempotent skip below
		}
		if !force {
			return fmt.Errorf("%s already exists and differs; re-run with --force to overwrite", target)
		}
		if name == backupEntryKey {
			fmt.Fprintf(stdout, "WARNING: overwriting the existing master key at %s; the previous key may be the only copy able to decrypt other backups\n", target)
		}
	}

	for name, target := range targets {
		if existing, err := os.ReadFile(target); err == nil && sha256Hex(existing) == sha256Hex(payloads[name]) {
			fmt.Fprintf(stdout, "unchanged %s\n", target)
			continue
		}
		if err := writeFileAtomic(target, payloads[name], 0o600); err != nil {
			return fmt.Errorf("install %s: %w", target, err)
		}
		fmt.Fprintf(stdout, "restored %s\n", target)
	}
	if manifest.MasterKeySHA256 != "" && !manifest.MasterKeyIncluded {
		if keyBytes, err := os.ReadFile(configPath + ".key"); err == nil && sha256Hex(keyBytes) != manifest.MasterKeySHA256 {
			fmt.Fprintln(stdout, "WARNING: the master key on this machine does not match the backup's fingerprint; encrypted values will not decrypt until the paired key is restored")
		}
	}
	fmt.Fprintln(stdout, "restore complete; verify with `nexaroute doctor`, `nexaroute audit verify` and `nexaroute secrets verify` before starting the gateway")
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
// through config.LoadBase. On loader failure it falls back to a plain
// JSON parse so a key-less restore can still install files, and reports
// why full validation was skipped.
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
	if cfg, err := config.LoadBase(stagedConfig); err == nil {
		return cfg, "", nil
	}
	var raw config.Config
	if err := json.Unmarshal(payloads[backupEntryConfig], &raw); err != nil {
		return config.Config{}, "", fmt.Errorf("bundled config does not parse: %w", err)
	}
	return raw, "bundled config could not be fully loaded for validation (its master key may be unavailable); installed after a structural parse only — run `nexaroute doctor` after restore", nil
}
