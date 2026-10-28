package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// EnvelopePrefix identifies the nxs2 envelope format (ADR 0007, revised).
// Unlike nxs1, nxs2 does not require the key-encryption key (KEK) to enter
// the gateway process: only a short-lived data-encryption key (DEK) is
// wrapped and unwrapped through an EnvelopeKeyProvider such as Vault Transit.
// The KEK is never returned, stored, or logged by this package.
const EnvelopePrefix = "nxs2:"

// maxEnvelopeDataBytes mirrors the config size bound (16 MiB): a data
// ciphertext larger than this cannot occur inside a legal config file.
const maxEnvelopeDataBytes = 16 << 20

// maxEnvelopeOuterBytes bounds the outer base64 segment of one envelope.
// 16 MiB of data ciphertext base64-encodes to ~21.3 MiB inside the inner
// JSON, which base64-encodes to ~28.5 MiB outer; 32 MiB leaves margin.
const maxEnvelopeOuterBytes = 32 << 20

// EnvelopeKeyProvider wraps and unwraps per-secret data keys without exposing
// the underlying key-encryption key (ADR 0007 §4). Implementations must fail
// closed and must never log, store, or return KEK material (they never
// receive any). A Transit provider must NOT satisfy KeyProvider: there is
// no Load/Store returning raw key bytes, by type construction.
type EnvelopeKeyProvider interface {
	// KeyRefHash returns the full SHA-256 hex of the canonical key
	// reference (CanonicalTransitKeyRef). It is non-secret and contains
	// no key material.
	KeyRefHash() string
	// WrapDEK asks the provider to wrap a fresh DEK. It returns the
	// provider ciphertext and the key version the provider used. It never
	// returns, stores, or logs KEK material.
	WrapDEK(ctx context.Context, dek []byte) (string, int, error)
	// UnwrapDEK asks the provider to unwrap a wrapped DEK. It returns a
	// fresh DEK that the caller owns and must wipe immediately after the
	// data authentication check, on success and on failure. A successful
	// unwrap is NOT authentication of the field: the data GCM open is
	// the authentication step (ADR 0007 §5.2).
	// The returned version is the version parsed from the wrapped
	// ciphertext prefix.
	UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error)
	// RewrapDEK asks the provider to rewrap a wrapped DEK to the latest
	// key version, using the provider's rewrap endpoint only. It must
	// never be implemented as UnwrapDEK followed by WrapDEK.
	RewrapDEK(ctx context.Context, wrapped string) (string, int, error)
	// Close releases client resources. It must not log or retain DEKs,
	// tokens, or wrapped DEKs.
	Close() error
}

// envelopeMeta is the strict nxs2 inner JSON schema (ADR 0007 §3.2).
// Field names are normative; unknown fields and duplicate keys are
// rejected by parseEnvelope, not ignored.
type envelopeMeta struct {
	EnvelopeVersion   int    `json:"envelope_version"`
	Provider          string `json:"provider"`
	KeyRefSHA256      string `json:"key_ref_sha256"`
	TransitKeyVersion int    `json:"transit_key_version"`
	WrappedDEK        string `json:"wrapped_dek"`
	DataNonceB64URL   string `json:"data_nonce_b64url"`
	DataCiphertextB64 string `json:"data_ciphertext_b64url"`
}

// Cipher is the selected at-rest encryption path. A raw-key cipher produces
// nxs1 envelopes; an envelope-key cipher produces nxs2 envelopes. The zero
// value is unusable and fails closed.
type Cipher struct {
	raw   []byte
	rawID string
	env   EnvelopeKeyProvider
}

// EnvelopeEnabled reports whether this cipher uses an envelope-key provider.
func (c Cipher) EnvelopeEnabled() bool { return c.env != nil }

// KeyRefHash returns the non-secret key-reference hash recorded in new
// envelopes, or "" for a raw cipher (which has no Transit reference).
func (c Cipher) KeyRefHash() string {
	if c.env != nil {
		return c.env.KeyRefHash()
	}
	return ""
}

// LoadCipher selects the configured at-rest encryption path. When Vault
// Transit is selected there is no fallback to a local file or environment
// key: any configuration error is returned.
func LoadCipher(configPath string) (Cipher, error) {
	env, enabled, err := ConfiguredEnvelope()
	if err != nil {
		return Cipher{}, err
	}
	if enabled {
		return Cipher{env: env}, nil
	}
	key, id, err := Key(configPath)
	if err != nil {
		return Cipher{}, err
	}
	return Cipher{raw: key, rawID: id}, nil
}

