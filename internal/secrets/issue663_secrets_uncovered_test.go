package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// issue663CoreBadUnwrap delegates everything to a real fake envelope
// provider except UnwrapDEK, which returns unrelated 32-byte key bytes.
// It is still a fake: no Vault, no network, no credential.
type issue663CoreBadUnwrap struct{ EnvelopeKeyProvider }

func (p issue663CoreBadUnwrap) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	return []byte("fedcba9876543210fedcba9876543210"), 1, nil
}

// issue663CoreShortUnwrap returns a DEK of the wrong length.
type issue663CoreShortUnwrap struct{ EnvelopeKeyProvider }

func (p issue663CoreShortUnwrap) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	return []byte("short"), 1, nil
}

// issue663CoreUnwrapVersion reports a version that never matches the
// envelope's recorded version, exercising the version-consistency check.
type issue663CoreUnwrapVersion struct{ EnvelopeKeyProvider }

func (p issue663CoreUnwrapVersion) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	dek, _, err := p.EnvelopeKeyProvider.UnwrapDEK(ctx, wrapped)
	return dek, 99, err
}

// issue663CoreEmptyWrap returns an empty wrapped DEK.
type issue663CoreEmptyWrap struct{ EnvelopeKeyProvider }

func (p issue663CoreEmptyWrap) WrapDEK(context.Context, []byte) (string, int, error) {
	return "", 1, nil
}

