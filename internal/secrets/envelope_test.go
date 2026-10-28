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
// nxs2 format without a real Vault server. It implements the ADR 0007 §4
// interface: version-returning Wrap/Unwrap/Rewrap, no aad parameter (the
// design sends no associated data or context to Transit).
type fakeEnvelope struct {
	mu      sync.Mutex
	keyRef  string
	version int
	next    int
	keys    map[string]fakeWrapped
	fail    error
}

type fakeWrapped struct {
	dek     []byte
	version int
}

func newFakeEnvelope(id string) *fakeEnvelope {
	return &fakeEnvelope{keyRef: TransitKeyRefHash("", "transit", id), version: 1, keys: map[string]fakeWrapped{}}
}

func (f *fakeEnvelope) KeyRefHash() string { return f.keyRef }

func (f *fakeEnvelope) WrapDEK(_ context.Context, dek []byte) (string, int, error) {
	if f.fail != nil {
		return "", 0, f.fail
	}
	if len(dek) != 32 {
		return "", 0, errors.New("fake: bad dek")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	// Wrapped bodies must be strict standard base64 (§3.2): "key-N"
	// contains '-', which standard base64 rejects.
	id := fmt.Sprintf("vault:v%d:%s", f.version, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("key-%d", f.next))))
	f.keys[id] = fakeWrapped{dek: append([]byte(nil), dek...), version: f.version}
	return id, f.version, nil
}

func (f *fakeEnvelope) UnwrapDEK(_ context.Context, wrapped string) ([]byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[wrapped]
	if !ok {
		return nil, 0, errors.New("fake: unknown wrapped key or revoked key version")
	}
	return append([]byte(nil), k.dek...), k.version, nil
}

func (f *fakeEnvelope) RewrapDEK(_ context.Context, wrapped string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[wrapped]
	if !ok {
		return "", 0, errors.New("fake: unknown wrapped key or revoked key version")
	}
	if k.version == f.version {
		return wrapped, f.version, nil
	}
	f.next++
	id := fmt.Sprintf("vault:v%d:%s", f.version, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("key-%d", f.next))))
	f.keys[id] = fakeWrapped{dek: append([]byte(nil), k.dek...), version: f.version}
	return id, f.version, nil
}

func (f *fakeEnvelope) Close() error { return nil }

func (f *fakeEnvelope) rotate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
}

func (f *fakeEnvelope) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = map[string]fakeWrapped{}
}

func sealField(t *testing.T, env EnvelopeKeyProvider, owner, field, plain string) string {
	t.Helper()
	text, err := sealEnvelope(context.Background(), env, owner, field, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func openField(t *testing.T, env EnvelopeKeyProvider, owner, field, text string) string {
	t.Helper()
	plain, err := openEnvelope(context.Background(), env, owner, field, text)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

func TestEnvelopeRoundTripAndStrictSchema(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "provider-a", "api_key", "roundtrip-secret")
	if !strings.HasPrefix(text, EnvelopePrefix) || strings.Contains(text, "roundtrip-secret") {
		t.Fatalf("unexpected envelope: %q", text)
	}
	meta, err := parseEnvelope(text)
	if err != nil {
		t.Fatal(err)
	}
	if meta.EnvelopeVersion != 2 || meta.Provider != "vault-transit" || meta.KeyRefSHA256 != env.KeyRefHash() || meta.TransitKeyVersion != 1 || meta.WrappedDEK == "" {
		t.Fatalf("bad metadata: %+v", meta)
	}
	if got := openField(t, env, "provider-a", "api_key", text); got != "roundtrip-secret" {
		t.Fatalf("round trip: %q", got)
	}
	// The inner JSON carries exactly the §3.2 field names.
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, EnvelopePrefix))
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	want := []string{"envelope_version", "provider", "key_ref_sha256", "transit_key_version", "wrapped_dek", "data_nonce_b64url", "data_ciphertext_b64url"}
	if len(obj) != len(want) {
		t.Fatalf("inner JSON must have exactly %d fields, got %d: %v", len(want), len(obj), obj)
	}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			t.Fatalf("inner JSON missing field %q", k)
		}
	}
}