// IsEnvelope reports whether s is a recognized encrypted envelope.
func IsEnvelope(s string) bool {
	return strings.HasPrefix(s, Prefix) || strings.HasPrefix(s, EnvelopePrefix)
}

// HasEnvelopes reports whether raw contains an nxs1 or nxs2 envelope.
func HasEnvelopes(raw []byte) bool {
	return strings.Contains(string(raw), Prefix) || strings.Contains(string(raw), EnvelopePrefix)
}

// Seal encrypts plain under the selected cipher and binds it to aad.
// This aad-based API is raw-only: an envelope cipher rejects it, because
// the nxs2 canonical AAD (ADR 0007 §5.1) includes the key reference and
// the per-envelope nonce, which are only known inside SealField. Use
// SealField for field encryption.
func (c Cipher) Seal(aad, plain []byte) (string, error) {
	if c.env != nil {
		return "", errors.New("envelope ciphers require SealField (canonical AAD); the aad-based Seal API is raw-only")
	}
	if len(c.raw) != 32 {
		return "", errors.New("invalid master key")
	}
	return seal(c.raw, aad, plain)
}

// Open decrypts a stored envelope under the selected cipher. This
// aad-based API is raw-only, mirroring Seal; use OpenField for fields.
func (c Cipher) Open(aad []byte, text string) ([]byte, error) {
	if c.env != nil {
		return nil, errors.New("envelope ciphers require OpenField (canonical AAD); the aad-based Open API is raw-only")
	}
	return Open(c.raw, aad, text)
}

// SealField encrypts plain for one secret-bearing field. For a raw cipher
// it binds AAD(owner, field); for an envelope cipher it binds the nxs2
// canonical AAD (ADR 0007 §5.1).
func (c Cipher) SealField(ctx context.Context, owner, field string, plain []byte) (string, error) {
	if c.env != nil {
		return sealEnvelope(ctx, c.env, owner, field, plain)
	}
	if len(c.raw) != 32 {
		return "", errors.New("invalid master key")
	}
	return seal(c.raw, AAD(owner, field), plain)
}

// OpenField decrypts one stored secret-bearing field. An nxs2 value under
// a raw cipher, an nxs1 value under an envelope cipher, and any unknown
// envelope all fail closed; plaintext is returned only by the transform
// layer, never by OpenField.
func (c Cipher) OpenField(ctx context.Context, owner, field, text string) ([]byte, error) {
	if strings.HasPrefix(text, EnvelopePrefix) {
		if c.env == nil {
			return nil, errors.New("nxs2 envelope requires the Vault Transit envelope provider; configure NEXAROUTE_KEY_PROVIDER=transit with NEXAROUTE_TRANSIT_ENABLED=true")
		}
		return openEnvelope(ctx, c.env, owner, field, text)
	}
	if c.env != nil {
		if strings.HasPrefix(text, Prefix) {
			return nil, errors.New("nxs1 envelope cannot be read in Vault Transit mode; run the explicit nxs1-to-nxs2 migration with the legacy key")
		}
		if ClassifyValue(text) == ClassUnknown {
			return nil, fmt.Errorf("unknown envelope in field %s:%s; refusing to read", owner, field)
		}
		return nil, fmt.Errorf("field %s:%s is not an envelope; plaintext secret fields fail closed in Vault Transit mode", owner, field)
	}
	if ClassifyValue(text) == ClassUnknown {
		return nil, fmt.Errorf("unknown envelope in field %s:%s; refusing to read", owner, field)
	}
	return Open(c.raw, AAD(owner, field), text)
}

// CanonicalTransitKeyRef builds the canonical key reference over which
// key_ref_sha256 is computed (ADR 0007 §3.3). It is defined over mount,
// namespace, and key — never over the Vault address, so it is stable
// across endpoint changes and key-version rotations.
func CanonicalTransitKeyRef(namespace, mount, key string) string {
	return "vault-transit" + "|" + "namespace=" + namespace + "|" + "mount=" + mount + "|" + "key=" + key
}

