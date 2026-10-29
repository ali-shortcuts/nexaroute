package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MigrationResult reports a completed migration. The backup is verbatim
// pre-migration bytes (ADR 0007 §7.2): it is decryptable only by the old
// KEK, so the old key must be archived, not deleted, until post-write
// verification passes and the next backup cycle succeeds.
type MigrationResult struct {
	Migrated     int
	BackupPath   string
	BackupSHA256 string
}

// RewrapResult reports a completed rewrap. AlreadyLatest means every
// field was already at the latest key version: nothing was written and
// no backup was created.
type RewrapResult struct {
	Rewrapped     int
	AlreadyLatest bool
	BackupPath    string
	BackupSHA256  string
}

// MigrateToEnvelopeFile converts the config at path to nxs2 following
// ADR 0007 §7.2: classify, decrypt old fully in memory, prove Transit
// connectivity with a canary, write a verbatim pre-migration backup,
// re-encrypt and verify in memory, atomically replace, then post-write
// verify. On post-write verification failure it rolls back by atomically
// renaming the backup over the config and verifies the rollback before
// reporting failure. An already-nxs2 config is verified only and never
// rewritten. The operation holds an exclusive file lock for its duration;
// a second writer fails with a lock message instead of merging.
// legacyRaw must be the exact nxs1 master key when the config contains
// nxs1 envelopes. It is never written to disk by this function.
func MigrateToEnvelopeFile(ctx context.Context, path string, legacyRaw []byte, env EnvelopeKeyProvider) (MigrationResult, error) {
	var result MigrationResult
	if env == nil {
		return result, errors.New("migration requires the Vault Transit envelope provider")
	}
	release, err := acquireSecretLock(path + ".lock")
	if err != nil {
		return result, err
	}
	defer release()
	raw, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	target, migrated, err := MigrateConfigBytes(ctx, raw, legacyRaw, env)
	if err != nil {
		return result, err
	}
	if migrated == 0 {
		return result, nil
	}
	backupName := path + "." + time.Now().UTC().Format("20060102T150405Z") + ".pre-transit.bak"
	if err := writeFileAtomic(backupName, raw); err != nil {
		return result, errors.New("cannot persist verbatim pre-migration backup")
	}
	result.BackupPath = backupName
	result.BackupSHA256 = hashBytes(raw)
	if err := writeFileAtomic(path, append(target, '\n')); err != nil {
		return result, errors.New("cannot atomically replace config during migration")
	}
	if err := verifyFileDecryptsWithTransit(ctx, path, env); err != nil {
		rollbackErr := rollbackFromBackup(path, backupName, func(b []byte) error {
			_, _, verr := TransformConfig(b, legacyRaw, false)
			return verr
		})
		if rollbackErr != nil {
			return result, fmt.Errorf("migration post-write verification failed (%v); rollback also failed (%v); restore %s manually", err, rollbackErr, backupName)
		}
		return result, fmt.Errorf("migration post-write verification failed (%v); rolled back to the pre-migration backup", err)
	}
	result.Migrated = migrated
	return result, nil
}

// MigrateDryRunInfo classifies and verifies a migration without writing
// anything and without re-encrypting any field: it decrypt-verifies the
// old config with the legacy key and proves Transit connectivity with a
// canary. It reports how many fields would migrate.
func MigrateDryRunInfo(ctx context.Context, raw []byte, legacyRaw []byte, env EnvelopeKeyProvider) (int, error) {
	if env == nil {
		return 0, errors.New("migration requires the Vault Transit envelope provider")
	}
	cls, err := ClassifyConfig(raw)
	if err != nil {
		return 0, err
	}
	if cls.Unknown > 0 {
		return 0, fmt.Errorf("config contains unknown envelope(s) in field(s) %s; refusing to migrate", joinFieldNames(cls.UnknownFields))
	}
	if cls.NXS1 > 0 && cls.NXS2 > 0 {
		return 0, errors.New("config contains mixed nxs1 and nxs2 envelopes; refusing to migrate")
	}
	if cls.Plaintext > 0 {
		return 0, fmt.Errorf("config contains plaintext secret field(s) %s; seal them under nxs1 first, then migrate", joinFieldNames(cls.PlaintextFields))
	}
	if cls.NXS2 > 0 {
		if _, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, false); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if cls.NXS1 == 0 {
		return 0, nil
	}
	if len(legacyRaw) != 32 {
		return 0, errors.New("migration requires the explicit 32-byte legacy master key for nxs1 envelopes")
	}
	if _, _, err := TransformConfig(raw, legacyRaw, false); err != nil {
		return 0, err
	}
	if err := ProveTransitConnectivity(ctx, env); err != nil {
		return 0, err
	}
	return cls.NXS1, nil
}