func TestEnvelopeAuthenticationBinding(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "provider-a", "api_key", "canary")
	// Owner move, field move, and key-reference move all fail.
	if _, err := openEnvelope(context.Background(), env, "provider-b", "api_key", text); err == nil {
		t.Fatal("owner move authenticated")
	}
	if _, err := openEnvelope(context.Background(), env, "provider-a", "other", text); err == nil {
		t.Fatal("field move authenticated")
	}
	other := newFakeEnvelope("key-b")
	if _, err := openEnvelope(context.Background(), other, "provider-a", "api_key", text); err == nil {
		t.Fatal("wrong key reference authenticated")
	}
	// Tamper each component independently.
	meta, _ := parseEnvelope(text)
	tamper := func(mut func(*envelopeMeta)) string {
		m := meta
		mut(&m)
		enc, err := encodeEnvelope(m)
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	cases := map[string]string{
		"nonce": tamper(func(m *envelopeMeta) {
			m.DataNonceB64URL = base64.RawURLEncoding.EncodeToString([]byte("0123456789ab"))
		}),
		"data": tamper(func(m *envelopeMeta) {
			m.DataCiphertextB64 = base64.RawURLEncoding.EncodeToString([]byte("tampered-tampered!"))
		}),
		"version":    tamper(func(m *envelopeMeta) { m.EnvelopeVersion = 3 }),
		"provider":   tamper(func(m *envelopeMeta) { m.Provider = "other" }),
		"keyref":     tamper(func(m *envelopeMeta) { m.KeyRefSHA256 = strings.Repeat("0", 64) }),
		"keyversion": tamper(func(m *envelopeMeta) { m.TransitKeyVersion = 99 }),
	}
	for name, enc := range cases {
		if _, err := openEnvelope(context.Background(), env, "provider-a", "api_key", enc); err == nil {
			t.Fatalf("tampered %s authenticated", name)
		}
	}
	// Wrapped-DEK transplant between two envelopes fails at the GCM open:
	// the transplanted DEK decrypts the wrong data ciphertext.
	second := sealField(t, env, "provider-a", "api_key", "second-secret")
	meta2, _ := parseEnvelope(second)
	swapped := meta
	swapped.WrappedDEK = meta2.WrappedDEK
	enc, _ := encodeEnvelope(swapped)
	if _, err := openEnvelope(context.Background(), env, "provider-a", "api_key", enc); err == nil {
		t.Fatal("wrapped-DEK transplant authenticated")
	}
	// Unknown wrapped key fails.
	unknown := meta
	unknown.WrappedDEK = "vault:v1:AAAA"
	enc, _ = encodeEnvelope(unknown)
	if _, err := openEnvelope(context.Background(), env, "provider-a", "api_key", enc); err == nil {
		t.Fatal("unknown wrapped key authenticated")
	}
}