// TransitKeyRefHash returns hex(SHA-256(canonical reference)). The hash is
// a mismatch detector, not confidentiality: key names are low-entropy.
func TransitKeyRefHash(namespace, mount, key string) string {
	sum := sha256.Sum256([]byte(CanonicalTransitKeyRef(namespace, mount, key)))
	return hex.EncodeToString(sum[:])
}

var transitSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateTransitReference checks mount, key, and namespace per ADR 0007
// §3.3, before any provider call.
func ValidateTransitReference(namespace, mount, key string) error {
	if err := validateTransitPath(mount, true); err != nil {
		return fmt.Errorf("invalid Transit mount: %w", err)
	}
	if len(key) < 1 || len(key) > 128 || !transitSegment.MatchString(key) {
		return errors.New("invalid Transit key: a single segment of letters, digits, dot, underscore, or hyphen, length 1-128")
	}
	if namespace != "" {
		if err := validateTransitPath(namespace, true); err != nil {
			return fmt.Errorf("invalid Transit namespace: %w", err)
		}
	}
	return nil
}

func validateTransitPath(p string, allowMulti bool) error {
	if p == "" {
		return errors.New("empty path")
	}
	if len(p) > 255 {
		return errors.New("path exceeds 255 characters")
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return errors.New("path must not have a leading or trailing slash or an empty segment")
	}
	segments := strings.Split(p, "/")
	if !allowMulti && len(segments) != 1 {
		return errors.New("path must be a single segment")
	}
	for _, s := range segments {
		if s == "." || s == ".." || !transitSegment.MatchString(s) {
			return errors.New("path segments must use only letters, digits, dot, underscore, or hyphen, and must not be . or ..")
		}
	}
	return nil
}

