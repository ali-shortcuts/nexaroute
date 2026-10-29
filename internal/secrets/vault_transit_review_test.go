package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file integrates the pre-rework review pins (vault_transit_review_test.go
// at 85ba159) with the reworked behavior. Each test names the finding it
// pins: kept pins document behavior that still holds; flipped pins document
// a finding the rework fixed, and assert the NEW behavior.

// TestReviewDataGCMAuthenticatesWithoutVaultAAD pins ADR 0007 §5.2:
// unwrap success is NOT authentication. The provider returns the DEK for
// a wrapped value without any owner/field context (the design sends no
// aad or context to Transit); only the data GCM open with the canonical
// AAD authenticates the field.
func TestReviewDataGCMAuthenticatesWithoutVaultAAD(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "p", "api_key", "canary")
	meta, err := parseEnvelope(text)
	if err != nil {
		t.Fatal(err)
	}
	// Unwrap succeeds: the provider knows nothing about the field.
	if _, _, err := env.UnwrapDEK(context.Background(), meta.WrappedDEK); err != nil {
		t.Fatalf("unwrap should succeed (it is not authentication): %v", err)
	}
	// But opening with the wrong owner/field fails at the GCM check.
	if _, err := openEnvelope(context.Background(), env, "p", "proxy_url", text); err == nil {
		t.Fatal("wrong-field GCM open authenticated")
	}
	if _, err := openEnvelope(context.Background(), env, "q", "api_key", text); err == nil {
		t.Fatal("wrong-owner GCM open authenticated")
	}
}

// TestReviewCrossConfigSubstitutionLimitation pins the F1 residual: the
// revised ADR's AAD still does not bind config identity, so a whole
// envelope transplanted between configs sharing key reference, owner,
// and field decrypts. Mitigation is operational (distinct Transit keys
// per environment/tenant) and documented; this test must flip if
// config-identity AAD ever lands. One fake models one Vault key shared
// by two configs: the envelope string moves unchanged and still opens.
func TestReviewCrossConfigSubstitutionLimitation(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "p", "api_key", "cross-config")
	if got := openField(t, env, "p", "api_key", text); got != "cross-config" {
		t.Fatalf("cross-config transplant unexpectedly failed: %q", got)
	}
	otherKey := newFakeEnvelope("key-b")
	if _, err := openEnvelope(context.Background(), otherKey, "p", "api_key", text); err == nil {
		t.Fatal("cross-key transplant authenticated")
	}
}

// TestReviewVersionFieldTamperFails pins explicit key versions (§3.2):
// envelope version, transit key version, and wrapped version mismatches
// are all rejected before any provider call.
func TestReviewVersionFieldTamperFails(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "p", "api_key", "v")
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
	for name, enc := range map[string]string{
		"envelope_version 0": tamper(func(m *envelopeMeta) { m.EnvelopeVersion = 0 }),
		"envelope_version 1": tamper(func(m *envelopeMeta) { m.EnvelopeVersion = 1 }),
		"envelope_version 3": tamper(func(m *envelopeMeta) { m.EnvelopeVersion = 3 }),
		"transit version 0":  tamper(func(m *envelopeMeta) { m.TransitKeyVersion = 0 }),
		"transit version 2":  tamper(func(m *envelopeMeta) { m.TransitKeyVersion = 2 }),
		"wrapped v2":         tamper(func(m *envelopeMeta) { m.WrappedDEK = "vault:v2:AAAA" }),
	} {
		if _, err := parseEnvelope(enc); err == nil {
			t.Errorf("parser accepted %s", name)
		}
	}
	// Key-reference tamper is rejected at open (before any Vault call).
	tampered := meta
	tampered.KeyRefSHA256 = strings.Repeat("f", 64)
	enc, _ := encodeEnvelope(tampered)
	if _, err := openEnvelope(context.Background(), env, "p", "api_key", enc); err == nil {
		t.Fatal("key-reference tamper opened")
	}
	var terr *TransitError
	if _, err := openEnvelope(context.Background(), env, "p", "api_key", enc); !errors.As(err, &terr) || terr.Class != ClassTransitConfigInvalid {
		t.Fatalf("key-reference tamper must be transit_config_invalid, got %v", err)
	}
}