func TestIssue663CoreCipherKeyRefHashSealOpen(t *testing.T) {
	rawKey := []byte("01234567890123456789012345678901")
	raw := Cipher{raw: rawKey}
	if got := raw.KeyRefHash(); got != "" {
		t.Fatalf("raw KeyRefHash = %q, want empty", got)
	}
	sealed, err := raw.Seal([]byte("aad"), []byte("plain-secret"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := raw.Open([]byte("aad"), sealed)
	if err != nil || string(opened) != "plain-secret" {
		t.Fatalf("raw Seal/Open = %q, %v", opened, err)
	}
	if _, err := (Cipher{raw: []byte("short")}).Seal([]byte("aad"), []byte("x")); err == nil {
		t.Fatal("Seal with a short raw key must fail")
	}
	if _, err := raw.Open([]byte("other-aad"), sealed); err == nil {
		t.Fatal("Open with the wrong AAD must fail authentication")
	}

	env := newFakeEnvelope("key-a")
	enc := Cipher{env: env}
	if got := enc.KeyRefHash(); got != env.KeyRefHash() || got == "" {
		t.Fatalf("envelope KeyRefHash = %q, want %q", got, env.KeyRefHash())
	}
	if _, err := enc.Seal([]byte("aad"), []byte("x")); err == nil || !strings.Contains(err.Error(), "SealField") {
		t.Fatalf("envelope Seal must demand SealField, got %v", err)
	}
	if _, err := enc.Open([]byte("aad"), sealed); err == nil || !strings.Contains(err.Error(), "OpenField") {
		t.Fatalf("envelope Open must demand OpenField, got %v", err)
	}
}

func TestIssue663CoreSealFieldOpenFieldBranches(t *testing.T) {
	ctx := context.Background()
	rawKey := []byte("01234567890123456789012345678901")
	raw := Cipher{raw: rawKey}
	sealed, err := raw.SealField(ctx, "p", "api_key", []byte("raw-secret"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := raw.OpenField(ctx, "p", "api_key", sealed)
	if err != nil || string(opened) != "raw-secret" {
		t.Fatalf("raw SealField/OpenField = %q, %v", opened, err)
	}
	if _, err := (Cipher{raw: []byte("short")}).SealField(ctx, "p", "api_key", []byte("x")); err == nil {
		t.Fatal("SealField with a short raw key must fail")
	}

	env := newFakeEnvelope("key-a")
	enc := Cipher{env: env}
	sealed, err = enc.SealField(ctx, "p", "api_key", []byte("env-secret"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err = enc.OpenField(ctx, "p", "api_key", sealed)
	if err != nil || string(opened) != "env-secret" {
		t.Fatalf("envelope SealField/OpenField = %q, %v", opened, err)
	}
	if _, err := enc.OpenField(ctx, "p", "api_key", "plain-secret"); err == nil {
		t.Fatal("plaintext field under an envelope cipher must fail closed")
	}
	if _, err := enc.OpenField(ctx, "p", "api_key", "nxs3:whatever"); err == nil {
		t.Fatal("unknown envelope under an envelope cipher must fail closed")
	}
	if _, err := raw.OpenField(ctx, "p", "api_key", sealed); err == nil {
		t.Fatal("nxs2 envelope under a raw cipher must fail closed")
	}
	if _, err := raw.OpenField(ctx, "p", "api_key", "nxs3:whatever"); err == nil {
		t.Fatal("unknown envelope under a raw cipher must fail closed")
	}
	legacyKey := []byte("01234567890123456789012345678901")
	old, _, err := TransformConfig([]byte(`{"providers":[{"id":"p","api_key":"legacy"}]}`), legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	nxs1 := firstField(t, old)
	if _, err := enc.OpenField(ctx, "p", "api_key", nxs1); err == nil {
		t.Fatal("nxs1 envelope under an envelope cipher must fail closed")
	}
}

func TestIssue663CoreEnvelopeProviderFailureShapes(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("key-a")
	if _, err := sealEnvelope(ctx, issue663CoreEmptyWrap{env}, "p", "api_key", []byte("x")); err == nil || !strings.Contains(err.Error(), "empty wrapped") {
		t.Fatalf("empty wrapped DEK must fail, got %v", err)
	}
	text := sealField(t, env, "p", "api_key", "v")
	if _, err := openEnvelope(ctx, issue663CoreUnwrapVersion{env}, "p", "api_key", text); err == nil || !strings.Contains(err.Error(), "key version inconsistent") {
		t.Fatalf("version-mismatched unwrap must fail, got %v", err)
	}
	if _, err := openEnvelope(ctx, issue663CoreShortUnwrap{env}, "p", "api_key", text); err == nil || !strings.Contains(err.Error(), "invalid data key length") {
		t.Fatalf("wrong-length unwrapped DEK must fail, got %v", err)
	}
	if _, err := openEnvelope(ctx, newFakeEnvelope("key-b"), "p", "api_key", text); err == nil {
		t.Fatal("envelope opened under a different key reference must fail")
	}
}

func TestIssue663CoreSecretFieldMap(t *testing.T) {
	raw := []byte(`{"name":"keep-me","providers":[{"id":"p","api_key":"a1","proxy_url":"http://proxy","headers":{"X-Token":"h1"},"credentials":[{"api_key":"c1"}]},{"id":"q","api_key":123}],"decision_providers":[{"id":"d","api_key":"d1"},{"id":"e"}],"admin":{"api_key":"adm"},"client_auth":{"keys":["k1","k2"]}}`)
	m, err := SecretFieldMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"p:api_key": "a1", "p:proxy_url": "http://proxy", "p:header:X-Token": "h1",
		"p:credentials[0].api_key": "c1", "d:api_key": "d1", "admin:api_key": "adm",
		"client_auth:keys[0]": "k1", "client_auth:keys[1]": "k2",
	}
	if len(m) != len(want) {
		t.Fatalf("SecretFieldMap has %d entries, want %d: %v", len(m), len(want), m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("SecretFieldMap[%q] = %q, want %q", k, m[k], v)
		}
	}
	if _, err := SecretFieldMap([]byte(`{not json`)); err == nil {
		t.Fatal("SecretFieldMap must reject invalid JSON")
	}
}

func TestIssue663CoreSubstituteSecretFields(t *testing.T) {
	raw := []byte(`{"name":"keep-me","providers":[{"id":"p","api_key":"a1","proxy_url":"http://proxy"}],"admin":{"api_key":"adm"}}`)
	out, err := SubstituteSecretFields(raw, func(owner, field, current string) (string, error) {
		if field == "api_key" {
			return "sealed:" + current, nil
		}
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	ps := root["providers"].([]any)
	p := ps[0].(map[string]any)
	if p["api_key"] != "sealed:a1" || p["proxy_url"] != "http://proxy" || root["name"] != "keep-me" {
		t.Fatalf("substitution rewrote the wrong fields: %s", out)
	}
	if root["admin"].(map[string]any)["api_key"] != "sealed:adm" {
		t.Fatalf("admin api_key was not substituted: %s", out)
	}
	if _, err := SubstituteSecretFields(raw, func(string, string, string) (string, error) {
		return "", errors.New("substitution failed")
	}); err == nil {
		t.Fatal("a substitution error must propagate")
	}
	if _, err := SubstituteSecretFields([]byte(`{bad`), func(string, string, string) (string, error) {
		return "", nil
	}); err == nil {
		t.Fatal("SubstituteSecretFields must reject invalid JSON")
	}
}

func TestIssue663CoreRewrapConfigBytes(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rewrap-canary"}]}`)
	enc, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RewrapConfigBytes(ctx, nil, enc); err == nil {
		t.Fatal("rewrap without a provider must fail")
	}
	if _, _, _, err := RewrapConfigBytes(ctx, env, []byte(`{"providers":[{"id":"p","api_key":"nxs3:nope"}]}`)); err == nil {
		t.Fatal("rewrap must refuse unknown envelopes")
	}
	legacyKey := []byte("01234567890123456789012345678901")
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RewrapConfigBytes(ctx, env, old); err == nil {
		t.Fatal("rewrap must refuse nxs1 configs")
	}
	if _, _, _, err := RewrapConfigBytes(ctx, env, raw); err == nil {
		t.Fatal("rewrap must refuse plaintext configs")
	}
	other := newFakeEnvelope("key-b")
	if _, _, _, err := RewrapConfigBytes(ctx, other, enc); err == nil {
		t.Fatal("rewrap under a different key reference must fail")
	}
	out, n, already, err := RewrapConfigBytes(ctx, env, enc)
	if err != nil || n != 0 || !already {
		t.Fatalf("no-op rewrap = n=%d already=%v err=%v", n, already, err)
	}
	plain, _, err := TransformConfigWith(ctx, Cipher{env: env}, out, false)
	if err != nil || !strings.Contains(string(plain), "rewrap-canary") {
		t.Fatalf("no-op rewrap output must still decrypt, err=%v", err)
	}
	env.rotate()
	out, n, already, err = RewrapConfigBytes(ctx, env, enc)
	if err != nil || n != 1 || already {
		t.Fatalf("rotated rewrap = n=%d already=%v err=%v", n, already, err)
	}
	plain, _, err = TransformConfigWith(ctx, Cipher{env: env}, out, false)
	if err != nil || !strings.Contains(string(plain), "rewrap-canary") {
		t.Fatalf("rewrapped config must decrypt to the original plaintext, err=%v", err)
	}
	meta, err := parseEnvelope(firstField(t, out))
	if err != nil || meta.TransitKeyVersion != 2 {
		t.Fatalf("rewrapped envelope version = %d, err=%v", meta.TransitKeyVersion, err)
	}
	revoked := newFakeEnvelope("key-a")
	enc2, _, err := TransformConfigWith(ctx, Cipher{env: revoked}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	revoked.revokeAll()
	if _, _, _, err := RewrapConfigBytes(ctx, revoked, enc2); err == nil {
		t.Fatal("rewrap must fail when the pre-verification decrypt fails")
	}
}

func TestIssue663CoreMigrateConfigBytes(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"migrate-canary"}]}`)
	if _, _, err := MigrateConfigBytes(ctx, raw, nil, nil); err == nil {
		t.Fatal("migration without a provider must fail")
	}
	if _, _, err := MigrateConfigBytes(ctx, []byte(`{"providers":[{"id":"p","api_key":"nxs3:nope"}]}`), nil, env); err == nil {
		t.Fatal("migration must refuse unknown envelopes")
	}
	legacyKey := []byte("01234567890123456789012345678901")
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	enc, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	mixed := []byte(`{"providers":[{"id":"p","api_key":"` + firstField(t, old) + `"},{"id":"q","api_key":"` + firstField(t, enc) + `"}]}`)
	if _, _, err := MigrateConfigBytes(ctx, mixed, legacyKey, env); err == nil {
		t.Fatal("migration must refuse mixed formats")
	}
	if _, _, err := MigrateConfigBytes(ctx, raw, legacyKey, env); err == nil {
		t.Fatal("migration must refuse plaintext fields")
	}
	out, n, err := MigrateConfigBytes(ctx, enc, nil, env)
	if err != nil || n != 0 || string(out) != string(enc) {
		t.Fatalf("already-migrated config must pass through unchanged: n=%d err=%v", n, err)
	}
	empty := []byte(`{"providers":[]}`)
	out, n, err = MigrateConfigBytes(ctx, empty, nil, env)
	if err != nil || n != 0 || string(out) != string(empty) {
		t.Fatalf("field-less config must pass through unchanged: n=%d err=%v", n, err)
	}
	if _, _, err := MigrateConfigBytes(ctx, old, []byte("short"), env); err == nil {
		t.Fatal("migration with a short legacy key must fail")
	}
	out, n, err = MigrateConfigBytes(ctx, old, legacyKey, env)
	if err != nil || n != 1 {
		t.Fatalf("nxs1 migration = n=%d err=%v", n, err)
	}
	plain, _, err := TransformConfigWith(ctx, Cipher{env: env}, out, false)
	if err != nil || !strings.Contains(string(plain), "migrate-canary") {
		t.Fatalf("migrated config must decrypt to the original plaintext, err=%v", err)
	}
	other := newFakeEnvelope("key-b")
	if _, _, err := MigrateConfigBytes(ctx, enc, nil, other); err == nil {
		t.Fatal("migration must fail when the nxs2 verification decrypt fails under another key")
	}
}

func TestIssue663CoreProveTransitConnectivity(t *testing.T) {
	ctx := context.Background()
	if err := ProveTransitConnectivity(ctx, newFakeEnvelope("key-a")); err != nil {
		t.Fatalf("fake canary round-trip must succeed: %v", err)
	}
	broken := newFakeEnvelope("key-a")
	broken.fail = errors.New("wrap refused")
	if err := ProveTransitConnectivity(ctx, broken); err == nil || !strings.Contains(err.Error(), "wrap") {
		t.Fatalf("wrap failure must fail the connectivity check, got %v", err)
	}
	if err := ProveTransitConnectivity(ctx, issue663CoreBadUnwrap{newFakeEnvelope("key-a")}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("canary mismatch must fail the connectivity check, got %v", err)
	}
}

func TestIssue663CoreLoadCipher(t *testing.T) {
	clearEnv := func(t *testing.T) {
		t.Helper()
		for _, name := range []string{
			"NEXAROUTE_KEY_PROVIDER", "NEXAROUTE_TRANSIT_ENABLED", "VAULT_ADDR",
			"VAULT_TOKEN", "VAULT_TOKEN_FILE", "NEXAROUTE_TRANSIT_MOUNT",
			"NEXAROUTE_TRANSIT_KEY", "VAULT_NAMESPACE",
			"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE",
			"NEXAROUTE_KEY_PROVIDER_ENV", "NEXAROUTE_KEY_PROVIDER_COMMAND",
			"NEXAROUTE_KEY_PROVIDER_REF",
		} {
			t.Setenv(name, "")
		}
	}
	t.Run("raw key from environment", func(t *testing.T) {
		clearEnv(t)
		rawKey := []byte("01234567890123456789012345678901")
		t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(rawKey))
		c, err := LoadCipher("unused-config.json")
		if err != nil || c.EnvelopeEnabled() || c.KeyRefHash() != "" {
			t.Fatalf("raw LoadCipher = enabled=%v hash=%q err=%v", c.EnvelopeEnabled(), c.KeyRefHash(), err)
		}
		sealed, err := c.SealField(context.Background(), "p", "api_key", []byte("v"))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := c.OpenField(context.Background(), "p", "api_key", sealed)
		if err != nil || string(opened) != "v" {
			t.Fatalf("raw LoadCipher round-trip = %q, %v", opened, err)
		}
	})
	t.Run("transit selection without a live vault", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
		t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
		t.Setenv("VAULT_ADDR", "http://127.0.0.1:9")
		t.Setenv("VAULT_TOKEN", "test-token")
		t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
		t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
		c, err := LoadCipher("unused-config.json")
		if err != nil || !c.EnvelopeEnabled() {
			t.Fatalf("transit LoadCipher = enabled=%v err=%v", c.EnvelopeEnabled(), err)
		}
		if got := c.KeyRefHash(); got != TransitKeyRefHash("", "transit", "nexaroute") {
			t.Fatalf("transit KeyRefHash = %q", got)
		}
	})
	t.Run("unknown provider fails closed", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NEXAROUTE_KEY_PROVIDER", "bogus-provider")
		if _, err := LoadCipher("unused-config.json"); err == nil {
			t.Fatal("LoadCipher with an unknown provider must fail")
		}
	})
}

func TestIssue663CoreDryRunInfo(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("key-a")
	legacyKey := []byte("01234567890123456789012345678901")
	raw := []byte(`{"providers":[{"id":"p","api_key":"dry-canary"}]}`)
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateDryRunInfo(ctx, old, legacyKey, nil); err == nil {
		t.Fatal("migration dry-run without a provider must fail")
	}
	n, err := MigrateDryRunInfo(ctx, old, legacyKey, env)
	if err != nil || n != 1 {
		t.Fatalf("migration dry-run = %d, %v", n, err)
	}
	if _, err := MigrateDryRunInfo(ctx, old, []byte("short"), env); err == nil {
		t.Fatal("migration dry-run with a short legacy key must fail")
	}
	if _, err := MigrateDryRunInfo(ctx, []byte(`{"providers":[{"id":"p","api_key":"nxs3:nope"}]}`), legacyKey, env); err == nil {
		t.Fatal("migration dry-run must refuse unknown envelopes")
	}
	if _, err := MigrateDryRunInfo(ctx, raw, legacyKey, env); err == nil {
		t.Fatal("migration dry-run must refuse plaintext fields")
	}
	enc, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	n, err = MigrateDryRunInfo(ctx, enc, nil, env)
	if err != nil || n != 0 {
		t.Fatalf("already-migrated dry-run = %d, %v", n, err)
	}
	n, err = MigrateDryRunInfo(ctx, []byte(`{"providers":[]}`), nil, env)
	if err != nil || n != 0 {
		t.Fatalf("field-less dry-run = %d, %v", n, err)
	}
	if _, _, err := RewrapDryRunInfo(ctx, enc, nil); err == nil {
		t.Fatal("rewrap dry-run without a provider must fail")
	}
	n, versions, err := RewrapDryRunInfo(ctx, enc, env)
	if err != nil || n != 1 || len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("rewrap dry-run = %d %v, %v", n, versions, err)
	}
	if _, _, err := RewrapDryRunInfo(ctx, old, env); err == nil {
		t.Fatal("rewrap dry-run must refuse nxs1 configs")
	}
	if _, _, err := RewrapDryRunInfo(ctx, []byte(`{"providers":[{"id":"p","api_key":"nxs3:nope"}]}`), env); err == nil {
		t.Fatal("rewrap dry-run must refuse unknown envelopes")
	}
	other := newFakeEnvelope("key-b")
	if _, _, err := RewrapDryRunInfo(ctx, enc, other); err == nil {
		t.Fatal("rewrap dry-run must fail when pre-verification decrypt fails under another key")
	}
}

func TestIssue663CoreVaultTransitProviderClose(t *testing.T) {
	p, err := NewVaultTransitProvider("http://127.0.0.1:9", "test-token", "", "transit", "nexaroute", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("provider Close must succeed: %v", err)
	}
	if got := p.KeyRefHash(); got != TransitKeyRefHash("", "transit", "nexaroute") {
		t.Fatalf("provider KeyRefHash = %q", got)
	}
}

func TestIssue663CoreValidateTransitReference(t *testing.T) {
	cases := []struct {
		namespace, mount, key string
		wantErr               bool
	}{
		{"", "transit", "k1", false},
		{"ns-1", "a/b", "k_1", false},
		{"", "", "k1", true},
		{"", "/transit", "k1", true},
		{"", "transit/", "k1", true},
		{"", "a//b", "k1", true},
		{"", "a/./b", "k1", true},
		{"", "transit", "", true},
		{"", "transit", "bad key!", true},
		{"bad//ns", "transit", "k1", true},
	}
	for _, tc := range cases {
		if err := ValidateTransitReference(tc.namespace, tc.mount, tc.key); (err != nil) != tc.wantErr {
			t.Errorf("ValidateTransitReference(%q,%q,%q) err=%v wantErr=%v", tc.namespace, tc.mount, tc.key, err, tc.wantErr)
		}
	}
	if err := validateTransitPath("a/b", false); err == nil {
		t.Error("single-segment path must reject a multi-segment value")
	}
	if err := validateTransitPath(strings.Repeat("a", 256), true); err == nil {
		t.Error("path over 255 characters must be rejected")
	}
}