// EnvelopeAAD builds the canonical nxs2 data authentication binding
// (ADR 0007 §5.1). Components are length-prefixed so no boundary can be
// confused by a component containing a separator. The wrapped DEK and the
// Transit key version are deliberately NOT in the AAD: rewrap changes
// both and must not invalidate data ciphertext (§5.2).
func EnvelopeAAD(keyRef, nonceB64, owner, field string) []byte {
	var b strings.Builder
	for _, c := range []string{"nxs2-binding-v1", "2", "vault-transit", keyRef, nonceB64, owner, field} {
		b.WriteString(strconv.Itoa(len(c)))
		b.WriteString(":")
		b.WriteString(c)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func sealEnvelope(ctx context.Context, env EnvelopeKeyProvider, owner, field string, plain []byte) (string, error) {
	dek := make([]byte, 32)
	if _, err := cryptoRead(dek); err != nil {
		return "", errors.New("cannot generate data key")
	}
	defer clear(dek)
	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err = cryptoRead(nonce); err != nil {
		return "", errors.New("cannot generate data nonce")
	}
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
	aad := EnvelopeAAD(env.KeyRefHash(), nonceB64, owner, field)
	data := g.Seal(nil, nonce, plain, aad)
	wrapped, version, err := env.WrapDEK(ctx, dek)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(wrapped) == "" {
		return "", errors.New("envelope provider returned an empty wrapped data key")
	}
	wrappedVersion, err := ParseTransitCiphertextVersion(wrapped)
	if err != nil {
		return "", fmt.Errorf("envelope provider returned an invalid wrapped data key: %w", err)
	}
	if wrappedVersion != version {
		return "", errors.New("envelope provider returned a key version inconsistent with its wrapped data key")
	}
	meta := envelopeMeta{
		EnvelopeVersion:   2,
		Provider:          "vault-transit",
		KeyRefSHA256:      env.KeyRefHash(),
		TransitKeyVersion: version,
		WrappedDEK:        wrapped,
		DataNonceB64URL:   nonceB64,
		DataCiphertextB64: base64.RawURLEncoding.EncodeToString(data),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", errors.New("cannot serialize envelope metadata")
	}
	return EnvelopePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func openEnvelope(ctx context.Context, env EnvelopeKeyProvider, owner, field, text string) ([]byte, error) {
	meta, err := parseEnvelope(text)
	if err != nil {
		return nil, err
	}
	if meta.KeyRefSHA256 != env.KeyRefHash() {
		return nil, &TransitError{Class: ClassTransitConfigInvalid, Op: "decrypt", Message: "nxs2 envelope key reference does not match the configured Transit key; check the mount/key/namespace configuration for field " + owner + ":" + field}
	}
	nonce, err := decodeEnvelopeField(meta.DataNonceB64URL)
	if err != nil || len(nonce) != 12 {
		return nil, errors.New("invalid nxs2 envelope nonce")
	}
	data, err := decodeEnvelopeField(meta.DataCiphertextB64)
	if err != nil || len(data) < 16 {
		return nil, errors.New("invalid nxs2 envelope ciphertext")
	}
	dek, version, err := env.UnwrapDEK(ctx, meta.WrappedDEK)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	if version != meta.TransitKeyVersion {
		return nil, errors.New("envelope provider returned a key version inconsistent with the envelope")
	}
	if len(dek) != 32 {
		return nil, errors.New("envelope provider returned an invalid data key length")
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, errors.New("invalid unwrapped data key")
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	aad := EnvelopeAAD(meta.KeyRefSHA256, meta.DataNonceB64URL, owner, field)
	plain, err := g.Open(nil, nonce, data, aad)
	if err != nil {
		return nil, errors.New("nxs2 ciphertext authentication failed")
	}
	return plain, nil
}

// parseEnvelope strictly parses an nxs2 envelope (ADR 0007 §3.1-§3.4).
// Unknown JSON fields, duplicate JSON keys, trailing data, non-strict
// base64, and any wrapped-DEK shape deviation are all parse failures.
func parseEnvelope(text string) (envelopeMeta, error) {
	var meta envelopeMeta
	if !strings.HasPrefix(text, EnvelopePrefix) {
		return meta, errors.New("invalid nxs2 envelope prefix")
	}
	encoded := strings.TrimPrefix(text, EnvelopePrefix)
	if len(encoded) > maxEnvelopeOuterBytes {
		return meta, errors.New("nxs2 envelope exceeds the safe size limit")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return meta, errors.New("invalid nxs2 envelope encoding")
	}
	fields, err := decodeStrictObject(raw)
	if err != nil {
		return meta, err
	}
	want := map[string]bool{
		"envelope_version": true, "provider": true, "key_ref_sha256": true,
		"transit_key_version": true, "wrapped_dek": true,
		"data_nonce_b64url": true, "data_ciphertext_b64url": true,
	}
	if len(fields) != len(want) {
		return meta, errors.New("invalid nxs2 envelope metadata: unexpected or missing fields")
	}
	for name := range fields {
		if !want[name] {
			return meta, errors.New("invalid nxs2 envelope metadata: unknown field " + name)
		}
	}
	unmarshalField := func(name string, dst any) error {
		if err := json.Unmarshal(fields[name], dst); err != nil {
			return fmt.Errorf("invalid nxs2 envelope metadata: field %s: %w", name, err)
		}
		return nil
	}
	if err := unmarshalField("envelope_version", &meta.EnvelopeVersion); err != nil {
		return meta, err
	}
	if err := unmarshalField("provider", &meta.Provider); err != nil {
		return meta, err
	}
	if err := unmarshalField("key_ref_sha256", &meta.KeyRefSHA256); err != nil {
		return meta, err
	}
	if err := unmarshalField("transit_key_version", &meta.TransitKeyVersion); err != nil {
		return meta, err
	}
	if err := unmarshalField("wrapped_dek", &meta.WrappedDEK); err != nil {
		return meta, err
	}
	if err := unmarshalField("data_nonce_b64url", &meta.DataNonceB64URL); err != nil {
		return meta, err
	}
	if err := unmarshalField("data_ciphertext_b64url", &meta.DataCiphertextB64); err != nil {
		return meta, err
	}
	if meta.EnvelopeVersion != 2 {
		return meta, errors.New("invalid nxs2 envelope metadata: envelope_version must be 2")
	}
	if meta.Provider != "vault-transit" {
		return meta, errors.New("invalid nxs2 envelope metadata: provider must be vault-transit")
	}
	if len(meta.KeyRefSHA256) != 64 || !isLowerHex(meta.KeyRefSHA256) {
		return meta, errors.New("invalid nxs2 envelope metadata: key_ref_sha256 must be 64 lowercase hex characters")
	}
	if meta.TransitKeyVersion < 1 {
		return meta, errors.New("invalid nxs2 envelope metadata: transit_key_version must be at least 1")
	}
	wrappedVersion, err := ParseTransitCiphertextVersion(meta.WrappedDEK)
	if err != nil {
		return meta, fmt.Errorf("invalid nxs2 envelope metadata: wrapped_dek: %w", err)
	}
	if wrappedVersion != meta.TransitKeyVersion {
		return meta, errors.New("invalid nxs2 envelope metadata: transit_key_version does not match the wrapped data key version")
	}
	nonce, err := decodeEnvelopeField(meta.DataNonceB64URL)
	if err != nil || len(nonce) != 12 {
		return meta, errors.New("invalid nxs2 envelope metadata: data nonce must decode to exactly 12 bytes")
	}
	data, err := decodeEnvelopeField(meta.DataCiphertextB64)
	if err != nil || len(data) < 16 || len(data) > maxEnvelopeDataBytes {
		return meta, errors.New("invalid nxs2 envelope metadata: data ciphertext must decode to 16 bytes-16 MiB")
	}
	return meta, nil
}

// decodeStrictObject decodes a JSON object while rejecting duplicate keys
// and trailing data. encoding/json alone silently takes the last duplicate,
// which is not acceptable for this format (ADR 0007 §3.2).
func decodeStrictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid nxs2 envelope metadata: not a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("invalid nxs2 envelope metadata: malformed object")
		}
		name, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid nxs2 envelope metadata: non-string key")
		}
		if _, dup := fields[name]; dup {
			return nil, errors.New("invalid nxs2 envelope metadata: duplicate field " + name)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errors.New("invalid nxs2 envelope metadata: malformed value")
		}
		fields[name] = v
	}
	tok, err = dec.Token()
	if err != nil || tok != json.Delim('}') {
		return nil, errors.New("invalid nxs2 envelope metadata: malformed object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid nxs2 envelope metadata: trailing data after object")
	}
	return fields, nil
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// ParseTransitCiphertextVersion strictly parses a Vault Transit ciphertext
// (ADR 0007 §3.4): exactly "vault:v<version>:<base64>", version ≥ 1 with no
// leading zeros and no sign, tail non-empty standard base64 with a strict
// re-encode check, at most 4096 bytes of ciphertext text.
func ParseTransitCiphertextVersion(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 || parts[0] != "vault" {
		return 0, errors.New("Transit ciphertext must have exactly three colon-separated parts starting with vault")
	}
	vpart := parts[1]
	if len(vpart) < 2 || vpart[0] != 'v' {
		return 0, errors.New("Transit ciphertext version must have the form v<decimal>")
	}
	digits := vpart[1:]
	if len(digits) > 1 && digits[0] == '0' {
		return 0, errors.New("Transit ciphertext version must not have leading zeros")
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, errors.New("Transit ciphertext version must be decimal")
		}
	}
	v, err := strconv.Atoi(digits)
	if err != nil || v < 1 {
		return 0, errors.New("Transit ciphertext version must be at least 1")
	}
	if len(parts[2]) == 0 || len(parts[2]) > 4096 {
		return 0, errors.New("Transit ciphertext body must be non-empty and at most 4096 bytes")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || base64.StdEncoding.EncodeToString(raw) != parts[2] {
		return 0, errors.New("Transit ciphertext body must be strict standard base64")
	}
	return v, nil
}

func encodeEnvelope(meta envelopeMeta) (string, error) {
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", errors.New("cannot serialize envelope metadata")
	}
	return EnvelopePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeEnvelopeField(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("invalid nxs2 envelope field encoding")
	}
	return b, nil
}