// TestReviewParserStrictnessFix pins the F4 fix: the parser that used to
// ignore unknown fields and take the last duplicate now rejects both,
// plus trailing data.
func TestReviewParserStrictnessFix(t *testing.T) {
	env := newFakeEnvelope("key-a")
	text := sealField(t, env, "p", "api_key", "x")
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, EnvelopePrefix))
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	obj["surprise"] = 1
	b, _ := json.Marshal(obj)
	if _, err := parseEnvelope(EnvelopePrefix + base64.RawURLEncoding.EncodeToString(b)); err == nil {
		t.Fatal("unknown field accepted (F4 not fixed)")
	}
	// Duplicate key.
	dup := strings.Replace(string(raw), `"provider"`, `"provider":"vault-transit","provider"`, 1)
	if _, err := parseEnvelope(EnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(dup))); err == nil {
		t.Fatal("duplicate key accepted (F4 not fixed)")
	}
	// Trailing data.
	trailing := string(raw) + ` {}`
	if _, err := parseEnvelope(EnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(trailing))); err == nil {
		t.Fatal("trailing data accepted (F4 not fixed)")
	}
}

// countingEnvelope fails UnwrapDEK on the nth call, letting tests force a
// post-write verification failure deterministically.
type countingEnvelope struct {
	EnvelopeKeyProvider
	failOn int
	calls  atomic.Int32
}

func (c *countingEnvelope) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	if c.calls.Add(1) == int32(c.failOn) {
		return nil, 0, errors.New("counting fake: forced unwrap failure")
	}
	return c.EnvelopeKeyProvider.UnwrapDEK(ctx, wrapped)
}

// TestReviewMigrationRollbackOnPostWriteFailure pins ADR 0007 §7.1/§7.2:
// a post-write verification failure rolls back to the verbatim backup
// and reports failure; the file ends up as the original bytes, never a
// mixed or half-migrated config. Unwrap sequence: canary (1), in-memory
// verify (2), file post-write verify (3) — the third is forced to fail.
func TestReviewMigrationRollbackOnPostWriteFailure(t *testing.T) {
	legacyKey := []byte("01234567890123456789012345678901")
	base := newFakeEnvelope("key-a")
	env := &countingEnvelope{EnvelopeKeyProvider: base, failOn: 3}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rollback-canary"}]}`)
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	original := append(old, '\n')
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := MigrateToEnvelopeFile(context.Background(), path, legacyKey, env)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("post-write failure must roll back and report it: result=%+v err=%v", result, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("rollback did not restore the original bytes")
	}
	// The backup is consumed by the atomic rename; the pre-operation
	// state survives in the config itself, which is what was verified.
}

// TestReviewRewrapRollbackOnPostWriteFailure pins the same §7.1 rule for
// rewrap. Unwrap sequence: pre-verify (1), in-memory post-verify (2),
// file post-write verify (3) — the third is forced to fail.
func TestReviewRewrapRollbackOnPostWriteFailure(t *testing.T) {
	base := newFakeEnvelope("key-a")
	env := &countingEnvelope{EnvelopeKeyProvider: base, failOn: 3}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rewrap-rollback"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: base}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	original := append(enc, '\n')
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	base.rotate()
	result, err := RewrapConfigFile(context.Background(), path, env)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("rewrap post-write failure must roll back: %+v %v", result, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("rewrap rollback did not restore the original bytes")
	}
}

