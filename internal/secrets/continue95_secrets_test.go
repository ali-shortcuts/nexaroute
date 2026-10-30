package secrets

// Coverage wave for issue #663: error paths in envelope.go, migration.go,
// secrets.go, vault_transit.go and key_provider.go. All keys and tokens
// below are synthetic test fixtures (deterministic byte patterns), never
// real credentials. Helper and test names use the Continue95Secrets
// prefix so they cannot collide with the sibling PR's TestIssue663 names.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Continue95SecretsSyntheticKey returns a deterministic 32-byte fixture key.
func Continue95SecretsSyntheticKey(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

// Continue95SecretsProviderConfig builds a minimal config document with
// one provider secret field.
func Continue95SecretsProviderConfig(value string) []byte {
	v, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return []byte(`{"providers":[{"id":"prov-synth","api_key":` + string(v) + `}]}`)
}

// Continue95SecretsMutateEnvelope decodes a valid nxs2 envelope, applies
// mutate to its inner JSON object, and re-encodes it.
func Continue95SecretsMutateEnvelope(t *testing.T, valid string, mutate func(map[string]any)) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(valid, EnvelopePrefix))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	mutate(obj)
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return EnvelopePrefix + base64.RawURLEncoding.EncodeToString(data)
}

// Continue95SecretsRawEnvelope wraps arbitrary inner JSON as an nxs2 envelope.
func Continue95SecretsRawEnvelope(t *testing.T, inner string) string {
	t.Helper()
	return EnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(inner))
}

// Continue95SecretsStubProvider is a controllable EnvelopeKeyProvider
// delegating to a fakeEnvelope unless an override field is set.
type Continue95SecretsStubProvider struct {
	base          *fakeEnvelope
	wrapErr       error
	wrapEmpty     bool
	wrapVersion   *int
	wrapResult    string
	wrapHas       bool
	unwrapErr     error
	unwrapDEK     []byte
	unwrapVersion *int
	rewrapErr     error
	rewrapResult  string
	rewrapHas     bool
}

func (p *Continue95SecretsStubProvider) KeyRefHash() string { return p.base.KeyRefHash() }

func (p *Continue95SecretsStubProvider) WrapDEK(ctx context.Context, dek []byte) (string, int, error) {
	if p.wrapErr != nil {
		return "", 0, p.wrapErr
	}
	if p.wrapEmpty {
		return "", 1, nil
	}
	if p.wrapHas {
		return p.wrapResult, 1, nil
	}
	wrapped, version, err := p.base.WrapDEK(ctx, dek)
	if err != nil {
		return "", 0, err
	}
	if p.wrapVersion != nil {
		version = *p.wrapVersion
	}
	return wrapped, version, nil
}

func (p *Continue95SecretsStubProvider) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	if p.unwrapErr != nil {
		return nil, 0, p.unwrapErr
	}
	dek, version, err := p.base.UnwrapDEK(ctx, wrapped)
	if err != nil {
		return nil, 0, err
	}
	if p.unwrapDEK != nil {
		dek = append([]byte(nil), p.unwrapDEK...)
	}
	if p.unwrapVersion != nil {
		version = *p.unwrapVersion
	}
	return dek, version, nil
}

func (p *Continue95SecretsStubProvider) RewrapDEK(ctx context.Context, wrapped string) (string, int, error) {
	if p.rewrapErr != nil {
		return "", 0, p.rewrapErr
	}
	if p.rewrapHas {
		return p.rewrapResult, 1, nil
	}
	return p.base.RewrapDEK(ctx, wrapped)
}

func (p *Continue95SecretsStubProvider) Close() error { return nil }

func Continue95SecretsIntPtr(v int) *int { return &v }

func TestContinue95SecretsParseTransitCiphertextVersions(t *testing.T) {
	valid := "vault:v1:" + base64.StdEncoding.EncodeToString([]byte("synthetic-body"))
	if v, err := ParseTransitCiphertextVersion(valid); err != nil || v != 1 {
		t.Fatalf("valid: version=%d err=%v", v, err)
	}
	if v, err := ParseTransitCiphertextVersion("vault:v12:" + base64.StdEncoding.EncodeToString([]byte("synthetic-body"))); err != nil || v != 12 {
		t.Fatalf("v12: version=%d err=%v", v, err)
	}
	for _, bad := range []string{
		"", "vault:v1", "vault:v1:a:b", "other:v1:AAAA", "vault:1:AAAA",
		"vault:v:AAAA", "vault:v01:AAAA", "vault:v0:AAAA", "vault:v-1:AAAA",
		"vault:vx:AAAA", "vault:v1:", "vault:v1:!!!!",
		"vault:v1:" + base64.StdEncoding.EncodeToString([]byte("x")) + "=",
		"vault:v999999999999999999999:AAAA",
	} {
		if _, err := ParseTransitCiphertextVersion(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
	long := "vault:v1:" + strings.Repeat("A", 4097)
	if _, err := ParseTransitCiphertextVersion(long); err == nil {
		t.Fatal("expected error for over-long body")
	}
}

func TestContinue95SecretsValidateTransitReference(t *testing.T) {
	if err := ValidateTransitReference("ns-a", "transit", "synth-key_1.2"); err != nil {
		t.Fatalf("valid reference rejected: %v", err)
	}
	if err := ValidateTransitReference("", "transit", "synth-key"); err != nil {
		t.Fatalf("empty namespace rejected: %v", err)
	}
	for _, tc := range []struct{ ns, mount, key string }{
		{"", "", "k"}, {"", "/transit", "k"}, {"", "transit/", "k"},
		{"", "a//b", "k"}, {"", "a/./b", "k"}, {"", "a/../b", "k"},
		{"", "a b", "k"}, {"", strings.Repeat("a", 256), "k"},
		{"", "transit", ""}, {"", "transit", strings.Repeat("k", 129)},
		{"", "transit", "bad key!"}, {"bad ns!", "transit", "k"},
		{"a//b", "transit", "k"},
	} {
		if err := ValidateTransitReference(tc.ns, tc.mount, tc.key); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
	canonical := CanonicalTransitKeyRef("ns-a", "transit", "synth-key")
	if canonical != "vault-transit|namespace=ns-a|mount=transit|key=synth-key" {
		t.Fatalf("canonical=%q", canonical)
	}
	sum := sha256.Sum256([]byte(canonical))
	if got := TransitKeyRefHash("ns-a", "transit", "synth-key"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash=%q want %q", got, hex.EncodeToString(sum[:]))
	}
	if got := EnvelopeAAD("ref-synth", "nonce-synth", "owner-synth", "field-synth"); len(got) == 0 || !strings.Contains(string(got), "nxs2-binding-v1") {
		t.Fatal("EnvelopeAAD missing binding marker")
	}
	if string(EnvelopeAAD("r", "n", "o1", "f")) == string(EnvelopeAAD("r", "n", "o2", "f")) {
		t.Fatal("EnvelopeAAD must bind the owner")
	}
}

func TestContinue95SecretsCipherSealOpenErrors(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("continue95-a")
	envCipher := Cipher{env: env}
	if _, err := envCipher.Seal([]byte("aad"), []byte("synthetic")); err == nil {
		t.Fatal("envelope cipher Seal must reject the aad API")
	}
	if _, err := envCipher.Open([]byte("aad"), "nxs2:whatever"); err == nil {
		t.Fatal("envelope cipher Open must reject the aad API")
	}
	badRaw := Cipher{raw: []byte("too-short")}
	if _, err := badRaw.Seal([]byte("aad"), []byte("synthetic")); err == nil {
		t.Fatal("short raw key must fail Seal")
	}
	if _, err := badRaw.SealField(ctx, "o", "f", []byte("synthetic")); err == nil {
		t.Fatal("short raw key must fail SealField")
	}
	rawCipher := Cipher{raw: Continue95SecretsSyntheticKey(7)}
	nxs2, err := envCipher.SealField(ctx, "prov-synth", "api_key", []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawCipher.OpenField(ctx, "prov-synth", "api_key", nxs2); err == nil {
		t.Fatal("nxs2 under a raw cipher must fail")
	}
	if _, err := envCipher.OpenField(ctx, "prov-synth", "api_key", "nxs9:synthetic"); err == nil {
		t.Fatal("unknown envelope under envelope cipher must fail")
	}
	if _, err := envCipher.OpenField(ctx, "prov-synth", "api_key", "synthetic-plaintext"); err == nil {
		t.Fatal("plaintext under envelope cipher must fail")
	}
	if _, err := rawCipher.OpenField(ctx, "prov-synth", "api_key", "nxs9:synthetic"); err == nil {
		t.Fatal("unknown envelope under raw cipher must fail")
	}
	nxs1, err := rawCipher.SealField(ctx, "prov-synth", "api_key", []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envCipher.OpenField(ctx, "prov-synth", "api_key", nxs1); err == nil {
		t.Fatal("nxs1 under envelope cipher must fail")
	}
	if got, err := rawCipher.OpenField(ctx, "prov-synth", "api_key", nxs1); err != nil || string(got) != "synthetic-secret" {
		t.Fatalf("raw roundtrip: got=%q err=%v", got, err)
	}
	if _, err := envCipher.OpenField(ctx, "prov-synth", "api_key", ""); err == nil {
		t.Fatal("empty value under envelope cipher must fail")
	}
	if IsEnvelope("synthetic-plaintext") || !IsEnvelope(nxs1) || !IsEnvelope(nxs2) {
		t.Fatal("IsEnvelope classification wrong")
	}
	if HasEnvelopes([]byte("nothing here")) || !HasEnvelopes([]byte("prefix "+nxs2)) {
		t.Fatal("HasEnvelopes classification wrong")
	}
}

func TestContinue95SecretsParseEnvelopeStrictness(t *testing.T) {
	env := newFakeEnvelope("continue95-b")
	valid := sealField(t, env, "prov-synth", "api_key", "synthetic-secret")
	mutations := []struct {
		name     string
		envelope string
	}{
		{"no prefix", strings.TrimPrefix(valid, EnvelopePrefix)},
		{"bad outer base64", EnvelopePrefix + "!!!not-base64!!!"},
		{"padded outer base64", EnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte("{}")) + "="},
		{"not an object", Continue95SecretsRawEnvelope(t, `[1,2,3]`)},
		{"malformed json", Continue95SecretsRawEnvelope(t, `{"envelope_version":`)},
		{"trailing data", Continue95SecretsRawEnvelope(t, `{"envelope_version":2} garbage`)},
		{"duplicate field", Continue95SecretsRawEnvelope(t, `{"envelope_version":2,"envelope_version":2,"provider":"vault-transit","key_ref_sha256":"`+env.KeyRefHash()+`","transit_key_version":1,"wrapped_dek":"vault:v1:QUJD","data_nonce_b64url":"AAAAAAAAAAAAAAAA","data_ciphertext_b64url":"AAAAAAAAAAAAAAAAAAAAAA"}`)},
		{"unknown field", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["extra_synth"] = "x" })},
		{"missing field", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { delete(m, "wrapped_dek") })},
		{"version one", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["envelope_version"] = float64(1) })},
		{"version wrong type", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["envelope_version"] = "two" })},
		{"provider wrong", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["provider"] = "other" })},
		{"keyref uppercase", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["key_ref_sha256"] = strings.ToUpper(env.KeyRefHash()) })},
		{"keyref short", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["key_ref_sha256"] = "abcd" })},
		{"key version zero", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["transit_key_version"] = float64(0) })},
		{"wrapped bad shape", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["wrapped_dek"] = "not-a-transit-ciphertext" })},
		{"nonce short", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["data_nonce_b64url"] = base64.RawURLEncoding.EncodeToString([]byte("short")) })},
		{"nonce bad encoding", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["data_nonce_b64url"] = "!!!" })},
		{"ciphertext short", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) {
			m["data_ciphertext_b64url"] = base64.RawURLEncoding.EncodeToString([]byte("tiny"))
		})},
		{"ciphertext bad encoding", Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) { m["data_ciphertext_b64url"] = "!!!" })},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseEnvelope(tc.envelope); err == nil {
				t.Fatalf("parseEnvelope accepted %s", tc.name)
			}
			// A malformed nxs2 value must classify as unknown, never plaintext.
			if strings.HasPrefix(tc.envelope, EnvelopePrefix) {
				if got := ClassifyValue(tc.envelope); got != ClassUnknown {
					t.Fatalf("ClassifyValue=%v want ClassUnknown for %s", got, tc.name)
				}
			}
		})
	}
	// Wrapped-version mismatch against the recorded version.
	mismatch := Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) {
		m["transit_key_version"] = float64(2)
	})
	if _, err := parseEnvelope(mismatch); err == nil {
		t.Fatal("wrapped version mismatch must fail parse")
	}
	if _, err := decodeEnvelopeField("!!!"); err == nil {
		t.Fatal("decodeEnvelopeField must reject bad encoding")
	}
	if _, err := decodeEnvelopeField(base64.RawURLEncoding.EncodeToString([]byte("x")) + "="); err == nil {
		t.Fatal("decodeEnvelopeField must reject padded encoding")
	}
	// decodeStrictObject: malformed value and non-object start.
	if _, err := decodeStrictObject([]byte(`{"a": }`)); err == nil {
		t.Fatal("malformed value must fail")
	}
	if _, err := decodeStrictObject([]byte(`"just a string"`)); err == nil {
		t.Fatal("non-object must fail")
	}
	if _, err := decodeStrictObject([]byte(`{"a":1`)); err == nil {
		t.Fatal("unterminated object must fail")
	}
}