// EnvelopeClass classifies one secret-bearing value (ADR 0007 §6).
type EnvelopeClass int

const (
	// ClassEmpty is an empty value (no secret stored).
	ClassEmpty EnvelopeClass = iota
	// ClassPlaintext is a non-empty value that is not an envelope.
	ClassPlaintext
	// ClassNXS1 is a raw-key envelope.
	ClassNXS1
	// ClassNXS2 is a well-formed Transit envelope.
	ClassNXS2
	// ClassUnknown matches nxs<digit>: values that are not a known,
	// supported version, and nxs2 values that fail strict parsing.
	ClassUnknown
)

var unknownEnvelopePrefix = regexp.MustCompile(`^nxs[0-9]+:`)

// ClassifyValue classifies one value. An nxs2 value that fails strict
// parsing is ClassUnknown, never plaintext: it must fail closed,
// never be re-sealed or skipped.
func ClassifyValue(s string) EnvelopeClass {
	if s == "" {
		return ClassEmpty
	}
	if strings.HasPrefix(s, Prefix) {
		return ClassNXS1
	}
	if strings.HasPrefix(s, EnvelopePrefix) {
		if _, err := parseEnvelope(s); err != nil {
			return ClassUnknown
		}
		return ClassNXS2
	}
	if unknownEnvelopePrefix.MatchString(s) {
		return ClassUnknown
	}
	return ClassPlaintext
}

