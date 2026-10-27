package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeEnvelope is a deterministic EnvelopeKeyProvider used to exercise the
// nxs2 format without a real Vault server. It records the AAD and key version
// so wrong-AAD, wrong-key and rewrap behavior can be asserted exactly.
type fakeEnvelope struct {
	mu      sync.Mutex
	id      string
	version int
	next    int
	keys    map[string]fakeWrapped
	fail    error
}

type fakeWrapped struct {
	dek []byte
	aad []byte
}

func newFakeEnvelope(id string) *fakeEnvelope {
	return &fakeEnvelope{id: id, version: 1, keys: map[string]fakeWrapped{}}
}

func (f *fakeEnvelope) KeyID() string { return f.id }

func (f *fakeEnvelope) WrapDEK(_ context.Context, dek, aad []byte) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	if len(dek) != 32 {
		return "", errors.New("fake: bad dek")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("vault:v%d:key-%d", f.version, f.next)
	f.keys[id] = fakeWrapped{dek: append([]byte(nil), dek...), aad: append([]byte(nil), aad...)}
	return id, nil
}

func (f *fakeEnvelope) UnwrapDEK(_ context.Context, wrapped string, aad []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[wrapped]
	if !ok {
		return nil, errors.New("fake: unknown wrapped key or revoked key version")
	}
	if string(k.aad) != string(aad) {
		return nil, errors.New("fake: associated data mismatch")
	}
	return append([]byte(nil), k.dek...), nil
}

func (f *fakeEnvelope) RewrapDEK(ctx context.Context, wrapped string, aad []byte) (string, error) {
	dek, err := f.UnwrapDEK(ctx, wrapped, aad)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.version++
	f.mu.Unlock()
	return f.WrapDEK(ctx, dek, aad)
}

func (f *fakeEnvelope) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = map[string]fakeWrapped{}
}