func TestContinue95SecretsOpenEnvelopeFailures(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("continue95-c")
	valid := sealField(t, env, "prov-synth", "api_key", "synthetic-secret")
	other := newFakeEnvelope("continue95-other")
	if _, err := openEnvelope(ctx, other, "prov-synth", "api_key", valid); err == nil {
		t.Fatal("key-reference mismatch must fail")
	}
	if _, err := openEnvelope(ctx, env, "other-owner", "api_key", valid); err == nil {
		t.Fatal("wrong owner must fail authentication")
	}
	if _, err := openEnvelope(ctx, env, "prov-synth", "other_field", valid); err == nil {
		t.Fatal("wrong field must fail authentication")
	}
	tampered := Continue95SecretsMutateEnvelope(t, valid, func(m map[string]any) {
		ct, _ := base64.RawURLEncoding.DecodeString(m["data_ciphertext_b64url"].(string))
		ct[0] ^= 0xff
		m["data_ciphertext_b64url"] = base64.RawURLEncoding.EncodeToString(ct)
	})
	if _, err := openEnvelope(ctx, env, "prov-synth", "api_key", tampered); err == nil {
		t.Fatal("tampered ciphertext must fail authentication")
	}
	revoked := newFakeEnvelope("continue95-d")
	revokedText := sealField(t, revoked, "prov-synth", "api_key", "synthetic-secret")
	revoked.revokeAll()
	if _, err := openEnvelope(ctx, revoked, "prov-synth", "api_key", revokedText); err == nil {
		t.Fatal("revoked wrapped key must fail unwrap")
	}
	stub := &Continue95SecretsStubProvider{base: env, unwrapVersion: Continue95SecretsIntPtr(99)}
	if _, err := openEnvelope(ctx, stub, "prov-synth", "api_key", valid); err == nil {
		t.Fatal("unwrap version mismatch must fail")
	}
	shortStub := &Continue95SecretsStubProvider{base: env, unwrapDEK: make([]byte, 16)}
	if _, err := openEnvelope(ctx, shortStub, "prov-synth", "api_key", valid); err == nil {
		t.Fatal("short unwrapped DEK must fail")
	}
	failStub := &Continue95SecretsStubProvider{base: env, unwrapErr: errors.New("synthetic unwrap failure")}
	if _, err := openEnvelope(ctx, failStub, "prov-synth", "api_key", valid); err == nil {
		t.Fatal("unwrap error must propagate")
	}
	// Seal failures.
	wrapFail := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-e"), wrapErr: errors.New("synthetic wrap failure")}
	if _, err := sealEnvelope(ctx, wrapFail, "prov-synth", "api_key", []byte("synthetic")); err == nil {
		t.Fatal("wrap failure must fail seal")
	}
	emptyStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-f"), wrapEmpty: true}
	if _, err := sealEnvelope(ctx, emptyStub, "prov-synth", "api_key", []byte("synthetic")); err == nil {
		t.Fatal("empty wrapped DEK must fail seal")
	}
	mismatchStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-g"), wrapVersion: Continue95SecretsIntPtr(42)}
	if _, err := sealEnvelope(ctx, mismatchStub, "prov-synth", "api_key", []byte("synthetic")); err == nil {
		t.Fatal("wrap version mismatch must fail seal")
	}
	invalidStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-h")}
	invalidStub.base.fail = errors.New("synthetic base failure")
	if _, err := sealEnvelope(ctx, invalidStub, "prov-synth", "api_key", []byte("synthetic")); err == nil {
		t.Fatal("base wrap failure must fail seal")
	}
}