// ConfigClassification counts secret-bearing fields by class and names the
// offending fields for mixed-format and unknown-envelope errors.
type ConfigClassification struct {
	NXS1            int
	NXS2            int
	Plaintext       int
	Unknown         int
	NXS1Fields      []string
	NXS2Fields      []string
	PlaintextFields []string
	UnknownFields   []string
}

// ClassifyConfig classifies every secret-bearing field of a config
// document. Empty values are not counted.
func ClassifyConfig(raw []byte) (ConfigClassification, error) {
	var out ConfigClassification
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return out, err
	}
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return v, nil
		}
		name := owner + ":" + field
		switch ClassifyValue(s) {
		case ClassNXS1:
			out.NXS1++
			out.NXS1Fields = append(out.NXS1Fields, name)
		case ClassNXS2:
			out.NXS2++
			out.NXS2Fields = append(out.NXS2Fields, name)
		case ClassPlaintext:
			out.Plaintext++
			out.PlaintextFields = append(out.PlaintextFields, name)
		case ClassUnknown:
			out.Unknown++
			out.UnknownFields = append(out.UnknownFields, name)
		}
		return v, nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return out, err
	}
	return out, nil
}

// SecretFieldMap returns owner:field -> stored string value for every
// secret-bearing field of a config document. It is used by the Transit
// save path to preserve unchanged nxs2 envelopes byte-for-byte.
func SecretFieldMap(raw []byte) (map[string]string, error) {
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	visit := func(owner, field string, v any) (any, error) {
		if s, ok := v.(string); ok {
			out[owner+":"+field] = s
		}
		return v, nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return nil, err
	}
	return out, nil
}

// SubstituteSecretFields rewrites secret-bearing fields of a config
// document using fn(owner, field, currentValue) -> newValue.
func SubstituteSecretFields(raw []byte, fn func(owner, field, current string) (string, error)) ([]byte, error) {
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return nil, err
	}
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok {
			return v, nil
		}
		return fn(owner, field, s)
	}
	if err := walkSecretFields(root, visit); err != nil {
		return nil, err
	}
	return json.MarshalIndent(root, "", "  ")
}

// TransformConfigWith changes secret-bearing JSON values in-place using the
// selected cipher. It is the envelope-aware counterpart of TransformConfig.
// Mixed nxs1+nxs2, unknown envelopes, and (in Transit mode) plaintext
// secret fields all fail closed; nothing is partially transformed.
func TransformConfigWith(ctx context.Context, c Cipher, raw []byte, encrypt bool) ([]byte, int, error) {
	cls, err := ClassifyConfig(raw)
	if err != nil {
		return nil, 0, err
	}
	if cls.Unknown > 0 {
		return nil, 0, fmt.Errorf("config contains unknown envelope(s) in field(s) %s; refusing to transform", strings.Join(cls.UnknownFields, ", "))
	}
	if cls.NXS1 > 0 && cls.NXS2 > 0 {
		return nil, 0, fmt.Errorf("config contains mixed nxs1 and nxs2 envelopes in field(s) %s; refusing to transform (migrate explicitly)", strings.Join(append(append([]string{}, cls.NXS1Fields...), cls.NXS2Fields...), ", "))
	}
	if c.env != nil && cls.NXS1 > 0 {
		return nil, 0, errors.New("nxs1 envelope cannot be transformed in Vault Transit mode; run the explicit nxs1-to-nxs2 migration with the legacy key")
	}
	if c.env == nil && cls.NXS2 > 0 {
		return nil, 0, errors.New("nxs2 envelope requires the Vault Transit envelope provider; configure NEXAROUTE_KEY_PROVIDER=transit with NEXAROUTE_TRANSIT_ENABLED=true")
	}
	if c.env != nil && !encrypt && cls.Plaintext > 0 {
		return nil, 0, fmt.Errorf("config contains plaintext secret field(s) %s in Vault Transit mode; seal them under nxs1 first, then run the explicit migration (plaintext is never auto-migrated into Transit)", strings.Join(cls.PlaintextFields, ", "))
	}
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return nil, 0, err
	}
	n := 0
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return v, nil
		}
		if encrypt {
			if IsEnvelope(s) {
				return s, nil
			}
			x, e := c.SealField(ctx, owner, field, []byte(s))
			if e != nil {
				return nil, e
			}
			n++
			return x, nil
		}
		if !IsEnvelope(s) {
			return s, nil
		}
		x, e := c.OpenField(ctx, owner, field, s)
		if e != nil {
			return nil, e
		}
		n++
		return string(x), nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return nil, n, err
	}
	b, err := json.MarshalIndent(root, "", "  ")
	return b, n, err
}