// RewrapConfigFile moves every wrapped data key to the latest Transit key
// version following ADR 0007 §7.3: decrypt-verify old, rewrap in memory
// via the rewrap endpoint only, decrypt-verify new, write a verbatim
// pre-rewrap backup, atomically replace, post-write verify, roll back on
// post-write failure. A config already at the latest version is left
// byte-identical: nothing is written and no backup is created. The
// operation holds an exclusive file lock for its duration.
func RewrapConfigFile(ctx context.Context, path string, env EnvelopeKeyProvider) (RewrapResult, error) {
	var result RewrapResult
	if env == nil {
		return result, errors.New("rewrap requires the Vault Transit envelope provider")
	}
	release, err := acquireSecretLock(path + ".lock")
	if err != nil {
		return result, err
	}
	defer release()
	raw, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	out, n, alreadyLatest, err := RewrapConfigBytes(ctx, env, raw)
	if err != nil {
		return result, err
	}
	if alreadyLatest {
		result.AlreadyLatest = true
		return result, nil
	}
	backupName := path + "." + time.Now().UTC().Format("20060102T150405Z") + ".pre-rewrap.bak"
	if err := writeFileAtomic(backupName, raw); err != nil {
		return result, errors.New("cannot persist verbatim pre-rewrap backup")
	}
	result.BackupPath = backupName
	result.BackupSHA256 = hashBytes(raw)
	if err := writeFileAtomic(path, append(out, '\n')); err != nil {
		return result, errors.New("cannot atomically replace config during rewrap")
	}
	if err := verifyFileDecryptsWithTransit(ctx, path, env); err != nil {
		rollbackErr := rollbackFromBackup(path, backupName, func(b []byte) error {
			_, _, verr := TransformConfigWith(ctx, Cipher{env: env}, b, false)
			return verr
		})
		if rollbackErr != nil {
			return result, fmt.Errorf("rewrap post-write verification failed (%v); rollback also failed (%v); restore %s manually", err, rollbackErr, backupName)
		}
		return result, fmt.Errorf("rewrap post-write verification failed (%v); rolled back to the pre-rewrap backup", err)
	}
	result.Rewrapped = n
	return result, nil
}

// RewrapDryRunInfo verifies the old config decrypts and reports the
// distinct Transit key versions present, without any rewrap call and
// without writing anything (ADR 0007 §7.3 dry-run).
func RewrapDryRunInfo(ctx context.Context, raw []byte, env EnvelopeKeyProvider) (int, []int, error) {
	if env == nil {
		return 0, nil, errors.New("rewrap requires the Vault Transit envelope provider")
	}
	cls, err := ClassifyConfig(raw)
	if err != nil {
		return 0, nil, err
	}
	if cls.Unknown > 0 {
		return 0, nil, fmt.Errorf("config contains unknown envelope(s) in field(s) %s; refusing to rewrap", joinFieldNames(cls.UnknownFields))
	}
	if cls.NXS1 > 0 || cls.Plaintext > 0 {
		return 0, nil, errors.New("rewrap requires a config containing only nxs2 envelopes; migrate nxs1/plaintext first")
	}
	if _, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, false); err != nil {
		return 0, nil, fmt.Errorf("rewrap dry-run pre-verification failed: %w", err)
	}
	versions, err := DistinctTransitKeyVersions(raw)
	if err != nil {
		return 0, nil, err
	}
	return cls.NXS2, versions, nil
}

// TransitEnvelopeInfo parses every nxs2 field of a config document and
// returns the common key-reference hash and the sorted distinct Transit
// key versions present (learned from ciphertext prefixes, never from a
// Vault read capability). Envelopes that disagree on the key reference
// are an error: no single Transit configuration can load such a config.
func TransitEnvelopeInfo(raw []byte) (string, []int, error) {
	versions, err := DistinctTransitKeyVersions(raw)
	if err != nil {
		return "", nil, err
	}
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return "", nil, err
	}
	keyRef := ""
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return v, nil
		}
		if !strings.HasPrefix(s, EnvelopePrefix) {
			return v, nil
		}
		meta, e := parseEnvelope(s)
		if e != nil {
			return nil, e
		}
		if keyRef == "" {
			keyRef = meta.KeyRefSHA256
		} else if keyRef != meta.KeyRefSHA256 {
			return nil, fmt.Errorf("nxs2 envelopes disagree on the Transit key reference in field %s:%s", owner, field)
		}
		return v, nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return "", nil, err
	}
	return keyRef, versions, nil
}

// DistinctTransitKeyVersions parses every nxs2 field and returns the
// sorted distinct Transit key versions present (learned from ciphertext
// prefixes, never from a Vault read capability).
func DistinctTransitKeyVersions(raw []byte) ([]int, error) {
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return v, nil
		}
		meta, e := parseEnvelope(s)
		if e != nil {
			return nil, e
		}
		seen[meta.TransitKeyVersion] = true
		return v, nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return nil, err
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out, nil
}

func verifyFileDecryptsWithTransit(ctx context.Context, path string, env EnvelopeKeyProvider) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, _, err = TransformConfigWith(ctx, Cipher{env: env}, raw, false)
	return err
}

// rollbackFromBackup atomically renames the backup over the config,
// syncs the directory, and verifies the rolled-back bytes with verify.
// The backup file is consumed by the rename; on success the operator
// still has the pre-operation state in the config itself.
func rollbackFromBackup(path, backupName string, verify func([]byte) error) error {
	raw, err := os.ReadFile(backupName)
	if err != nil {
		return err
	}
	if err := os.Rename(backupName, path); err != nil {
		return err
	}
	syncDirectory(filepath.Dir(path))
	if err := verify(raw); err != nil {
		return err
	}
	return nil
}

func joinFieldNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
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