func TestContinue95SecretsTransformConfigWithGuards(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("continue95-i")
	raw := Continue95SecretsSyntheticKey(11)
	rawCipher := Cipher{raw: raw}
	nxs2 := sealField(t, env, "prov-synth", "api_key", "synthetic-secret")
	nxs1doc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-plaintext"), raw, true)
	if err != nil {
		t.Fatal(err)
	}
	var nxs1 string
	{
		var doc struct {
			Providers []struct {
				APIKey string `json:"api_key"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(nxs1doc, &doc); err != nil {
			t.Fatal(err)
		}
		nxs1 = doc.Providers[0].APIKey
	}
	mixed := []byte(`{"providers":[{"id":"prov-synth","api_key":` + mustJSON(t, nxs1) + `},{"id":"prov-two","api_key":` + mustJSON(t, nxs2) + `}]}`)
	if _, _, err := TransformConfigWith(ctx, Cipher{env: env}, mixed, false); err == nil {
		t.Fatal("mixed nxs1+nxs2 must fail")
	}
	unknown := Continue95SecretsProviderConfig("nxs9:synthetic-unknown")
	if _, _, err := TransformConfigWith(ctx, rawCipher, unknown, false); err == nil {
		t.Fatal("unknown envelope must fail transform")
	}
	if _, _, err := TransformConfigWith(ctx, Cipher{env: env}, Continue95SecretsProviderConfig(nxs1), false); err == nil {
		t.Fatal("nxs1 in Transit mode must fail transform")
	}
	if _, _, err := TransformConfigWith(ctx, rawCipher, Continue95SecretsProviderConfig(nxs2), false); err == nil {
		t.Fatal("nxs2 under raw cipher must fail transform")
	}
	if _, _, err := TransformConfigWith(ctx, Cipher{env: env}, Continue95SecretsProviderConfig("synthetic-plaintext"), false); err == nil {
		t.Fatal("plaintext decrypt in Transit mode must fail transform")
	}
	// Encrypt already sealed: passthrough, zero new fields.
	if out, n, err := TransformConfigWith(ctx, Cipher{env: env}, Continue95SecretsProviderConfig(nxs2), true); err != nil || n != 0 || !strings.Contains(string(out), nxs2) {
		t.Fatalf("already-sealed passthrough: n=%d err=%v", n, err)
	}
	// Decrypt with the wrong field binding fails.
	if _, _, err := TransformConfigWith(ctx, rawCipher, Continue95SecretsProviderConfig(nxs1), false); err != nil {
		t.Fatal(err)
	}
	wrongOwner := []byte(`{"providers":[{"id":"other-owner","api_key":` + mustJSON(t, nxs1) + `}]}`)
	if _, _, err := TransformConfigWith(ctx, rawCipher, wrongOwner, false); err == nil {
		t.Fatal("wrong-owner nxs1 decrypt must fail")
	}
	if _, _, err := TransformConfigWith(ctx, rawCipher, []byte(`not json`), false); err == nil {
		t.Fatal("bad JSON must fail transform")
	}
	// Raw TransformConfig guards.
	if _, _, err := TransformConfig(Continue95SecretsProviderConfig("x"), []byte("short"), true); err == nil {
		t.Fatal("short raw key must fail TransformConfig")
	}
	if _, _, err := TransformConfig(Continue95SecretsProviderConfig(nxs2), raw, true); err == nil {
		t.Fatal("encrypting nxs2 with raw TransformConfig must fail")
	}
	if _, _, err := TransformConfig(Continue95SecretsProviderConfig(nxs2), raw, false); err == nil {
		t.Fatal("decrypting nxs2 with raw TransformConfig must fail")
	}
	// Classify / map / substitute guards.
	if _, err := ClassifyConfig([]byte(`not json`)); err == nil {
		t.Fatal("ClassifyConfig bad JSON must fail")
	}
	if _, err := SecretFieldMap([]byte(`not json`)); err == nil {
		t.Fatal("SecretFieldMap bad JSON must fail")
	}
	m, err := SecretFieldMap(Continue95SecretsProviderConfig("synthetic-value"))
	if err != nil || m["prov-synth:api_key"] != "synthetic-value" {
		t.Fatalf("SecretFieldMap=%v err=%v", m, err)
	}
	if _, err := SubstituteSecretFields(Continue95SecretsProviderConfig("v"), func(_, _, _ string) (string, error) {
		return "", errors.New("synthetic substitute failure")
	}); err == nil {
		t.Fatal("substitute error must propagate")
	}
	if cls, err := ClassifyConfig(Continue95SecretsProviderConfig("")); err != nil || cls.NXS1+cls.NXS2+cls.Plaintext+cls.Unknown != 0 {
		t.Fatalf("empty field must not count: %+v err=%v", cls, err)
	}
	if got := ClassifyValue(""); got != ClassEmpty {
		t.Fatalf("empty class=%v", got)
	}
	if got := ClassifyValue("synthetic-plaintext"); got != ClassPlaintext {
		t.Fatalf("plaintext class=%v", got)
	}
	if got := ClassifyValue(nxs1); got != ClassNXS1 {
		t.Fatalf("nxs1 class=%v", got)
	}
	if got := ClassifyValue(nxs2); got != ClassNXS2 {
		t.Fatalf("nxs2 class=%v", got)
	}
	if got := ClassifyValue("nxs3:synthetic"); got != ClassUnknown {
		t.Fatalf("nxs3 class=%v", got)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContinue95SecretsRewrapAndMigrateBytesGuards(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("continue95-j")
	raw := Continue95SecretsSyntheticKey(13)
	nxs2 := sealField(t, env, "prov-synth", "api_key", "synthetic-secret")
	doc := Continue95SecretsProviderConfig(nxs2)
	if _, _, _, err := RewrapConfigBytes(ctx, nil, doc); err == nil {
		t.Fatal("rewrap with nil provider must fail")
	}
	if _, _, err := MigrateConfigBytes(ctx, doc, raw, nil); err == nil {
		t.Fatal("migrate with nil provider must fail")
	}
	if _, _, _, err := RewrapConfigBytes(ctx, env, Continue95SecretsProviderConfig("nxs9:x")); err == nil {
		t.Fatal("rewrap unknown must fail")
	}
	nxs1doc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-plaintext"), raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := RewrapConfigBytes(ctx, env, nxs1doc); err == nil {
		t.Fatal("rewrap nxs1 must fail")
	}
	if _, _, _, err := RewrapConfigBytes(ctx, env, Continue95SecretsProviderConfig("synthetic-plaintext")); err == nil {
		t.Fatal("rewrap plaintext must fail")
	}
	if _, _, err := MigrateConfigBytes(ctx, Continue95SecretsProviderConfig("nxs9:x"), raw, env); err == nil {
		t.Fatal("migrate unknown must fail")
	}
	if _, _, err := MigrateConfigBytes(ctx, Continue95SecretsProviderConfig("synthetic-plaintext"), raw, env); err == nil {
		t.Fatal("migrate plaintext must fail")
	}
	if out, n, err := MigrateConfigBytes(ctx, doc, raw, env); err != nil || n != 0 || string(out) != string(doc) {
		t.Fatalf("already-nxs2 migrate: n=%d err=%v", n, err)
	}
	if out, n, err := MigrateConfigBytes(ctx, Continue95SecretsProviderConfig("synthetic-plaintext-free"), raw, env); err == nil {
		_ = out
		_ = n
		t.Fatal("expected plaintext migrate failure above (guard)")
	}
	if _, _, err := MigrateConfigBytes(ctx, nxs1doc, []byte("short"), env); err == nil {
		t.Fatal("migrate with short legacy key must fail")
	}
	// Successful nxs1 -> nxs2 migration in memory, then decrypt-verify.
	migrated, n, err := MigrateConfigBytes(ctx, nxs1doc, raw, env)
	if err != nil || n != 1 {
		t.Fatalf("migrate nxs1: n=%d err=%v", n, err)
	}
	plain, _, err := TransformConfigWith(ctx, Cipher{env: env}, migrated, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), "synthetic-plaintext") {
		t.Fatalf("migrated doc does not decrypt: %s", plain)
	}
	// Mixed migrate fails.
	var nxs1val string
	{
		var d struct {
			Providers []struct {
				APIKey string `json:"api_key"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(nxs1doc, &d); err != nil {
			t.Fatal(err)
		}
		nxs1val = d.Providers[0].APIKey
	}
	mixed := []byte(`{"providers":[{"id":"prov-synth","api_key":` + mustJSON(t, nxs1val) + `},{"id":"prov-two","api_key":` + mustJSON(t, nxs2) + `}]}`)
	if _, _, err := MigrateConfigBytes(ctx, mixed, raw, env); err == nil {
		t.Fatal("mixed migrate must fail")
	}
	// Rewrap: rotate, rewrap once, then already-latest.
	env.rotate()
	out, n, already, err := RewrapConfigBytes(ctx, env, doc)
	if err != nil || n != 1 || already {
		t.Fatalf("rewrap rotated: n=%d already=%v err=%v", n, already, err)
	}
	if plain2, _, err := TransformConfigWith(ctx, Cipher{env: env}, out, false); err != nil || !strings.Contains(string(plain2), "synthetic-secret") {
		t.Fatalf("rewrapped doc decrypt: err=%v", err)
	}
	if _, n, already, err := RewrapConfigBytes(ctx, env, out); err != nil || n != 0 || !already {
		t.Fatalf("rewrap already-latest: n=%d already=%v err=%v", n, already, err)
	}
	// Rewrap pre-verification failure: revoke before rewrap.
	revoked := newFakeEnvelope("continue95-k")
	revokedDoc := Continue95SecretsProviderConfig(sealField(t, revoked, "prov-synth", "api_key", "synthetic-secret"))
	revoked.revokeAll()
	if _, _, _, err := RewrapConfigBytes(ctx, revoked, revokedDoc); err == nil {
		t.Fatal("rewrap pre-verification failure must fail")
	}
	// Rewrap with a provider returning an invalid rewrapped ciphertext.
	stub := &Continue95SecretsStubProvider{base: env, rewrapHas: true, rewrapResult: "not-a-transit-ciphertext"}
	rotEnv := newFakeEnvelope("continue95-l")
	rotDoc := Continue95SecretsProviderConfig(sealField(t, rotEnv, "prov-synth", "api_key", "synthetic-secret"))
	rotEnv.rotate()
	_ = stub
	if _, _, _, err := RewrapConfigBytes(ctx, &Continue95SecretsStubProvider{base: rotEnv, rewrapHas: true, rewrapResult: "not-a-transit-ciphertext"}, rotDoc); err == nil {
		t.Fatal("invalid rewrap result must fail")
	}
	// Rewrap key-reference mismatch inside the visit.
	other := newFakeEnvelope("continue95-m")
	if _, _, _, err := RewrapConfigBytes(ctx, other, doc); err == nil {
		t.Fatal("rewrap keyref mismatch must fail")
	}
}

func TestContinue95SecretsFileFlowsAndDryRuns(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("continue95-n")
	raw := Continue95SecretsSyntheticKey(17)
	if _, err := MigrateToEnvelopeFile(ctx, filepath.Join(t.TempDir(), "missing.json"), raw, nil); err == nil {
		t.Fatal("file migrate nil provider must fail")
	}
	if _, err := MigrateToEnvelopeFile(ctx, filepath.Join(t.TempDir(), "missing.json"), raw, env); err == nil {
		t.Fatal("file migrate missing file must fail")
	}
	if _, err := RewrapConfigFile(ctx, filepath.Join(t.TempDir(), "missing.json"), nil); err == nil {
		t.Fatal("file rewrap nil provider must fail")
	}
	if _, err := RewrapConfigFile(ctx, filepath.Join(t.TempDir(), "missing.json"), env); err == nil {
		t.Fatal("file rewrap missing file must fail")
	}
	dir := t.TempDir()
	// Already-nxs2: migrate returns 0 and writes nothing (no backup).
	nxs2 := sealField(t, env, "prov-synth", "api_key", "synthetic-secret")
	alreadyPath := filepath.Join(dir, "already.json")
	if err := os.WriteFile(alreadyPath, Continue95SecretsProviderConfig(nxs2), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := MigrateToEnvelopeFile(ctx, alreadyPath, raw, env); err != nil || res.Migrated != 0 || res.BackupPath != "" {
		t.Fatalf("already migrate: %+v err=%v", res, err)
	}
	// nxs1 file migration: backup is verbatim, migrated file decrypts.
	nxs1doc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-plaintext"), raw, true)
	if err != nil {
		t.Fatal(err)
	}
	migPath := filepath.Join(dir, "mig.json")
	if err := os.WriteFile(migPath, nxs1doc, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateToEnvelopeFile(ctx, migPath, raw, env)
	if err != nil || res.Migrated != 1 || res.BackupPath == "" {
		t.Fatalf("file migrate: %+v err=%v", res, err)
	}
	backup, err := os.ReadFile(res.BackupPath)
	if err != nil || string(backup) != string(nxs1doc) {
		t.Fatalf("backup must be verbatim pre-migration bytes: err=%v", err)
	}
	sum := sha256.Sum256(nxs1doc)
	if res.BackupSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("backup sha=%q", res.BackupSHA256)
	}
	after, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatal(err)
	}
	if plain, _, err := TransformConfigWith(ctx, Cipher{env: env}, after, false); err != nil || !strings.Contains(string(plain), "synthetic-plaintext") {
		t.Fatalf("migrated file decrypt: err=%v", err)
	}
	// Rewrap file: already-latest, then rotated.
	if rres, err := RewrapConfigFile(ctx, migPath, env); err != nil || !rres.AlreadyLatest || rres.BackupPath != "" {
		t.Fatalf("rewrap already-latest file: %+v err=%v", rres, err)
	}
	env.rotate()
	if rres, err := RewrapConfigFile(ctx, migPath, env); err != nil || rres.Rewrapped != 1 || rres.BackupPath == "" {
		t.Fatalf("rewrap file rotated: %+v err=%v", rres, err)
	}
	// Dry runs.
	if _, err := MigrateDryRunInfo(ctx, Continue95SecretsProviderConfig("x"), raw, nil); err == nil {
		t.Fatal("dry-run migrate nil must fail")
	}
	if n, err := MigrateDryRunInfo(ctx, nxs1doc, raw, env); err != nil || n != 1 {
		t.Fatalf("dry-run migrate nxs1: n=%d err=%v", n, err)
	}
	if n, err := MigrateDryRunInfo(ctx, Continue95SecretsProviderConfig(nxs2), raw, env); err != nil || n != 0 {
		t.Fatalf("dry-run migrate nxs2: n=%d err=%v", n, err)
	}
	if _, err := MigrateDryRunInfo(ctx, Continue95SecretsProviderConfig("synthetic-plaintext"), raw, env); err == nil {
		t.Fatal("dry-run migrate plaintext must fail")
	}
	if _, err := MigrateDryRunInfo(ctx, nxs1doc, []byte("short"), env); err == nil {
		t.Fatal("dry-run migrate short legacy must fail")
	}
	if _, _, err := RewrapDryRunInfo(ctx, Continue95SecretsProviderConfig("x"), nil); err == nil {
		t.Fatal("dry-run rewrap nil must fail")
	}
	if n, versions, err := RewrapDryRunInfo(ctx, Continue95SecretsProviderConfig(nxs2), env); err != nil || n != 1 || len(versions) != 1 {
		t.Fatalf("dry-run rewrap: n=%d versions=%v err=%v", n, versions, err)
	}
	if _, _, err := RewrapDryRunInfo(ctx, nxs1doc, env); err == nil {
		t.Fatal("dry-run rewrap nxs1 must fail")
	}
	// Distinct versions + disagreeing key references.
	vEnv := newFakeEnvelope("continue95-o")
	first := sealField(t, vEnv, "prov-synth", "api_key", "synthetic-one")
	vEnv.rotate()
	second := sealField(t, vEnv, "prov-two", "api_key", "synthetic-two")
	twoDoc := []byte(`{"providers":[{"id":"prov-synth","api_key":` + mustJSON(t, first) + `},{"id":"prov-two","api_key":` + mustJSON(t, second) + `}]}`)
	versions, err := DistinctTransitKeyVersions(twoDoc)
	if err != nil || len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("distinct versions=%v err=%v", versions, err)
	}
	if _, err := DistinctTransitKeyVersions([]byte(`not json`)); err == nil {
		t.Fatal("distinct versions bad JSON must fail")
	}
	if _, err := DistinctTransitKeyVersions(Continue95SecretsProviderConfig("nxs9:x")); err == nil {
		t.Fatal("distinct versions unknown envelope must fail")
	}
	otherEnv := newFakeEnvelope("continue95-p")
	disagree := []byte(`{"providers":[{"id":"prov-synth","api_key":` + mustJSON(t, first) + `},{"id":"prov-two","api_key":` + mustJSON(t, sealField(t, otherEnv, "prov-two", "api_key", "synthetic-two")) + `}]}`)
	if _, _, err := TransitEnvelopeInfo(disagree); err == nil {
		t.Fatal("disagreeing key references must fail TransitEnvelopeInfo")
	}
	if ref, vers, err := TransitEnvelopeInfo(twoDoc); err != nil || ref != vEnv.KeyRefHash() || len(vers) != 2 {
		t.Fatalf("TransitEnvelopeInfo: ref=%q vers=%v err=%v", ref, vers, err)
	}
	if _, _, err := TransitEnvelopeInfo([]byte(`not json`)); err == nil {
		t.Fatal("TransitEnvelopeInfo bad JSON must fail")
	}
}

func TestContinue95SecretsBackupAndConnectivity(t *testing.T) {
	ctx := context.Background()
	env := newFakeEnvelope("continue95-q")
	raw := Continue95SecretsSyntheticKey(19)
	if _, err := EncryptedBackupWith(Cipher{env: env}, []byte("synthetic-config")); err == nil {
		t.Fatal("Transit mode must refuse sealed backups")
	}
	if _, err := DecryptBackupWith(Cipher{env: env}, []byte("synthetic")); err == nil {
		t.Fatal("Transit mode must refuse sealed backup decryption")
	}
	blob, err := EncryptedBackupWith(Cipher{raw: raw}, []byte("synthetic-config-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if back, err := DecryptBackupWith(Cipher{raw: raw}, blob); err != nil || string(back) != "synthetic-config-bytes" {
		t.Fatalf("backup roundtrip: %q err=%v", back, err)
	}
	if _, err := DecryptBackupWith(Cipher{raw: raw}, []byte("nxs1:corrupt")); err == nil {
		t.Fatal("corrupt backup must fail")
	}
	if err := ProveTransitConnectivity(ctx, env); err != nil {
		t.Fatalf("connectivity proof failed: %v", err)
	}
	failEnv := newFakeEnvelope("continue95-r")
	failEnv.fail = errors.New("synthetic wrap failure")
	if err := ProveTransitConnectivity(ctx, failEnv); err == nil {
		t.Fatal("connectivity wrap failure must fail")
	}
	mismatchStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-s"), unwrapDEK: Continue95SecretsSyntheticKey(99)}
	if err := ProveTransitConnectivity(ctx, mismatchStub); err == nil {
		t.Fatal("connectivity canary mismatch must fail")
	}
	unwrapFail := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-t"), unwrapErr: errors.New("synthetic unwrap failure")}
	// Prove wraps through the base, then unwrap fails via the stub.
	if err := ProveTransitConnectivity(ctx, unwrapFail); err == nil {
		t.Fatal("connectivity unwrap failure must fail")
	}
}

func TestContinue95SecretsRawSealOpenAndKeyFlows(t *testing.T) {
	raw := Continue95SecretsSyntheticKey(23)
	aad := AAD("prov-synth", "api_key")
	sealed, err := seal(raw, aad, []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, Prefix) {
		t.Fatalf("nxs1 prefix missing: %q", sealed)
	}
	if plain, err := Open(raw, aad, sealed); err != nil || string(plain) != "synthetic-secret" {
		t.Fatalf("nxs1 roundtrip: %q err=%v", plain, err)
	}
	for _, bad := range []string{
		"nxs1:short", "nxs1:badkey:AAAA:AAAA", sealed + "extra",
		strings.Replace(sealed, "nxs1:", "nxs2:", 1),
	} {
		if _, err := Open(raw, aad, bad); err == nil {
			t.Fatalf("Open accepted %q", bad)
		}
	}
	if _, err := Open(raw, AAD("other", "api_key"), sealed); err == nil {
		t.Fatal("wrong AAD must fail Open")
	}
	if _, err := Open(Continue95SecretsSyntheticKey(29), aad, sealed); err == nil {
		t.Fatal("wrong key must fail Open")
	}
	if _, err := seal([]byte("short"), aad, []byte("x")); err == nil {
		t.Fatal("seal with short key must fail")
	}
	if _, err := Open([]byte("short"), aad, sealed); err == nil {
		t.Fatal("Open with short key must fail")
	}
	// Key() flows with synthetic environment keys only.
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(raw))
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	got, id, err := Key(filepath.Join(t.TempDir(), "config.json"))
	if err != nil || len(got) != 32 || id == "" {
		t.Fatalf("Key env: len=%d id=%q err=%v", len(got), id, err)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "!!!not-base64!!!")
	if _, _, err := Key(filepath.Join(t.TempDir(), "config.json")); err == nil {
		t.Fatal("bad base64 master key must fail")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("short")))
	if _, _, err := Key(filepath.Join(t.TempDir(), "config.json")); err == nil {
		t.Fatal("short base64 master key must fail")
	}
	// Explicit key file: wrong mode, wrong length, missing.
	dir := t.TempDir()
	badMode := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(badMode, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", badMode)
	if _, _, err := Key(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("key file with mode 0644 must fail")
	}
	shortFile := filepath.Join(dir, "short.key")
	if err := os.WriteFile(shortFile, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", shortFile)
	if _, _, err := Key(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("short key file must fail")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", filepath.Join(dir, "missing.key"))
	if _, _, err := Key(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("missing explicit key file must fail")
	}
	// Transit provider cannot supply a raw key.
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	if _, _, err := Key(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("transit KeyProvider selection must fail Key()")
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "bogus-provider")
	if _, _, err := Key(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("unsupported provider must fail Key()")
	}
	// Rotation recovery: staged .next key promotes when it decrypts the config.
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	oldKey := Continue95SecretsSyntheticKey(31)
	newKey := Continue95SecretsSyntheticKey(41)
	cfgPath := filepath.Join(dir, "rot.json")
	enc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-plaintext"), newKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath+".key", oldKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath+".key.next", newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	promoted, _, err := Key(cfgPath)
	if err != nil || string(promoted) != string(newKey) {
		t.Fatalf("pending rotation must promote staged key: err=%v", err)
	}
	if _, err := os.Stat(cfgPath + ".key.next"); !os.IsNotExist(err) {
		t.Fatal("staged key must be consumed by promotion")
	}
	// Missing-key recovery path: no .key, staged .next decrypts config.
	cfgPath2 := filepath.Join(dir, "rot2.json")
	if err := os.WriteFile(cfgPath2, enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath2+".key.next", newKey, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := Key(cfgPath2)
	if err != nil || string(recovered) != string(newKey) {
		t.Fatalf("missing-key recovery failed: err=%v", err)
	}
	// Bad staged key: readRotationKey rejects a short staged key, and
	// Key() surfaces that inspection failure instead of proceeding.
	plainPath := filepath.Join(dir, "plain.json")
	if err := os.WriteFile(plainPath, []byte(`{"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath+".key.next", []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Key(plainPath); err == nil {
		t.Fatal("short staged key must fail key inspection")
	}
}

func TestContinue95SecretsKeyProviderEdges(t *testing.T) {
	ctx := context.Background()
	envProv := EnvProvider{}
	if _, err := envProv.Load(ctx, ""); err == nil {
		t.Fatal("empty env ref must fail")
	}
	if _, err := envProv.Load(ctx, "CONTINUE95_SYNTH_MISSING_ENV_VAR_XYZ"); err == nil {
		t.Fatal("missing env var must fail")
	}
	t.Setenv("CONTINUE95_SYNTH_KEY_ENV", base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := envProv.Load(ctx, "CONTINUE95_SYNTH_KEY_ENV"); err == nil {
		t.Fatal("short env key must fail")
	}
	t.Setenv("CONTINUE95_SYNTH_KEY_ENV", base64.StdEncoding.EncodeToString(Continue95SecretsSyntheticKey(53)))
	if k, err := envProv.Load(ctx, "CONTINUE95_SYNTH_KEY_ENV"); err != nil || len(k) != 32 {
		t.Fatalf("valid env key: err=%v", err)
	}
	if err := envProv.Store(ctx, "x", Continue95SecretsSyntheticKey(53)); err == nil {
		t.Fatal("env provider Store must be read-only")
	}
	cmdProv := CommandProvider{Command: ""}
	if _, err := cmdProv.Load(ctx, "ref"); err == nil {
		t.Fatal("empty command must fail")
	}
	if err := cmdProv.Store(ctx, "ref", []byte("short")); err == nil {
		t.Fatal("command Store with short key must fail")
	}
	badCmd := CommandProvider{Command: "false"}
	if _, err := badCmd.Load(ctx, "ref"); err == nil {
		t.Fatal("failing command must fail Load")
	}
	if err := badCmd.Store(ctx, "ref", Continue95SecretsSyntheticKey(53)); err == nil {
		t.Fatal("failing command must fail Store")
	}
}

func TestContinue95SecretsVaultTransitConfiguration(t *testing.T) {
	// NewVaultTransitProvider validation.
	if _, err := NewVaultTransitProvider("", "synthetic-token", "", "transit", "k", 0); err == nil {
		t.Fatal("empty Vault address must fail")
	}
	if _, err := NewVaultTransitProvider("ftp://example.test", "synthetic-token", "", "transit", "k", 0); err == nil {
		t.Fatal("non-http(s) Vault address must fail")
	}
	if _, err := NewVaultTransitProvider("https://user@example.test", "synthetic-token", "", "transit", "k", 0); err == nil {
		t.Fatal("Vault address with userinfo must fail")
	}
	if _, err := NewVaultTransitProvider("http://192.0.2.10", "synthetic-token", "", "transit", "k", 0); err == nil {
		t.Fatal("http to non-loopback must fail")
	}
	if _, err := NewVaultTransitProvider("http://127.0.0.1:8200", "", "", "transit", "k", 0); err == nil {
		t.Fatal("empty Vault token must fail")
	}
	if _, err := NewVaultTransitProvider("http://127.0.0.1:8200", "synthetic-token", "", "bad mount!", "k", 0); err == nil {
		t.Fatal("bad mount must fail")
	}
	if _, err := NewVaultTransitProvider("http://127.0.0.1:8200", "synthetic-token", "", "transit", "k", 61*1000000000); err == nil {
		t.Fatal("timeout over the cap must fail")
	}
	p, err := NewVaultTransitProvider("http://127.0.0.1:8200", "synthetic-token", "", "transit", "synth-key", 0)
	if err != nil || p.KeyRefHash() != TransitKeyRefHash("", "transit", "synth-key") {
		t.Fatalf("loopback provider: err=%v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := p.WrapDEK(context.Background(), []byte("short")); err == nil {
		t.Fatal("WrapDEK with short DEK must fail before any request")
	}
	if _, _, err := p.UnwrapDEK(context.Background(), "not-a-ciphertext"); err == nil {
		t.Fatal("UnwrapDEK with bad ciphertext must fail before any request")
	}
	if _, _, err := p.RewrapDEK(context.Background(), "not-a-ciphertext"); err == nil {
		t.Fatal("RewrapDEK with bad ciphertext must fail before any request")
	}
	// ConfiguredEnvelope selection via the getenv hook.
	orig := getenv
	defer func() { getenv = orig }()
	setEnv := func(m map[string]string) {
		getenv = func(k string) string { return m[k] }
	}
	setEnv(map[string]string{"NEXAROUTE_TRANSIT_ENABLED": "true"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("gate true without transit provider must fail")
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "file", "NEXAROUTE_TRANSIT_ENABLED": "maybe"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("invalid gate value must fail")
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "false"})
	if _, enabled, err := ConfiguredEnvelope(); err == nil || enabled {
		t.Fatalf("gate false must report not-enabled: enabled=%v err=%v", enabled, err)
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "NEXAROUTE_MASTER_KEY": "synthetic-conflict"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("transit + raw key conflict must fail")
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN": "synthetic-a", "VAULT_TOKEN_FILE": "synthetic-b"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("both token sources must fail")
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN": "synthetic-token"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("missing addr/mount/key must fail")
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN": "synthetic-token", "VAULT_ADDR": "http://127.0.0.1:8200", "NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "synth-key"})
	if prov, enabled, err := ConfiguredEnvelope(); err != nil || !enabled || prov == nil {
		t.Fatalf("valid transit selection: enabled=%v err=%v", enabled, err)
	}
	// Token file: wrong mode, then correct mode.
	dir := t.TempDir()
	badMode := filepath.Join(dir, "token-bad")
	if err := os.WriteFile(badMode, []byte("synthetic-token"), 0o644); err != nil {
		t.Fatal(err)
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN_FILE": badMode, "VAULT_ADDR": "http://127.0.0.1:8200", "NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "synth-key"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("token file with mode 0644 must fail")
	}
	goodMode := filepath.Join(dir, "token-good")
	if err := os.WriteFile(goodMode, []byte("synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN_FILE": goodMode, "VAULT_ADDR": "http://127.0.0.1:8200", "NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "synth-key"})
	if _, enabled, err := ConfiguredEnvelope(); err != nil || !enabled {
		t.Fatalf("token file selection: enabled=%v err=%v", enabled, err)
	}
	wsMode := filepath.Join(dir, "token-ws")
	if err := os.WriteFile(wsMode, []byte(" synthetic-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN_FILE": wsMode, "VAULT_ADDR": "http://127.0.0.1:8200", "NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "synth-key"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("token file with leading whitespace must fail")
	}
	setEnv(map[string]string{"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true", "VAULT_TOKEN_FILE": filepath.Join(dir, "missing"), "VAULT_ADDR": "http://127.0.0.1:8200", "NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "synth-key"})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("missing token file must fail")
	}
	// TransitError formatting with and without an operation.
	te := &TransitError{Class: ClassTransitUnavailable, Message: "synthetic"}
	if te.Error() != "transit_unavailable: synthetic" {
		t.Fatalf("TransitError=%q", te.Error())
	}
	te2 := &TransitError{Class: ClassTransitForbidden, Op: "encrypt", Message: "synthetic"}
	if te2.Error() != "transit_forbidden: encrypt: synthetic" {
		t.Fatalf("TransitError=%q", te2.Error())
	}
}

// --- Wave 2 additions: error branches mapped from the c2ca13c profile ---

// Continue95SecretsCountingProvider delegates to a base provider until a
// call budget is exhausted, then fails. A negative limit means unlimited.
// It exists so multi-step flows (migrate seal/verify, rewrap post-write
// verify + rollback verify) can be failed at exactly one chosen step.
type Continue95SecretsCountingProvider struct {
	base        EnvelopeKeyProvider
	wrapLimit   int
	wrapCalls   int
	unwrapLimit int
	unwrapCalls int
}

func (p *Continue95SecretsCountingProvider) KeyRefHash() string { return p.base.KeyRefHash() }

func (p *Continue95SecretsCountingProvider) WrapDEK(ctx context.Context, dek []byte) (string, int, error) {
	p.wrapCalls++
	if p.wrapLimit >= 0 && p.wrapCalls > p.wrapLimit {
		return "", 0, errors.New("synthetic wrap budget exhausted")
	}
	return p.base.WrapDEK(ctx, dek)
}

func (p *Continue95SecretsCountingProvider) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	p.unwrapCalls++
	if p.unwrapLimit >= 0 && p.unwrapCalls > p.unwrapLimit {
		return nil, 0, errors.New("synthetic unwrap budget exhausted")
	}
	return p.base.UnwrapDEK(ctx, wrapped)
}

func (p *Continue95SecretsCountingProvider) RewrapDEK(ctx context.Context, wrapped string) (string, int, error) {
	return p.base.RewrapDEK(ctx, wrapped)
}

func (p *Continue95SecretsCountingProvider) Close() error { return nil }

func TestContinue95SecretsCipherAccessorsAndLoadCipher(t *testing.T) {
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-acc")}
	if !(Cipher{env: stub}.EnvelopeEnabled()) || (Cipher{}.EnvelopeEnabled()) {
		t.Fatal("EnvelopeEnabled mismatch")
	}
	if got := (Cipher{env: stub}.KeyRefHash()); got != stub.base.KeyRefHash() {
		t.Fatalf("KeyRefHash=%q", got)
	}
	if got := (Cipher{}.KeyRefHash()); got != "" {
		t.Fatalf("raw KeyRefHash=%q", got)
	}

	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	keyPath := cfgPath + ".key"
	if err := os.WriteFile(keyPath, Continue95SecretsSyntheticKey(7), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCipher(cfgPath); err == nil {
		t.Fatal("LoadCipher with mode 0644 key accepted")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCipher(cfgPath)
	if err != nil {
		t.Fatalf("LoadCipher valid key: %v", err)
	}
	if c.EnvelopeEnabled() {
		t.Fatal("raw cipher reports envelope enabled")
	}
}

func TestContinue95SecretsSealCryptoFailures(t *testing.T) {
	old := cryptoRead
	t.Cleanup(func() { cryptoRead = old })
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-crypto")}
	ctx := context.Background()

	cryptoRead = func([]byte) (int, error) { return 0, errors.New("synthetic crypto failure") }
	if _, err := (Cipher{env: stub}.SealField(ctx, "prov-synth", "api_key", []byte("synthetic"))); err == nil || !strings.Contains(err.Error(), "cannot generate data key") {
		t.Fatalf("seal DEK failure err=%v", err)
	}
	if err := ProveTransitConnectivity(ctx, stub); err == nil || !strings.Contains(err.Error(), "cannot generate Transit connectivity canary") {
		t.Fatalf("prove canary failure err=%v", err)
	}
	if _, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic"), Continue95SecretsSyntheticKey(3), true); err == nil {
		t.Fatal("TransformConfig seal with failing crypto accepted")
	}

	calls := 0
	cryptoRead = func(b []byte) (int, error) {
		calls++
		if calls == 1 {
			for i := range b {
				b[i] = byte(i)
			}
			return len(b), nil
		}
		return 0, errors.New("synthetic crypto failure")
	}
	if _, err := (Cipher{env: stub}.SealField(ctx, "prov-synth", "api_key", []byte("synthetic"))); err == nil || !strings.Contains(err.Error(), "cannot generate data nonce") {
		t.Fatalf("seal nonce failure err=%v", err)
	}
	cryptoRead = old
}

func TestContinue95SecretsParseEnvelopeAdditions(t *testing.T) {
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-parse")}
	valid := sealField(t, stub, "prov-synth", "api_key", "synthetic-secret")

	if _, err := parseEnvelope(EnvelopePrefix + strings.Repeat("A", maxEnvelopeOuterBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize envelope err=%v", err)
	}
	replaced := Continue95SecretsMutateEnvelope(t, valid, func(obj map[string]any) {
		delete(obj, "provider")
		obj["bogus"] = "x"
	})
	if _, err := parseEnvelope(replaced); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field replacement err=%v", err)
	}
	typeCases := []struct {
		field string
		value any
	}{
		{"provider", 42.0},
		{"key_ref_sha256", 42.0},
		{"transit_key_version", "1"},
		{"wrapped_dek", 42.0},
		{"data_nonce_b64url", 42.0},
		{"data_ciphertext_b64url", 42.0},
	}
	for _, tc := range typeCases {
		mutated := Continue95SecretsMutateEnvelope(t, valid, func(obj map[string]any) {
			obj[tc.field] = tc.value
		})
		if _, err := parseEnvelope(mutated); err == nil || !strings.Contains(err.Error(), "field "+tc.field) {
			t.Fatalf("type mismatch %s err=%v", tc.field, err)
		}
	}
	truncated := Continue95SecretsRawEnvelope(t, `{"envelope_version": 2`)
	if _, err := parseEnvelope(truncated); err == nil || !strings.Contains(err.Error(), "malformed object") {
		t.Fatalf("truncated object err=%v", err)
	}
}

func TestContinue95SecretsRewrapBytesBranches(t *testing.T) {
	ctx := context.Background()
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-rewrap")}
	sealed := sealField(t, stub, "prov-synth", "api_key", "synthetic-secret")
	var doc map[string]any
	raw := Continue95SecretsProviderConfig(sealed)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	provs := doc["providers"].([]any)
	doc["providers"] = append(provs, map[string]any{"id": "num-synth", "api_key": 42.0})
	mixed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	out, _, _, err := RewrapConfigBytes(ctx, stub, mixed)
	if err != nil {
		t.Fatalf("rewrap numeric doc: %v", err)
	}
	var outDoc map[string]any
	if err := json.Unmarshal(out, &outDoc); err != nil {
		t.Fatal(err)
	}
	outProvs := outDoc["providers"].([]any)
	if len(outProvs) != 2 || outProvs[1].(map[string]any)["api_key"] != 42.0 {
		t.Fatalf("numeric field changed: %v", outProvs)
	}
	if versions, err := DistinctTransitKeyVersions(mixed); err != nil || len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("distinct numeric doc versions=%v err=%v", versions, err)
	}
	if ref, versions, err := TransitEnvelopeInfo(mixed); err != nil || ref != stub.base.KeyRefHash() || len(versions) != 1 {
		t.Fatalf("transit info numeric doc ref=%q versions=%v err=%v", ref, versions, err)
	}

	rewrapFail := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-rewrap2"), rewrapErr: errors.New("synthetic rewrap failure")}
	sealed2 := sealField(t, rewrapFail.base, "prov-synth", "api_key", "synthetic-secret")
	if _, _, _, err := RewrapConfigBytes(ctx, rewrapFail, Continue95SecretsProviderConfig(sealed2)); err == nil {
		t.Fatal("rewrap with failing RewrapDEK accepted")
	}
	invalidStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-rewrap3"), rewrapHas: true, rewrapResult: "garbage-ciphertext"}
	sealed3 := sealField(t, invalidStub.base, "prov-synth", "api_key", "synthetic-secret")
	if _, _, _, err := RewrapConfigBytes(ctx, invalidStub, Continue95SecretsProviderConfig(sealed3)); err == nil || !strings.Contains(err.Error(), "invalid wrapped data key") {
		t.Fatalf("rewrap invalid wrapped err=%v", err)
	}
	unknownStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-rewrap4"), rewrapHas: true, rewrapResult: "vault:v2:QUJDREVG"}
	sealed4 := sealField(t, unknownStub.base, "prov-synth", "api_key", "synthetic-secret")
	if _, _, _, err := RewrapConfigBytes(ctx, unknownStub, Continue95SecretsProviderConfig(sealed4)); err == nil || !strings.Contains(err.Error(), "post-verification failed") {
		t.Fatalf("rewrap post-verification err=%v", err)
	}
}

func TestContinue95SecretsMigrateBytesBranches(t *testing.T) {
	ctx := context.Background()
	legacy := Continue95SecretsSyntheticKey(5)
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-mig")}
	if _, _, err := MigrateConfigBytes(ctx, []byte("{bad json"), legacy, stub); err == nil {
		t.Fatal("migrate classify error accepted")
	}
	failStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-mig2"), unwrapErr: errors.New("synthetic unwrap failure")}
	sealedFail := sealField(t, failStub.base, "prov-synth", "api_key", "synthetic-secret")
	if _, _, err := MigrateConfigBytes(ctx, Continue95SecretsProviderConfig(sealedFail), legacy, failStub); err == nil {
		t.Fatal("migrate nxs2 verify failure accepted")
	}
	nxs1doc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-migrate-secret"), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	sealFail := &Continue95SecretsCountingProvider{base: stub, wrapLimit: 1, unwrapLimit: -1}
	if _, _, err := MigrateConfigBytes(ctx, nxs1doc, legacy, sealFail); err == nil {
		t.Fatal("migrate seal failure accepted")
	}
	verifyFail := &Continue95SecretsCountingProvider{base: stub, wrapLimit: -1, unwrapLimit: 1}
	if _, _, err := MigrateConfigBytes(ctx, nxs1doc, legacy, verifyFail); err == nil || !strings.Contains(err.Error(), "in-memory verification failed") {
		t.Fatalf("migrate verify failure err=%v", err)
	}
}

func TestContinue95SecretsDryRunBranches(t *testing.T) {
	ctx := context.Background()
	legacy := Continue95SecretsSyntheticKey(5)
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-dry")}
	if _, err := MigrateDryRunInfo(ctx, []byte("{bad"), legacy, stub); err == nil {
		t.Fatal("migrate dry-run classify error accepted")
	}
	unknownDoc := []byte(`{"providers":[{"id":"a","api_key":"nxs3:whatever"},{"id":"b","api_key":"nxs3:other"}]}`)
	if _, err := MigrateDryRunInfo(ctx, unknownDoc, legacy, stub); err == nil || !strings.Contains(err.Error(), "a:api_key") || !strings.Contains(err.Error(), "b:api_key") || !strings.Contains(err.Error(), ", ") {
		t.Fatalf("migrate dry-run unknown err=%v", err)
	}
	nxs1doc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-migrate-secret"), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	sealed := sealField(t, stub, "prov-synth", "api_key", "synthetic-secret")
	var mixed map[string]any
	if err := json.Unmarshal(nxs1doc, &mixed); err != nil {
		t.Fatal(err)
	}
	var nxs2map map[string]any
	if err := json.Unmarshal(Continue95SecretsProviderConfig(sealed), &nxs2map); err != nil {
		t.Fatal(err)
	}
	mixed["providers"] = append(mixed["providers"].([]any), nxs2map["providers"].([]any)...)
	mixedDoc, err := json.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateDryRunInfo(ctx, mixedDoc, legacy, stub); err == nil || !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("migrate dry-run mixed err=%v", err)
	}
	failStub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-dry2"), unwrapErr: errors.New("synthetic unwrap failure")}
	sealedFail := sealField(t, failStub.base, "prov-synth", "api_key", "synthetic-secret")
	if _, err := MigrateDryRunInfo(ctx, Continue95SecretsProviderConfig(sealedFail), legacy, failStub); err == nil {
		t.Fatal("migrate dry-run nxs2 decrypt failure accepted")
	}
	if n, err := MigrateDryRunInfo(ctx, []byte(`{"providers": [{"id": "x"}]}`), legacy, stub); err != nil || n != 0 {
		t.Fatalf("migrate dry-run no-secret n=%d err=%v", n, err)
	}
	wrong := Continue95SecretsSyntheticKey(9)
	if _, err := MigrateDryRunInfo(ctx, nxs1doc, wrong, stub); err == nil {
		t.Fatal("migrate dry-run nxs1 decrypt failure accepted")
	}
	if _, err := MigrateDryRunInfo(ctx, nxs1doc, legacy, failStub); err == nil {
		t.Fatal("migrate dry-run prove failure accepted")
	}

	if _, _, err := RewrapDryRunInfo(ctx, []byte("{bad"), stub); err == nil {
		t.Fatal("rewrap dry-run classify error accepted")
	}
	if _, _, err := RewrapDryRunInfo(ctx, []byte(`{"providers":[{"id":"a","api_key":"nxs3:whatever"}]}`), stub); err == nil {
		t.Fatal("rewrap dry-run unknown accepted")
	}
	if _, _, err := RewrapDryRunInfo(ctx, Continue95SecretsProviderConfig(sealedFail), failStub); err == nil || !strings.Contains(err.Error(), "pre-verification") {
		t.Fatalf("rewrap dry-run pre-verification err=%v", err)
	}
}

func TestContinue95SecretsCountingProviderRewrapFileRollback(t *testing.T) {
	ctx := context.Background()
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-roll")}
	sealed := sealField(t, stub, "prov-synth", "api_key", "synthetic-secret")
	stub.base.version = 2
	counting := &Continue95SecretsCountingProvider{base: stub, wrapLimit: -1, unwrapLimit: 2}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, Continue95SecretsProviderConfig(sealed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RewrapConfigFile(ctx, path, counting)
	if err == nil || !strings.Contains(err.Error(), "rollback also failed") {
		t.Fatalf("rewrap file rollback err=%v", err)
	}
}

// Continue95SecretsVaultProvider builds a VaultTransitProvider against a
// local httptest server running handler. No live Vault is ever contacted.
func Continue95SecretsVaultProvider(t *testing.T, handler http.HandlerFunc) *VaultTransitProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := NewVaultTransitProvider(srv.URL, "synthetic-token", "", "transit", "synth-key", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func Continue95SecretsVaultJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestContinue95SecretsRollbackAndWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	verifyOK := func([]byte) error { return nil }
	if err := rollbackFromBackup(filepath.Join(dir, "config.json"), filepath.Join(dir, "missing.bak"), verifyOK); err == nil {
		t.Fatal("rollback with missing backup accepted")
	}
	backupPath := filepath.Join(dir, "config.json.bak")
	if err := os.WriteFile(backupPath, []byte("synthetic backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rollbackFromBackup(dir, backupPath, verifyOK); err == nil {
		t.Fatal("rollback rename onto a directory accepted")
	}
	target := filepath.Join(dir, "target.json")
	verifyFail := func([]byte) error { return errors.New("synthetic verify failure") }
	if err := rollbackFromBackup(target, backupPath, verifyFail); err == nil || !strings.Contains(err.Error(), "synthetic verify failure") {
		t.Fatalf("rollback verify failure err=%v", err)
	}

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(blocker, "x"), []byte("data")); err == nil {
		t.Fatal("writeFileAtomic under a regular file accepted")
	}
	if err := writeFileAtomic(dir, []byte("data")); err == nil {
		t.Fatal("writeFileAtomic onto a directory accepted")
	}
}

func TestContinue95SecretsVaultTransitResponses(t *testing.T) {
	ctx := context.Background()
	key := Continue95SecretsSyntheticKey(11)

	garbage := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		Continue95SecretsVaultJSON(w, map[string]any{"data": map[string]any{"ciphertext": "garbage", "key_version": 1}})
	})
	if _, _, err := garbage.WrapDEK(ctx, key); err == nil || !strings.Contains(err.Error(), "invalid ciphertext") {
		t.Fatalf("encrypt garbage err=%v", err)
	}
	if _, _, err := garbage.RewrapDEK(ctx, "vault:v1:QUJDREVG"); err == nil || !strings.Contains(err.Error(), "invalid ciphertext") {
		t.Fatalf("rewrap garbage err=%v", err)
	}

	short := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		Continue95SecretsVaultJSON(w, map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(make([]byte, 16))}})
	})
	if _, _, err := short.UnwrapDEK(ctx, "vault:v1:QUJDREVG"); err == nil || !strings.Contains(err.Error(), "invalid data key") {
		t.Fatalf("decrypt short err=%v", err)
	}

	inconsistent := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		Continue95SecretsVaultJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:QUJDREVG", "key_version": 2}})
	})
	if _, _, err := inconsistent.RewrapDEK(ctx, "vault:v1:QUJDREVG"); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("rewrap inconsistent err=%v", err)
	}

	missing := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		Continue95SecretsVaultJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:QUJDREVG", "key_version": 0}})
	})
	if _, _, err := missing.RewrapDEK(ctx, "vault:v1:QUJDREVG"); err == nil || !strings.Contains(err.Error(), "missing key_version") {
		t.Fatalf("rewrap missing version err=%v", err)
	}

	unavailable := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	cancelCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, _, err := unavailable.WrapDEK(cancelCtx, key); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("retry cancellation err=%v", err)
	}
	cancel()

	truncated := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, buf, hijackErr := hj.Hijack(); hijackErr == nil {
				_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n0123456789")
				_ = buf.Flush()
				_ = conn.Close()
			}
		}
	})
	if _, _, err := truncated.WrapDEK(ctx, key); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("truncated body err=%v", err)
	}

	teapot := Continue95SecretsVaultProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	if _, _, err := teapot.WrapDEK(ctx, key); err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatalf("default status err=%v", err)
	}

	if _, err := NewVaultTransitProvider("example.com", "synthetic-token", "", "transit", "k", 5*time.Second); err == nil || !strings.Contains(err.Error(), "absolute URL") {
		t.Fatalf("bare address err=%v", err)
	}
}

func TestContinue95SecretsConfiguredEnvelopeBranches(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_TOKEN_FILE", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_ENV", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_COMMAND", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_REF", "")
	if p, enabled, err := ConfiguredEnvelope(); err != nil || enabled || p != nil {
		t.Fatalf("unset provider p=%v enabled=%v err=%v", p, enabled, err)
	}

	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	if _, _, err := ConfiguredEnvelope(); err == nil || !strings.Contains(err.Error(), "requires VAULT_TOKEN") {
		t.Fatalf("missing token err=%v", err)
	}

	t.Setenv("VAULT_TOKEN", "synthetic-token")
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:9")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "")
	if _, _, err := ConfiguredEnvelope(); err == nil || !strings.Contains(err.Error(), "requires NEXAROUTE_TRANSIT_KEY") {
		t.Fatalf("missing key err=%v", err)
	}

	t.Setenv("VAULT_ADDR", "ftp://x")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "synth-key")
	if _, _, err := ConfiguredEnvelope(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("bad scheme err=%v", err)
	}
}

func TestContinue95SecretsKeyRotationBranches(t *testing.T) {
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")

	t.Setenv("NEXAROUTE_KEY_PROVIDER", "command")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_REF", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_COMMAND", "")
	prov, ref, enabled, err := configuredProvider()
	if err != nil || !enabled || ref != "nexaroute/master-key" {
		t.Fatalf("command provider prov=%v ref=%q enabled=%v err=%v", prov, ref, enabled, err)
	}
	if cp, ok := prov.(CommandProvider); !ok || cp.Command != "" {
		t.Fatalf("command provider detail=%v", prov)
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")

	dir := t.TempDir()
	current := Continue95SecretsSyntheticKey(3)
	next := Continue95SecretsSyntheticKey(9)
	third := Continue95SecretsSyntheticKey(15)

	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath+".key", current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath+".key.next", make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Key(badPath); err == nil || !strings.Contains(err.Error(), "cannot inspect staged master key") {
		t.Fatalf("bad staged key err=%v", err)
	}

	missingPath := filepath.Join(dir, "missing.json")
	if err := os.WriteFile(missingPath+".key", current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missingPath+".key.next", next, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _, err := Key(missingPath); err != nil || !strings.EqualFold(hex.EncodeToString(got), hex.EncodeToString(current)) {
		t.Fatalf("missing config key err=%v", err)
	}

	thirdPath := filepath.Join(dir, "third.json")
	sealed, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-third-secret"), third, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thirdPath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thirdPath+".key", current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thirdPath+".key.next", next, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _, err := Key(thirdPath); err != nil || !strings.EqualFold(hex.EncodeToString(got), hex.EncodeToString(current)) {
		t.Fatalf("both-decrypt-fail key err=%v", err)
	}
}

func TestContinue95SecretsOpenAndTransformBranches(t *testing.T) {
	short := make([]byte, 20)
	for i := range short {
		short[i] = byte(i + 2)
	}
	text := "nxs1:" + keyID(short) + ":" + base64.RawStdEncoding.EncodeToString(make([]byte, 12)) + ":" + base64.RawStdEncoding.EncodeToString(make([]byte, 100))
	if _, err := Open(short, []byte("test-aad"), text); err == nil || !strings.Contains(err.Error(), "invalid master key") {
		t.Fatalf("open short key err=%v", err)
	}

	numericDoc := []byte(`{"providers": [{"id": "num", "api_key": 42}]}`)
	out, n, err := TransformConfig(numericDoc, Continue95SecretsSyntheticKey(3), true)
	if err != nil || n != 0 || !strings.Contains(string(out), "42") {
		t.Fatalf("transform numeric out=%s n=%d err=%v", out, n, err)
	}
	plainDoc := Continue95SecretsProviderConfig("synthetic-plain")
	out, n, err = TransformConfig(plainDoc, Continue95SecretsSyntheticKey(3), false)
	if err != nil || n != 0 || !strings.Contains(string(out), "synthetic-plain") {
		t.Fatalf("transform passthrough out=%s n=%d err=%v", out, n, err)
	}

	if _, err := EncryptedBackupWith(Cipher{raw: make([]byte, 20)}, []byte("synthetic backup")); err == nil {
		t.Fatal("encrypted backup with short raw key accepted")
	}
}

func TestContinue95SecretsSubstituteFieldsBranches(t *testing.T) {
	if _, err := SubstituteSecretFields([]byte("{bad"), func(owner, field, current string) (string, error) {
		return current, nil
	}); err == nil {
		t.Fatal("substitute decode error accepted")
	}
	replace := func(owner, field, current string) (string, error) { return "replaced-" + current, nil }
	mixedDoc := []byte(`{"providers": [{"id": "a", "api_key": 42}, {"id": "b", "api_key": "synthetic"}]}`)
	out, err := SubstituteSecretFields(mixedDoc, replace)
	if err != nil || !strings.Contains(string(out), "replaced-synthetic") || !strings.Contains(string(out), "42") {
		t.Fatalf("substitute numeric out=%s err=%v", out, err)
	}
	headerDoc := []byte(`{"providers": [{"id": "a", "api_key": "synthetic", "headers": {"X-Custom": "v"}}]}`)
	headerFail := func(owner, field, current string) (string, error) {
		if field == "header:X-Custom" {
			return "", errors.New("synthetic header failure")
		}
		return current, nil
	}
	if _, err := SubstituteSecretFields(headerDoc, headerFail); err == nil || !strings.Contains(err.Error(), "header") {
		t.Fatalf("substitute header err=%v", err)
	}
	credDoc := []byte(`{"providers": [{"id": "a", "api_key": "synthetic", "credentials": [{"api_key": "v"}]}]}`)
	credFail := func(owner, field, current string) (string, error) {
		if field == "credentials[0].api_key" {
			return "", errors.New("synthetic credential failure")
		}
		return current, nil
	}
	if _, err := SubstituteSecretFields(credDoc, credFail); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("substitute credential err=%v", err)
	}
	credNonMap := []byte(`{"providers": [{"id": "a", "api_key": "synthetic", "credentials": ["x"]}]}`)
	if _, err := SubstituteSecretFields(credNonMap, replace); err != nil {
		t.Fatalf("substitute credential non-map err=%v", err)
	}
	provNonMap := []byte(`{"providers": ["x"]}`)
	if _, err := SubstituteSecretFields(provNonMap, replace); err != nil {
		t.Fatalf("substitute provider non-map err=%v", err)
	}
}

func TestContinue95SecretsValidateTransitReferenceKey(t *testing.T) {
	// NOTE: validateTransitPath's single-segment branch (envelope.go
	// 238-239) is NOT_RUN: both callers pass allowMulti=true, so the
	// branch is dead. A multi-segment key fails at the key regex instead.
	if err := ValidateTransitReference("ns", "mount", "a/b"); err == nil || !strings.Contains(err.Error(), "invalid Transit key") {
		t.Fatalf("multi-segment key err=%v", err)
	}
}

func TestContinue95SecretsFileLockAndKeyring(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("continue95-lock")}
	if _, err := RewrapConfigFile(context.Background(), filepath.Join(blocker, "config.json"), stub); err == nil || !strings.Contains(err.Error(), "cannot open migration lock file") {
		t.Fatalf("lock open failure err=%v", err)
	}

	sealed := sealField(t, stub, "prov-synth", "api_key", "synthetic-secret")
	latestPath := filepath.Join(dir, "latest.json")
	if err := os.WriteFile(latestPath, Continue95SecretsProviderConfig(sealed), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := RewrapConfigFile(context.Background(), latestPath, stub)
	if err != nil || !res.AlreadyLatest {
		t.Fatalf("lock acquire/release via already-latest res=%+v err=%v", res, err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, "secret-tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho stored\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	if err := (linuxKeyringProvider{}).Store(context.Background(), "nexaroute/master-key", Continue95SecretsSyntheticKey(4)); err != nil {
		t.Fatalf("keyring store err=%v", err)
	}
}

// TestContinue95SecretsSealWrappedParse covers sealEnvelope's
// wrapped-key parse failure: a provider that returns a wrapped value
// in no recognizable Transit format makes SealField fail instead of
// producing an envelope that could never be opened.
func TestContinue95SecretsSealWrappedParse(t *testing.T) {
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("synth-key"), wrapHas: true, wrapResult: "not-a-vault-ciphertext"}
	if _, err := (Cipher{env: stub}.SealField(context.Background(), "owner", "field", []byte("synthetic-secret"))); err == nil || !strings.Contains(err.Error(), "invalid wrapped data key") {
		t.Fatalf("seal with unparseable wrapped key err=%v", err)
	}
}

// TestContinue95SecretsTransformWithDecryptPassthrough covers
// TransformConfigWith's decrypt passthrough: with a raw (non-envelope)
// cipher, a plaintext field decrypts to itself with zero fields
// transformed.
func TestContinue95SecretsTransformWithDecryptPassthrough(t *testing.T) {
	key := Continue95SecretsSyntheticKey(0x40)
	doc := Continue95SecretsProviderConfig("synthetic-plain-key")
	out, n, err := TransformConfigWith(context.Background(), Cipher{raw: key}, doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("passthrough transformed n=%d", n)
	}
	if !strings.Contains(string(out), "synthetic-plain-key") {
		t.Fatalf("passthrough lost plaintext: %s", out)
	}
}

// TestContinue95SecretsRewrapBytesClassifyError covers
// RewrapConfigBytes' classify failure: undecodable input is rejected
// before any provider call.
func TestContinue95SecretsRewrapBytesClassifyError(t *testing.T) {
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("synth-key")}
	if _, _, _, err := RewrapConfigBytes(context.Background(), stub, []byte("{bad")); err == nil {
		t.Fatal("rewrap of undecodable config accepted")
	}
}

// TestContinue95SecretsKeyringStoreFailure covers the keyring Store
// error return: a failing secret-tool helper makes Store fail closed
// instead of silently dropping the key.
func TestContinue95SecretsKeyringStoreFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "secret-tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := (linuxKeyringProvider{}.Store(context.Background(), "synth-ref", Continue95SecretsSyntheticKey(0x50))); err == nil || !strings.Contains(err.Error(), "OS keyring store failed") {
		t.Fatalf("keyring store failure err=%v", err)
	}
}

// TestContinue95SecretsRewrapAndMigrateBackupWriteFailures covers the
// pre-replace backup-write failures of RewrapConfigFile and
// MigrateToEnvelopeFile: when the timestamped backup name is already
// taken by a directory, the atomic backup write fails and the config
// is left untouched instead of being replaced without a backup.
func TestContinue95SecretsRewrapAndMigrateBackupWriteFailures(t *testing.T) {
	ctx := context.Background()
	// Rewrap: sealed at version 1, provider bumped to 2, so a rewrap
	// is genuinely pending and the backup step is reached.
	stub := &Continue95SecretsStubProvider{base: newFakeEnvelope("synth-key")}
	sealed, _, err := TransformConfigWith(ctx, Cipher{env: stub}, Continue95SecretsProviderConfig("synthetic-rewrap-key"), true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rewrapPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(rewrapPath, append(sealed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	stub.base.version = 2
	for i := 0; i <= 10; i++ {
		ts := time.Now().UTC().Add(time.Duration(i) * time.Second).Format("20060102T150405Z")
		if err := os.MkdirAll(rewrapPath+"."+ts+".pre-rewrap.bak", 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RewrapConfigFile(ctx, rewrapPath, stub); err == nil || !strings.Contains(err.Error(), "cannot persist verbatim pre-rewrap backup") {
		t.Fatalf("rewrap backup-write failure err=%v", err)
	}
	if after, err := os.ReadFile(rewrapPath); err != nil || !bytes.Equal(bytes.TrimSpace(after), bytes.TrimSpace(sealed)) {
		t.Fatalf("rewrap failure modified config: err=%v", err)
	}

	// Migration: sealed legacy config plus the correct legacy key, so
	// migration is pending and the backup step is reached.
	legacy := Continue95SecretsSyntheticKey(0x60)
	legacyDoc, _, err := TransformConfig(Continue95SecretsProviderConfig("synthetic-migrate-key"), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	migPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(migPath, append(legacyDoc, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 10; i++ {
		ts := time.Now().UTC().Add(time.Duration(i) * time.Second).Format("20060102T150405Z")
		if err := os.MkdirAll(migPath+"."+ts+".pre-transit.bak", 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := MigrateToEnvelopeFile(ctx, migPath, legacy, stub); err == nil || !strings.Contains(err.Error(), "cannot persist verbatim pre-migration backup") {
		t.Fatalf("migration backup-write failure err=%v", err)
	}
	if after, err := os.ReadFile(migPath); err != nil || !bytes.Equal(bytes.TrimSpace(after), bytes.TrimSpace(legacyDoc)) {
		t.Fatalf("migration failure modified config: err=%v", err)
	}
}

// TestContinue95SecretsParseEnvelopeTruncatedString probes the
// closing-token error of decodeStrictObject: an object whose last
// value is a complete string but which lacks its closing brace must
// be rejected as a malformed object.
func TestContinue95SecretsParseEnvelopeTruncatedString(t *testing.T) {
	env := Continue95SecretsRawEnvelope(t, `{"envelope_version": "x"`)
	if _, err := parseEnvelope(env); err == nil || !strings.Contains(err.Error(), "malformed object") {
		t.Fatalf("truncated string-value envelope err=%v", err)
	}
}