// RewrapConfigBytes moves every nxs2 wrapped data key to the latest provider
// key version without decrypting the secret plaintext in the gateway
// (ADR 0007 §7.3). The full flow runs in memory: decrypt-verify the old
// config, rewrap each field via the provider rewrap endpoint only,
// decrypt-verify the new config against the old plaintext. Fields already
// at the latest version are left byte-identical. The returned bool reports
// whether every field was already at the latest version (no change).
// A field that no longer decrypts aborts before any rewrap call.
func RewrapConfigBytes(ctx context.Context, env EnvelopeKeyProvider, raw []byte) ([]byte, int, bool, error) {
	if env == nil {
		return nil, 0, false, errors.New("rewrap requires the Vault Transit envelope provider")
	}
	cls, err := ClassifyConfig(raw)
	if err != nil {
		return nil, 0, false, err
	}
	if cls.Unknown > 0 {
		return nil, 0, false, fmt.Errorf("config contains unknown envelope(s) in field(s) %s; refusing to rewrap", strings.Join(cls.UnknownFields, ", "))
	}
	if cls.NXS1 > 0 || cls.Plaintext > 0 {
		return nil, 0, false, errors.New("rewrap requires a config containing only nxs2 envelopes; migrate nxs1/plaintext first")
	}
	plainOld, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, false)
	if err != nil {
		return nil, 0, false, fmt.Errorf("rewrap pre-verification failed: %w", err)
	}
	root, err := decodeConfigRoot(raw)
	if err != nil {
		return nil, 0, false, err
	}
	n := 0
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return v, nil
		}
		meta, e := parseEnvelope(s)
		if e != nil {
			return nil, e
		}
		if meta.KeyRefSHA256 != env.KeyRefHash() {
			return nil, errors.New("nxs2 envelope key reference does not match the configured Transit key")
		}
		wrapped, _, e := env.RewrapDEK(ctx, meta.WrappedDEK)
		if e != nil {
			return nil, e
		}
		if wrapped == meta.WrappedDEK {
			return s, nil
		}
		version, e := ParseTransitCiphertextVersion(wrapped)
		if e != nil {
			return nil, fmt.Errorf("rewrap returned an invalid wrapped data key: %w", e)
		}
		meta.WrappedDEK = wrapped
		meta.TransitKeyVersion = version
		x, e := encodeEnvelope(meta)
		if e != nil {
			return nil, e
		}
		n++
		return x, nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return nil, 0, false, err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, 0, false, err
	}
	if n == 0 {
		return out, 0, true, nil
	}
	plainNew, _, err := TransformConfigWith(ctx, Cipher{env: env}, out, false)
	if err != nil {
		return nil, 0, false, fmt.Errorf("rewrap post-verification failed: %w", err)
	}
	if !bytes.Equal(plainOld, plainNew) {
		return nil, 0, false, errors.New("rewrap post-verification failed: rewrapped config does not decrypt to the original plaintext")
	}
	return out, n, false, nil
}