func TestEnvelopeRoundTripAndMetadata(t *testing.T) {
	env := newFakeEnvelope("key-a")
	aad := AAD("provider-a", "api_key")
	text, err := sealEnvelope(context.Background(), env, aad, []byte("roundtrip-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, EnvelopePrefix) || strings.Contains(text, "roundtrip-secret") {
		t.Fatalf("unexpected envelope: %q", text)
	}
	meta, err := parseEnvelope(text)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != 2 || meta.KeyID != env.KeyID() || meta.Wrapped == "" {
		t.Fatalf("bad metadata: %+v", meta)
	}
	plain, err := openEnvelope(context.Background(), env, aad, text)
	if err != nil || string(plain) != "roundtrip-secret" {
		t.Fatalf("round trip: %q %v", plain, err)
	}
}

func TestEnvelopeAADBindingAndTamper(t *testing.T) {
	env := newFakeEnvelope("key-a")
	aad := AAD("provider-a", "api_key")
	text, err := sealEnvelope(context.Background(), env, aad, []byte("canary"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openEnvelope(context.Background(), env, AAD("provider-b", "api_key"), text); err == nil {
		t.Fatal("AAD provider move authenticated")
	}
	if _, err = openEnvelope(context.Background(), env, AAD("provider-a", "other"), text); err == nil {
		t.Fatal("AAD field move authenticated")
	}
	meta, _ := parseEnvelope(text)
	for _, field := range []string{"nonce", "data"} {
		tampered := meta
		if field == "nonce" {
			tampered.Nonce = base64.RawURLEncoding.EncodeToString([]byte("0123456789ab"))
		} else {
			tampered.Data = base64.RawURLEncoding.EncodeToString([]byte("tampered"))
		}
		encoded, _ := encodeEnvelope(tampered)
		if _, err = openEnvelope(context.Background(), env, aad, encoded); err == nil {
			t.Fatalf("tampered %s authenticated", field)
		}
	}
	tampered := meta
	tampered.Wrapped = "vault:v1:unknown"
	encoded, _ := encodeEnvelope(tampered)
	if _, err = openEnvelope(context.Background(), env, aad, encoded); err == nil {
		t.Fatal("unknown wrapped key authenticated")
	}
	if _, err = parseEnvelope("nxs2:not-base64!"); err == nil {
		t.Fatal("malformed envelope accepted")
	}
}

func TestEnvelopeWrongKeyReference(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text, err := sealEnvelope(context.Background(), env, AAD("p", "api_key"), []byte("canary"))
	if err != nil {
		t.Fatal(err)
	}
	other := newFakeEnvelope("key-b")
	if _, err = openEnvelope(context.Background(), other, AAD("p", "api_key"), text); err == nil || !strings.Contains(err.Error(), "key reference") {
		t.Fatalf("wrong key reference accepted: %v", err)
	}
}

func TestTransformConfigWithEnvelopeAndLegacyCompatibility(t *testing.T) {
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"canary"}],"admin":{"api_key":"admin-canary"}}`)
	enc, n, err := TransformConfigWith(Cipher{env: env}, raw, true)
	if err != nil || n != 2 || !strings.Contains(string(enc), EnvelopePrefix) {
		t.Fatalf("encrypt n=%d err=%v", n, err)
	}
	plain, n, err := TransformConfigWith(Cipher{env: env}, enc, false)
	if err != nil || n != 2 || !strings.Contains(string(plain), "canary") {
		t.Fatalf("decrypt n=%d err=%v", n, err)
	}

	legacyKey := []byte("01234567890123456789012345678901")
	old, n, err := TransformConfig(raw, legacyKey, true)
	if err != nil || n != 2 || !strings.Contains(string(old), Prefix) {
		t.Fatalf("nxs1 encrypt n=%d err=%v", n, err)
	}
	back, n, err := TransformConfig(old, legacyKey, false)
	if err != nil || n != 2 || !strings.Contains(string(back), "canary") {
		t.Fatalf("nxs1 decrypt n=%d err=%v", n, err)
	}
	if _, _, err = TransformConfigWith(Cipher{env: env}, old, false); err == nil {
		t.Fatal("nxs1 decrypted in Transit mode without migration")
	}
	if _, _, err = TransformConfig(old, legacyKey, true); err != nil {
		t.Fatalf("nxs1 idempotent re-encrypt failed: %v", err)
	}
}

func TestMigrateConfigBytes(t *testing.T) {
	legacyKey := []byte("01234567890123456789012345678901")
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"migration-canary"}]}`)
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	migrated, n, err := MigrateConfigBytes(context.Background(), old, legacyKey, env)
	if err != nil || n != 1 {
		t.Fatalf("migrate n=%d err=%v", n, err)
	}
	if strings.Contains(string(migrated), Prefix) || !strings.Contains(string(migrated), EnvelopePrefix) {
		t.Fatal("migration did not replace nxs1 with nxs2")
	}
	plain, _, err := TransformConfigWith(Cipher{env: env}, migrated, false)
	if err != nil || !strings.Contains(string(plain), "migration-canary") {
		t.Fatalf("migrated config does not decrypt: %v", err)
	}
	again, n, err := MigrateConfigBytes(context.Background(), migrated, nil, env)
	if err != nil || n != 0 || string(again) != string(migrated) {
		t.Fatalf("migration not idempotent: n=%d err=%v", n, err)
	}
	if _, _, err = MigrateConfigBytes(context.Background(), old, []byte("wrong"), env); err == nil {
		t.Fatal("migration accepted wrong legacy key")
	}
	mixed := []byte(strings.Replace(string(old), `"api_key"`, `"other":"nxs2:abc","api_key"`, 1))
	if _, _, err = MigrateConfigBytes(context.Background(), mixed, legacyKey, env); err == nil {
		t.Fatal("mixed nxs1/nxs2 config accepted")
	}
}

