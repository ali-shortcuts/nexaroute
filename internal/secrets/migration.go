package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// migrationJournalPath is a non-secret recovery marker used while converting a
// config to nxs2. It records hashes and the encrypted backup filename only.
const migrationJournalPathSuffix = ".nxs2-migration"

type migrationJournal struct {
	Version      int    `json:"version"`
	Backup       string `json:"backup"`
	SourceSHA256 string `json:"source_sha256"`
	TargetSHA256 string `json:"target_sha256"`
}

// MigrateToEnvelopeFile converts the config at path to nxs2 with an
// authenticated encrypted backup and crash-recoverable atomic replacement.
// legacyRaw must be the exact nxs1 master key when the config contains nxs1
// envelopes. It is never written to disk by this function.
func MigrateToEnvelopeFile(ctx context.Context, path string, legacyRaw []byte, env EnvelopeKeyProvider) (int, error) {
	if env == nil {
		return 0, errors.New("migration requires the Vault Transit envelope provider")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	journalPath := path + migrationJournalPathSuffix
	target, migrated, err := MigrateConfigBytes(ctx, raw, legacyRaw, env)
	if err != nil {
		return 0, err
	}
	if migrated == 0 {
		// Already migrated (or no secret fields). Verify an existing journal
		// describes this config before removing it.
		if j, ok, jerr := readMigrationJournal(journalPath); jerr != nil {
			return 0, jerr
		} else if ok {
			if j.TargetSHA256 != hashBytes(raw) {
				return 0, errors.New("migration journal target does not match the current config; refusing to recover automatically")
			}
			if err = os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
				return 0, errors.New("cannot remove completed migration journal")
			}
			syncDirectory(filepath.Dir(path))
		}
		return 0, nil
	}

	if j, ok, jerr := readMigrationJournal(journalPath); jerr != nil {
		return 0, jerr
	} else if ok {
		switch hashBytes(raw) {
		case j.SourceSHA256:
			// Interrupted before the config replacement. Continue below.
		case j.TargetSHA256:
			// Interrupted after the config replacement but before journal
			// removal. Verify the committed config and finish recovery.
			if _, _, verr := TransformConfigWith(Cipher{env: env}, raw, false); verr != nil {
				return 0, fmt.Errorf("migration journal matches a committed config that does not decrypt: %w", verr)
			}
			if rerr := os.Remove(journalPath); rerr != nil && !os.IsNotExist(rerr) {
				return 0, errors.New("cannot remove completed migration journal")
			}
			syncDirectory(filepath.Dir(path))
			return migrated, nil
		default:
			return 0, errors.New("migration journal does not match the current config; refusing to recover automatically")
		}
	}

	backupName := BackupName(path)
	backup, err := EncryptedBackupWith(Cipher{env: env}, raw)
	if err != nil {
		return 0, errors.New("cannot create encrypted migration backup")
	}
	if err = writeFileAtomic(backupName, backup); err != nil {
		return 0, errors.New("cannot persist encrypted migration backup")
	}
	journal := migrationJournal{
		Version:      1,
		Backup:       filepath.Base(backupName),
		SourceSHA256: hashBytes(raw),
		TargetSHA256: hashBytes(target),
	}
	jraw, err := json.Marshal(journal)
	if err != nil {
		return 0, errors.New("cannot serialize migration journal")
	}
	if err = writeFileAtomic(journalPath, append(jraw, '\n')); err != nil {
		return 0, errors.New("cannot persist migration journal")
	}
	if err = writeFileAtomic(path, append(target, '\n')); err != nil {
		return 0, errors.New("cannot atomically replace config during migration")
	}
	if err = os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		return 0, errors.New("config migrated but the recovery journal could not be removed")
	}
	syncDirectory(filepath.Dir(path))
	return migrated, nil
}

// RewrapConfigFile moves every wrapped data key to the latest Transit key
// version with an authenticated encrypted backup and atomic replacement.
func RewrapConfigFile(ctx context.Context, path string, env EnvelopeKeyProvider) (int, error) {
	if env == nil {
		return 0, errors.New("rewrap requires the Vault Transit envelope provider")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	out, n, err := RewrapConfigBytes(ctx, env, raw)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	backup, err := EncryptedBackupWith(Cipher{env: env}, raw)
	if err != nil {
		return 0, errors.New("cannot create encrypted rewrap backup")
	}
	if err = writeFileAtomic(BackupName(path), backup); err != nil {
		return 0, errors.New("cannot persist encrypted rewrap backup")
	}
	if err = writeFileAtomic(path, append(out, '\n')); err != nil {
		return 0, errors.New("cannot atomically replace config during rewrap")
	}
	syncDirectory(filepath.Dir(path))
	return n, nil
}

func readMigrationJournal(path string) (migrationJournal, bool, error) {
	var j migrationJournal
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return j, false, nil
		}
		return j, false, errors.New("cannot read migration journal")
	}
	if err = json.Unmarshal(raw, &j); err != nil || j.Version != 1 || j.SourceSHA256 == "" || j.TargetSHA256 == "" {
		return j, false, errors.New("invalid migration journal; preserve it and restore from backup")
	}
	return j, true, nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".nexaroute-secret-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	syncDirectory(dir)
	return nil
}