// MigrateConfigBytes converts a stored config to nxs2 (ADR 0007 §7.2).
// nxs1 values require the explicit legacy raw key; there is no implicit
// conversion and no generated substitute key. Already-migrated configs are
// verified and returned unchanged. Plaintext secret fields fail: they must
// be sealed under nxs1 first, never auto-migrated into Transit. Mixed
// formats and unknown envelopes fail closed. The returned count is the
// number of migrated fields (0 for already-migrated or field-less configs).
func MigrateConfigBytes(ctx context.Context, raw []byte, legacyRaw []byte, env EnvelopeKeyProvider) ([]byte, int, error) {
	if env == nil {
		return nil, 0, errors.New("migration requires the Vault Transit envelope provider")
	}
	cls, err := ClassifyConfig(raw)
	if err != nil {
		return nil, 0, err
	}
	if cls.Unknown > 0 {
		return nil, 0, fmt.Errorf("config contains unknown envelope(s) in field(s) %s; refusing to migrate", strings.Join(cls.UnknownFields, ", "))
	}
	if cls.NXS1 > 0 && cls.NXS2 > 0 {
		return nil, 0, errors.New("config contains mixed nxs1 and nxs2 envelopes; refusing to migrate")
	}
	if cls.Plaintext > 0 {
		return nil, 0, fmt.Errorf("config contains plaintext secret field(s) %s; seal them under nxs1 first, then migrate (plaintext is never auto-migrated into Transit)", strings.Join(cls.PlaintextFields, ", "))
	}
	if cls.NXS2 > 0 {
		if _, _, err := TransformConfigWith(ctx, Cipher{env: env}, raw, false); err != nil {
			return nil, 0, err
		}
		return raw, 0, nil
	}
	if cls.NXS1 == 0 {
		return raw, 0, nil
	}
	if len(legacyRaw) != 32 {
		return nil, 0, errors.New("migration requires the explicit 32-byte legacy master key for nxs1 envelopes")
	}
	plain, _, err := TransformConfig(raw, legacyRaw, false)
	if err != nil {
		return nil, 0, err
	}
	if err := ProveTransitConnectivity(ctx, env); err != nil {
		return nil, 0, err
	}
	out, n, err := TransformConfigWith(ctx, Cipher{env: env}, plain, true)
	if err != nil {
		return nil, 0, err
	}
	verify, _, err := TransformConfigWith(ctx, Cipher{env: env}, out, false)
	if err != nil {
		return nil, 0, fmt.Errorf("migration in-memory verification failed: %w", err)
	}
	if !bytes.Equal(plain, verify) {
		return nil, 0, errors.New("migration in-memory verification failed: migrated config does not decrypt to the original plaintext")
	}
	return out, n, nil
}

// ProveTransitConnectivity wraps and unwraps a fresh random canary DEK and
// wipes it. It proves Transit works without touching any config (ADR 0007
// §7.2 step 3). The canary DEK is never stored.
func ProveTransitConnectivity(ctx context.Context, env EnvelopeKeyProvider) error {
	dek := make([]byte, 32)
	if _, err := cryptoRead(dek); err != nil {
		return errors.New("cannot generate Transit connectivity canary")
	}
	defer clear(dek)
	wrapped, _, err := env.WrapDEK(ctx, dek)
	if err != nil {
		return fmt.Errorf("Transit connectivity check failed on wrap: %w", err)
	}
	back, _, err := env.UnwrapDEK(ctx, wrapped)
	if err != nil {
		return fmt.Errorf("Transit connectivity check failed on unwrap: %w", err)
	}
	defer clear(back)
	if !bytes.Equal(dek, back) {
		return errors.New("Transit connectivity check failed: canary round-trip mismatch")
	}
	return nil
}

// EncryptedBackupWith wraps original config bytes in an authenticated
// encrypted blob using the selected cipher. This sealed-backup format is
// raw-only: Transit backups are verbatim pre-operation bytes (ADR 0007
// §7.2/§7.6), because a Transit-sealed backup would depend on Transit
// availability and would not be a rollback.
func EncryptedBackupWith(c Cipher, raw []byte) ([]byte, error) {
	if c.env != nil {
		return nil, errors.New("Transit mode does not use sealed backups; migration and rewrap write verbatim pre-operation backups")
	}
	v, err := c.Seal([]byte("nexaroute-backup-v1"), raw)
	if err != nil {
		return nil, err
	}
	return []byte(v + "\n"), nil
}

// DecryptBackupWith opens a backup written by EncryptedBackupWith.
func DecryptBackupWith(c Cipher, raw []byte) ([]byte, error) {
	if c.env != nil {
		return nil, errors.New("Transit mode does not use sealed backups; migration and rewrap write verbatim pre-operation backups")
	}
	return c.Open([]byte("nexaroute-backup-v1"), strings.TrimSpace(string(raw)))
}