func TestEnvelopeParserStrictness(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "p", "api_key", "x")
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, EnvelopePrefix))
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	encode := func(o map[string]any) string {
		b, _ := json.Marshal(o)
		return EnvelopePrefix + base64.RawURLEncoding.EncodeToString(b)
	}
	clone := func() map[string]any {
		out := map[string]any{}
		for k, v := range obj {
			out[k] = v
		}
		return out
	}
	cases := map[string]string{}
	o := clone()
	o["surprise"] = 1
	cases["unknown field"] = encode(o)
	o = clone()
	o["envelope_version"] = 1
	cases["version 1"] = encode(o)
	o = clone()
	o["envelope_version"] = 3
	cases["version 3"] = encode(o)
	o = clone()
	o["provider"] = "vault-kv"
	cases["wrong provider"] = encode(o)
	o = clone()
	o["key_ref_sha256"] = "ABCDEF"
	cases["short uppercase keyref"] = encode(o)
	o = clone()
	o["transit_key_version"] = 0
	cases["version 0"] = encode(o)
	o = clone()
	o["transit_key_version"] = 2
	cases["version mismatch"] = encode(o)
	o = clone()
	o["wrapped_dek"] = "vault:v01:AAAA"
	cases["leading-zero version"] = encode(o)
	o = clone()
	o["wrapped_dek"] = "vault:v0:AAAA"
	cases["version zero wrapped"] = encode(o)
	o = clone()
	o["wrapped_dek"] = "vault:v1:AAAA:extra"
	cases["extra colon"] = encode(o)
	o = clone()
	o["wrapped_dek"] = "vault:v1:not base64!"
	cases["bad wrapped base64"] = encode(o)
	o = clone()
	o["wrapped_dek"] = "vault:v1:"
	cases["empty wrapped"] = encode(o)
	o = clone()
	o["data_nonce_b64url"] = base64.RawURLEncoding.EncodeToString([]byte("short"))
	cases["short nonce"] = encode(o)
	o = clone()
	o["data_ciphertext_b64url"] = base64.RawURLEncoding.EncodeToString([]byte("tiny"))
	cases["short data"] = encode(o)
	cases["padded outer"] = EnvelopePrefix + base64.URLEncoding.EncodeToString(raw)
	cases["trailing data"] = encode(clone()) + "!!!"
	cases["not base64"] = "nxs2:not-base64!"
	cases["empty"] = "nxs2:"
	for name, enc := range cases {
		if _, err := parseEnvelope(enc); err == nil {
			t.Errorf("parser accepted %s", name)
		}
	}
	// Duplicate keys and trailing JSON data are rejected even though
	// encoding/json alone would accept them.
	dup := strings.Replace(string(raw), `"provider"`, `"provider":"vault-transit","provider"`, 1)
	if _, err := parseEnvelope(EnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(dup))); err == nil {
		t.Error("parser accepted duplicate JSON key")
	}
	trailing := string(raw) + ` {}`
	if _, err := parseEnvelope(EnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(trailing))); err == nil {
		t.Error("parser accepted trailing JSON data")
	}
	// Wrapped-DEK shape table.
	for _, bad := range []string{"", "vault:", "vault:v1", "vault:v1:", "v1:AAAA", "vault:v1:AAAA:BBB", "vault:vx:AAAA", "vault:v-1:AAAA", "vault:v1:****"} {
		if _, err := ParseTransitCiphertextVersion(bad); err == nil {
			t.Errorf("wrapped parser accepted %q", bad)
		}
	}
	if v, err := ParseTransitCiphertextVersion("vault:v12:QUJD"); err != nil || v != 12 {
		t.Errorf("wrapped parser rejected valid v12: %d %v", v, err)
	}
}

func TestEnvelopeCrossRecordSubstitution(t *testing.T) {
	env := newFakeEnvelope("key-a")
	// Same key, same owner, different field: the AAD binds the field.
	a := sealField(t, env, "p", "api_key", "secret-a")
	if _, err := openEnvelope(context.Background(), env, "p", "proxy_url", a); err == nil {
		t.Fatal("cross-field substitution authenticated")
	}
	// Whole-envelope transplant across configs that share key reference,
	// owner, and field SUCCEEDS. This pins the F1 residual: the design
	// does not bind config identity, so the mitigation is operational
	// (distinct Transit keys per environment/tenant). The test
	// documents the surface; it must flip if config-identity AAD lands.
	// One fake models one Vault key shared by two configs: the envelope
	// string moves from config A to config B unchanged and still opens.
	if got := openField(t, env, "p", "api_key", a); got != "secret-a" {
		t.Fatalf("cross-config transplant with shared key unexpectedly failed: %q", got)
	}
	// A different Vault key (different key reference) still refuses it.
	otherKey := newFakeEnvelope("key-b")
	if _, err := openEnvelope(context.Background(), otherKey, "p", "api_key", a); err == nil {
		t.Fatal("cross-key transplant authenticated")
	}
}