func TestMigrateToEnvelopeFileAndInterruptedRecovery(t *testing.T) {
	legacyKey := []byte("01234567890123456789012345678901")
	env := newFakeEnvelope("key-a")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"migration-canary"}]}`)
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	n, err := MigrateToEnvelopeFile(context.Background(), path, legacyKey, env)
	if err != nil || n != 1 {
		t.Fatalf("migrate file n=%d err=%v", n, err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), Prefix) || !strings.Contains(string(got), EnvelopePrefix) {
		t.Fatal("file not migrated")
	}
	if _, err = os.Stat(path + migrationJournalPathSuffix); !os.IsNotExist(err) {
		t.Fatalf("journal not removed: %v", err)
	}
	backups, _ := filepath.Glob(path + ".*.enc.bak")
	if len(backups) != 1 {
		t.Fatalf("expected one migration backup, got %v", backups)
	}
	braw, _ := os.ReadFile(backups[0])
	backupPlain, err := DecryptBackupWith(Cipher{env: env}, braw)
	if err != nil || string(backupPlain) != string(old) {
		t.Fatalf("backup does not decrypt to the original bytes: %v", err)
	}

	// Interrupted before config replacement: journal exists, source config
	// is still nxs1. The next run resumes and completes.
	path2 := filepath.Join(dir, "config2.json")
	if err = os.WriteFile(path2, old, 0600); err != nil {
		t.Fatal(err)
	}
	target, _, err := MigrateConfigBytes(context.Background(), old, legacyKey, env)
	if err != nil {
		t.Fatal(err)
	}
	journal := migrationJournal{Version: 1, Backup: "backup.enc.bak", SourceSHA256: hashBytes(old), TargetSHA256: hashBytes(target)}
	jraw, _ := jsonMarshal(journal)
	if err = os.WriteFile(path2+migrationJournalPathSuffix, jraw, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err = MigrateToEnvelopeFile(context.Background(), path2, legacyKey, env); err != nil || n != 1 {
		t.Fatalf("resume before replacement n=%d err=%v", n, err)
	}
	if _, err = os.Stat(path2 + migrationJournalPathSuffix); !os.IsNotExist(err) {
		t.Fatalf("journal not removed after resume: %v", err)
	}

	// Interrupted after config replacement but before journal removal: the
	// committed nxs2 config is verified and the journal is cleaned up.
	path3 := filepath.Join(dir, "config3.json")
	if err = os.WriteFile(path3, target, 0600); err != nil {
		t.Fatal(err)
	}
	journal.SourceSHA256 = hashBytes(old)
	journal.TargetSHA256 = hashBytes(target)
	jraw, _ = jsonMarshal(journal)
	if err = os.WriteFile(path3+migrationJournalPathSuffix, jraw, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err = MigrateToEnvelopeFile(context.Background(), path3, nil, env); err != nil || n != 0 {
		t.Fatalf("resume after replacement n=%d err=%v", n, err)
	}
	if _, err = os.Stat(path3 + migrationJournalPathSuffix); !os.IsNotExist(err) {
		t.Fatalf("journal not removed after committed recovery: %v", err)
	}
}

func TestRewrapConfigBytesAndRevokedVersion(t *testing.T) {
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rewrap-canary"}]}`)
	enc, _, err := TransformConfigWith(Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := parseEnvelope(firstSecret(t, enc))
	rewrapped, n, err := RewrapConfigBytes(context.Background(), env, enc)
	if err != nil || n != 1 {
		t.Fatalf("rewrap n=%d err=%v", n, err)
	}
	after, _ := parseEnvelope(firstSecret(t, rewrapped))
	if before.Wrapped == after.Wrapped {
		t.Fatal("rewrap did not change the wrapped data key")
	}
	if _, _, err = TransformConfigWith(Cipher{env: env}, rewrapped, false); err != nil {
		t.Fatalf("rewrapped config does not decrypt: %v", err)
	}
	if _, err = openEnvelope(context.Background(), env, AAD("p", "other"), string(rewrapped)); err == nil {
		t.Fatal("rewrap lost AAD binding")
	}

	// Simulate a historical Transit key version that is no longer available.
	env.revokeAll()
	if _, _, err = TransformConfigWith(Cipher{env: env}, rewrapped, false); err == nil {
		t.Fatal("revoked key version decrypted")
	}
}

func TestConfiguredEnvelopeFailsClosedWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacyKey := []byte("01234567890123456789012345678901")
	raw, _, err := TransformConfig([]byte(`{"admin":{"api_key":"canary"}}`), legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "vault-transit")
	t.Setenv("NEXAROUTE_TRANSIT_ADDR", "")
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "")
	if _, _, err = ConfiguredEnvelope(); err == nil {
		t.Fatal("missing Transit configuration accepted")
	}
	if _, _, err = Key(path); err == nil {
		t.Fatal("Key() silently supplied a raw key in Transit mode")
	}
	if _, err = os.Stat(path + ".key"); !os.IsNotExist(err) {
		t.Fatalf("Transit mode created a fallback key file: %v", err)
	}
	if _, err = LoadCipher(path); err == nil {
		t.Fatal("LoadCipher fell back to a local key in Transit mode")
	}

	t.Setenv("NEXAROUTE_TRANSIT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("NEXAROUTE_TRANSIT_TOKEN", "test-token")
	env, enabled, err := ConfiguredEnvelope()
	if err != nil || !enabled || env == nil {
		t.Fatalf("valid Transit selection failed: %v", err)
	}
	if _, _, err = TransformConfigWith(Cipher{env: env}, raw, false); err == nil || !strings.Contains(err.Error(), "nxs1") {
		t.Fatalf("nxs1 was not rejected in Transit mode: %v", err)
	}
}

func firstSecret(t *testing.T, raw []byte) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	providers := root["providers"].([]any)
	return providers[0].(map[string]any)["api_key"].(string)
}

// jsonMarshal keeps the test helper independent of encoding details.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func TestNXS2WithoutTransitDoesNotCreateFallbackKey(t *testing.T) {
	env := newFakeEnvelope("key-a")
	raw, _, err := TransformConfigWith(Cipher{env: env}, []byte(`{"admin":{"api_key":"canary"}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "file")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	if _, err = LoadCipher(path); err == nil {
		t.Fatal("nxs2 config loaded without the Transit provider")
	}
	if _, err = os.Stat(path + ".key"); !os.IsNotExist(err) {
		t.Fatalf("nxs2 config created a fallback key: %v", err)
	}
}