// TestReviewLegacyKeyOnlyRequiredForNXS1 pins the F11 fix at the file
// level: plaintext-only and already-nxs2 configs migrate without any
// legacy key; nxs1 configs still demand it.
func TestReviewLegacyKeyOnlyRequiredForNXS1(t *testing.T) {
	legacyKey := []byte("01234567890123456789012345678901")
	env := newFakeEnvelope("key-a")
	dir := t.TempDir()
	// Plaintext-only: fails with the plaintext message, not a key error.
	plain := filepath.Join(dir, "plain.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"plain"}]}`)
	if err := os.WriteFile(plain, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateToEnvelopeFile(context.Background(), plain, nil, env); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("plaintext-only migration must fail with the plaintext message, got %v", err)
	}
	// Already-nxs2: verify-only, no legacy key needed.
	nxs2 := filepath.Join(dir, "nxs2.json")
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nxs2, append(enc, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := MigrateToEnvelopeFile(context.Background(), nxs2, nil, env); err != nil || result.Migrated != 0 {
		t.Fatalf("already-nxs2 migration must verify without a legacy key: %+v %v", result, err)
	}
	// nxs1: legacy key demanded.
	nxs1 := filepath.Join(dir, "nxs1.json")
	old, _, err := TransformConfig(raw, legacyKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nxs1, append(old, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateToEnvelopeFile(context.Background(), nxs1, nil, env); err == nil {
		t.Fatal("nxs1 migration without legacy key succeeded")
	}
}

// TestReviewVaultErrorClasses pins §8.4 mapping at the HTTP layer: 404 is
// a single attempt with transit_key_not_found; 429 retries (3 attempts)
// and ends transit_unavailable.
func TestReviewVaultErrorClasses(t *testing.T) {
	dek := []byte("01234567890123456789012345678901")
	t.Run("404 single attempt", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			http.Error(w, "nope", http.StatusNotFound)
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "tok", "", "transit", "nexaroute", time.Second)
		_, _, err := p.UnwrapDEK(context.Background(), "vault:v1:AAAA")
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitKeyNotFound {
			t.Fatalf("404 must be transit_key_not_found, got %v", err)
		}
		if attempts.Load() != 1 {
			t.Fatalf("404 must not be retried, got %d attempts", attempts.Load())
		}
	})
	t.Run("429 retries then unavailable", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			http.Error(w, "slow", http.StatusTooManyRequests)
		}))
		defer srv.Close()
		p, _ := NewVaultTransitProvider(srv.URL, "tok", "", "transit", "nexaroute", time.Second)
		_, _, err := p.WrapDEK(context.Background(), dek)
		var terr *TransitError
		if !errors.As(err, &terr) || terr.Class != ClassTransitUnavailable {
			t.Fatalf("exhausted 429 must be transit_unavailable, got %v", err)
		}
		if attempts.Load() != 3 {
			t.Fatalf("429 must be attempted 3 times, got %d", attempts.Load())
		}
	})
}