func TestTransformConfigWithEnvelopeAndLegacyCompatibility(t *testing.T) {
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"canary"}],"admin":{"api_key":"admin-canary"}}`)
	enc, n, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true)
	if err != nil || n != 2 || !strings.Contains(string(enc), EnvelopePrefix) {
		t.Fatalf("encrypt n=%d err=%v", n, err)
	}
	plain, n, err := TransformConfigWith(context.Background(), Cipher{env: env}, enc, false)
	if err != nil || n != 2 || !strings.Contains(string(plain), "canary") {
		t.Fatalf("decrypt n=%d err=%v", n, err)
	}
	// nxs1 byte-for-byte compatibility is untouched.
	legacyKey := []byte("01234567890123456789012345678901")
	old, n, err := TransformConfig(raw, legacyKey, true)
	if err != nil || n != 2 || !strings.Contains(string(old), Prefix) {
		t.Fatalf("nxs1 encrypt n=%d err=%v", n, err)
	}
	back, n, err := TransformConfig(old, legacyKey, false)
	if err != nil || n != 2 || !strings.Contains(string(back), "canary") {
		t.Fatalf("nxs1 decrypt n=%d err=%v", n, err)
	}
	if _, _, err = TransformConfigWith(context.Background(), Cipher{env: env}, old, false); err == nil {
		t.Fatal("nxs1 decrypted in Transit mode without migration")
	}
	if _, _, err = TransformConfig(old, legacyKey, true); err != nil {
		t.Fatalf("nxs1 idempotent re-encrypt failed: %v", err)
	}
	// Mixed, unknown, and plaintext-in-Transit all fail closed.
	mixed := []byte(`{"providers":[{"id":"p","api_key":"` + firstField(t, old) + `"},{"id":"q","api_key":"` + firstField(t, enc) + `"}]}`)
	if _, _, err = TransformConfigWith(context.Background(), Cipher{env: env}, mixed, false); err == nil {
		t.Fatal("mixed nxs1+nxs2 transformed")
	}
	unknown := []byte(`{"providers":[{"id":"p","api_key":"nxs3:whatever"}]}`)
	if _, _, err = TransformConfigWith(context.Background(), Cipher{env: env}, unknown, false); err == nil {
		t.Fatal("unknown envelope transformed")
	}
	if _, _, err = TransformConfigWith(context.Background(), Cipher{env: env}, unknown, true); err == nil {
		t.Fatal("unknown envelope sealed")
	}
	if _, _, err = TransformConfigWith(context.Background(), Cipher{env: env}, raw, false); err == nil {
		t.Fatal("plaintext secret field decrypted in Transit mode")
	}
	// Sealing a genuinely new plaintext field in Transit mode works.
	if _, n, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true); err != nil || n != 2 {
		t.Fatalf("new plaintext field seal failed: n=%d err=%v", n, err)
	}
	// Sealed backup helpers are raw-only in Transit mode.
	if _, err := EncryptedBackupWith(Cipher{env: env}, raw); err == nil {
		t.Fatal("sealed backup created in Transit mode")
	}
	rawBackup, err := EncryptedBackupWith(Cipher{raw: legacyKey}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := DecryptBackupWith(Cipher{raw: legacyKey}, rawBackup); err != nil || string(back) != string(raw) {
		t.Fatalf("raw sealed backup round-trip failed: %v", err)
	}
}

func firstField(t *testing.T, config []byte) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(config, &root); err != nil {
		t.Fatal(err)
	}
	ps := root["providers"].([]any)
	return ps[0].(map[string]any)["api_key"].(string)
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
	plain, n, err := TransformConfigWith(context.Background(), Cipher{env: env}, migrated, false)
	if err != nil || n != 1 || !strings.Contains(string(plain), "migration-canary") {
		t.Fatalf("migrated decrypt n=%d err=%v", n, err)
	}
	// Already migrated: verified, returned unchanged, zero count.
	again, n, err := MigrateConfigBytes(context.Background(), migrated, nil, env)
	if err != nil || n != 0 || string(again) != string(migrated) {
		t.Fatalf("already-migrated must verify and return unchanged: n=%d err=%v", n, err)
	}
	// Plaintext fails (never auto-migrated into Transit).
	if _, _, err := MigrateConfigBytes(context.Background(), raw, nil, env); err == nil {
		t.Fatal("plaintext migrated into Transit")
	}
	// Wrong legacy key fails before any Vault wrap.
	if _, _, err := MigrateConfigBytes(context.Background(), old, []byte("01234567890123456789012345678902"), env); err == nil {
		t.Fatal("wrong legacy key migrated")
	}
	// Missing legacy key fails.
	if _, _, err := MigrateConfigBytes(context.Background(), old, nil, env); err == nil {
		t.Fatal("missing legacy key migrated")
	}
	// Canary failure aborts with no output.
	env.fail = errors.New("fake: vault down")
	if _, _, err := MigrateConfigBytes(context.Background(), old, legacyKey, env); err == nil {
		t.Fatal("canary failure migrated")
	}
	env.fail = nil
	// Field-less config is a no-op.
	empty := []byte(`{"providers":[]}`)
	out, n, err := MigrateConfigBytes(context.Background(), empty, nil, env)
	if err != nil || n != 0 || string(out) != string(empty) {
		t.Fatalf("field-less migrate must be a no-op: n=%d err=%v", n, err)
	}
	// Mixed and unknown fail.
	mixed := []byte(`{"providers":[{"id":"p","api_key":"` + firstField(t, old) + `"},{"id":"q","api_key":"` + firstField(t, migrated) + `"}]}`)
	if _, _, err := MigrateConfigBytes(context.Background(), mixed, legacyKey, env); err == nil {
		t.Fatal("mixed config migrated")
	}
	unknown := []byte(`{"providers":[{"id":"p","api_key":"nxs9:nope"}]}`)
	if _, _, err := MigrateConfigBytes(context.Background(), unknown, legacyKey, env); err == nil {
		t.Fatal("unknown envelope migrated")
	}
}

func TestRewrapConfigBytes(t *testing.T) {
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rewrap-canary"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	// Already at latest: no change, alreadyLatest true.
	out, n, latest, err := RewrapConfigBytes(context.Background(), env, enc)
	if err != nil || n != 0 || !latest {
		t.Fatalf("already-latest rewrap must be a no-op: n=%d latest=%v err=%v", n, latest, err)
	}
	if _, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, out, false); err != nil {
		t.Fatalf("already-latest output must still decrypt: %v", err)
	}
	// After rotation: rewrap moves the version, data unchanged.
	env.rotate()
	out, n, latest, err = RewrapConfigBytes(context.Background(), env, enc)
	if err != nil || n != 1 || latest {
		t.Fatalf("rewrap after rotation: n=%d latest=%v err=%v", n, latest, err)
	}
	plain, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, out, false)
	if err != nil || !strings.Contains(string(plain), "rewrap-canary") {
		t.Fatalf("rewrapped decrypt failed: %v", err)
	}
	meta, err := parseEnvelope(firstField(t, out))
	if err != nil || meta.TransitKeyVersion != 2 {
		t.Fatalf("rewrapped version must be 2: %+v %v", meta, err)
	}
	// A field that no longer decrypts aborts before any rewrap call.
	env2 := newFakeEnvelope("key-a")
	enc2, _, err := TransformConfigWith(context.Background(), Cipher{env: env2}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	env2.revokeAll()
	if _, _, _, err := RewrapConfigBytes(context.Background(), env2, enc2); err == nil {
		t.Fatal("undecryptable field rewrapped")
	}
	// nxs1 and plaintext refuse rewrap.
	legacyKey := []byte("01234567890123456789012345678901")
	old, _, _ := TransformConfig(raw, legacyKey, true)
	if _, _, _, err := RewrapConfigBytes(context.Background(), env, old); err == nil {
		t.Fatal("nxs1 rewrapped")
	}
	if _, _, _, err := RewrapConfigBytes(context.Background(), env, raw); err == nil {
		t.Fatal("plaintext rewrapped")
	}
}

func TestClassifier(t *testing.T) {
	env := newFakeEnvelope("key-a")
	nxs2 := sealField(t, env, "p", "api_key", "v")
	cases := map[string]EnvelopeClass{
		"":                 ClassEmpty,
		"plaintext":        ClassPlaintext,
		"nxs1:abc":         ClassNXS1,
		nxs2:               ClassNXS2,
		"nxs3:whatever":    ClassUnknown,
		"nxs2:not-base64!": ClassUnknown,
		"nxs2:AAAA":        ClassUnknown,
		"nxs10:whatever":   ClassUnknown,
		"nxs:whatever":     ClassPlaintext,
	}
	for s, want := range cases {
		if got := ClassifyValue(s); got != want {
			t.Errorf("ClassifyValue(%q) = %v, want %v", s, got, want)
		}
	}
	cls, err := ClassifyConfig([]byte(`{"providers":[{"id":"p","api_key":"` + nxs2 + `"},{"id":"q","api_key":"plain"}],"admin":{"api_key":""}}`))
	if err != nil || cls.NXS2 != 1 || cls.Plaintext != 1 {
		t.Fatalf("classify config: %+v %v", cls, err)
	}
	// Distinct versions and key-ref info.
	_, versions, err := TransitEnvelopeInfo([]byte(`{"providers":[{"id":"p","api_key":"` + nxs2 + `"}]}`))
	if err != nil || len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("envelope info: %v %v", versions, err)
	}
	other := newFakeEnvelope("key-b")
	otherEnv := sealField(t, other, "p", "api_key", "v")
	mixed := []byte(`{"providers":[{"id":"p","api_key":"` + nxs2 + `"},{"id":"q","api_key":"` + otherEnv + `"}]}`)
	if _, _, err := TransitEnvelopeInfo(mixed); err == nil {
		t.Fatal("disagreeing key references accepted")
	}
}

func TestMigrationFileFlows(t *testing.T) {
	legacyKey := []byte("01234567890123456789012345678901")
	env := newFakeEnvelope("key-a")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"file-canary"}]}`)
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(old, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := MigrateToEnvelopeFile(context.Background(), path, legacyKey, env)
	if err != nil || result.Migrated != 1 {
		t.Fatalf("file migrate: %+v %v", result, err)
	}
	// Verbatim backup exists and equals the pre-migration bytes.
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(append(old, '\n')) {
		t.Fatal("migration backup is not the verbatim pre-migration bytes")
	}
	if st, err := os.Stat(result.BackupPath); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("migration backup must be 0600: %v", err)
	}
	// Migrated file decrypts under Transit.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, after, false); err != nil {
		t.Fatalf("migrated file does not decrypt: %v", err)
	}
	// Re-running is verify-only: no rewrite, no new backup.
	before, _ := os.ReadFile(path)
	result2, err := MigrateToEnvelopeFile(context.Background(), path, nil, env)
	if err != nil || result2.Migrated != 0 || result2.BackupPath != "" {
		t.Fatalf("re-run must verify only: %+v %v", result2, err)
	}
	after2, _ := os.ReadFile(path)
	if string(before) != string(after2) {
		t.Fatal("re-run rewrote an already-migrated config")
	}
	// Migration without the legacy key on an nxs1 config fails and
	// leaves the file byte-identical.
	path2 := filepath.Join(dir, "config2.json")
	if err := os.WriteFile(path2, append(old, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateToEnvelopeFile(context.Background(), path2, nil, env); err == nil {
		t.Fatal("migration without legacy key succeeded")
	}
	unchanged, _ := os.ReadFile(path2)
	if string(unchanged) != string(append(old, '\n')) {
		t.Fatal("failed migration modified the file")
	}
	// Lock contention fails fast instead of merging.
	release, err := acquireSecretLock(path2 + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateToEnvelopeFile(context.Background(), path2, legacyKey, env); err == nil {
		t.Fatal("concurrent migration ran despite the lock")
	}
	release()
}

func TestRewrapFileFlows(t *testing.T) {
	env := newFakeEnvelope("key-a")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rewrap-file"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(enc, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// Already latest: nothing written, no backup.
	result, err := RewrapConfigFile(context.Background(), path, env)
	if err != nil || !result.AlreadyLatest || result.BackupPath != "" {
		t.Fatalf("already-latest rewrap must write nothing: %+v %v", result, err)
	}
	before, _ := os.ReadFile(path)
	// After rotation: rewrapped + verbatim backup, file decrypts.
	env.rotate()
	result, err = RewrapConfigFile(context.Background(), path, env)
	if err != nil || result.Rewrapped != 1 || result.BackupPath == "" {
		t.Fatalf("rewrap file: %+v %v", result, err)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil || string(backup) != string(before) {
		t.Fatal("rewrap backup is not the verbatim pre-rewrap bytes")
	}
	after, _ := os.ReadFile(path)
	if _, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, after, false); err != nil {
		t.Fatalf("rewrapped file does not decrypt: %v", err)
	}
	// Failed rewrap leaves the file byte-identical with no new backup.
	env.revokeAll()
	failed, _ := os.ReadFile(path)
	entriesBefore, _ := os.ReadDir(dir)
	if _, err := RewrapConfigFile(context.Background(), path, env); err == nil {
		t.Fatal("failed rewrap succeeded")
	}
	failedAfter, _ := os.ReadFile(path)
	if string(failed) != string(failedAfter) {
		t.Fatal("failed rewrap modified the file")
	}
	entriesAfter, _ := os.ReadDir(dir)
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatal("failed rewrap created files")
	}
}

func FuzzParseEnvelope(f *testing.F) {
	f.Add("nxs2:not-base64!")
	f.Add("nxs2:AAAA")
	f.Add("")
	f.Add("nxs3:x")
	f.Fuzz(func(t *testing.T, s string) {
		meta, err := parseEnvelope(s)
		if err != nil {
			return
		}
		if meta.EnvelopeVersion != 2 || meta.Provider != "vault-transit" || len(meta.KeyRefSHA256) != 64 {
			t.Fatalf("parser accepted an invalid envelope: %+v", meta)
		}
	})
}