// TestReviewExistingKeyFileIgnoredInTransit pins §4: Transit load never
// creates, reads, or writes <config>.key files, including when a stale
// invalid one happens to exist.
func TestReviewExistingKeyFileIgnoredInTransit(t *testing.T) {
	fake := newFakeTransitServer()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "tok")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "nexaroute")
	cipher, err := LoadCipher(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cipher.EnvelopeEnabled() {
		t.Fatal("expected an envelope cipher")
	}
	raw := []byte(`{"providers":[{"id":"p","api_key":"keyfile-canary"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), cipher, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(enc, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".key", []byte("stale-invalid-key-material"), 0600); err != nil {
		t.Fatal(err)
	}
	cipher2, err := LoadCipher(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := TransformConfigWith(context.Background(), cipher2, enc, false)
	if err != nil || !strings.Contains(string(plain), "keyfile-canary") {
		t.Fatalf("Transit load with stale .key present failed: %v", err)
	}
	for _, suffix := range []string{".key.next", ".key.previous"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("Transit mode created %s", suffix)
		}
	}
	stale, _ := os.ReadFile(path + ".key")
	if string(stale) != "stale-invalid-key-material" {
		t.Fatal("Transit mode modified the stale .key file")
	}
}

// TestReviewRewrapEndpointAssertion pins §7.3: a full rewrap flow calls
// the rewrap endpoint and never the encrypt endpoint; decrypt calls are
// the pre/post verifications, distinguishable in the request log order.
func TestReviewRewrapEndpointAssertion(t *testing.T) {
	fake := newFakeTransitServer()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	p, err := NewVaultTransitProvider(srv.URL, "tok", "", "transit", "nexaroute", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"providers":[{"id":"p","api_key":"endpoint-canary"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: p}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	// Rotate so rewrap actually moves versions.
	fake.rotate()
	before := len(fake.requests)
	out, n, latest, err := RewrapConfigBytes(context.Background(), p, enc)
	if err != nil || n != 1 || latest {
		t.Fatalf("rewrap: n=%d latest=%v err=%v", n, latest, err)
	}
	_ = out
	ops := []string{}
	for _, r := range fake.requests[before:] {
		ops = append(ops, r.op)
	}
	counts := map[string]int{}
	for _, op := range ops {
		counts[op]++
	}
	if counts["encrypt"] != 0 {
		t.Fatalf("rewrap flow must never call encrypt, got ops %v", ops)
	}
	if counts["rewrap"] != 1 {
		t.Fatalf("rewrap flow must call rewrap exactly once, got ops %v", ops)
	}
	if counts["decrypt"] != 2 {
		t.Fatalf("rewrap flow must decrypt-verify twice (pre + post), got ops %v", ops)
	}
	// Order: pre-verify decrypt, rewrap, post-verify decrypt.
	if len(ops) != 3 || ops[0] != "decrypt" || ops[1] != "rewrap" || ops[2] != "decrypt" {
		t.Fatalf("rewrap request order must be decrypt/rewrap/decrypt, got %v", ops)
	}
}

// TestReviewRewrapFailureLeavesFileUnchanged pins §7.1: a failed rewrap
// (here: revoked versions) leaves the file byte-identical with no new
// files of any kind.
func TestReviewRewrapFailureLeavesFileUnchanged(t *testing.T) {
	env := newFakeEnvelope("key-a")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{"providers":[{"id":"p","api_key":"rewrap-fail"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(enc, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	env.revokeAll()
	before, _ := os.ReadFile(path)
	// The §7.1 lock file is expected to exist after any call; compare
	// directory entries excluding it.
	nonLock := func() []string {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".lock") {
				names = append(names, e.Name())
			}
		}
		return names
	}
	entriesBefore := nonLock()
	if _, err := RewrapConfigFile(context.Background(), path, env); err == nil {
		t.Fatal("failed rewrap succeeded")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed rewrap modified the file")
	}
	if entriesAfter := nonLock(); len(entriesAfter) != len(entriesBefore) {
		t.Fatal("failed rewrap created files")
	}
}

// TestReviewGateOffMakesNoVaultCall pins §10.1: gate off with an nxs2
// fixture fails with the gate message and the fake server sees zero
// requests; conflicting configuration likewise fails before any call.
func TestReviewGateOffMakesNoVaultCall(t *testing.T) {
	fake := newFakeTransitServer()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	env := newFakeEnvelope("key-a")
	raw := []byte(`{"providers":[{"id":"p","api_key":"gate-canary"}]}`)
	enc, _, err := TransformConfigWith(context.Background(), Cipher{env: env}, raw, true)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, append(enc, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	set := func(kv map[string]string) {
		for _, name := range []string{"NEXAROUTE_KEY_PROVIDER", "NEXAROUTE_TRANSIT_ENABLED", "VAULT_ADDR", "VAULT_TOKEN", "NEXAROUTE_TRANSIT_MOUNT", "NEXAROUTE_TRANSIT_KEY", "NEXAROUTE_MASTER_KEY"} {
			t.Setenv(name, "")
		}
		for k, v := range kv {
			t.Setenv(k, v)
		}
	}
	set(map[string]string{
		"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "false",
		"VAULT_ADDR": srv.URL, "VAULT_TOKEN": "tok",
		"NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "nexaroute",
	})
	if _, err := LoadCipher(path); err == nil {
		t.Fatal("gate-off load succeeded")
	}
	set(map[string]string{
		"NEXAROUTE_KEY_PROVIDER": "transit", "NEXAROUTE_TRANSIT_ENABLED": "true",
		"VAULT_ADDR": srv.URL, "VAULT_TOKEN": "tok",
		"NEXAROUTE_TRANSIT_MOUNT": "transit", "NEXAROUTE_TRANSIT_KEY": "nexaroute",
		"NEXAROUTE_MASTER_KEY": "conflict",
	})
	if _, _, err := ConfiguredEnvelope(); err == nil {
		t.Fatal("conflicting configuration accepted")
	}
	if len(fake.requests) != 0 {
		t.Fatalf("gate-off/conflict paths made %d Vault calls", len(fake.requests))
	}
}
