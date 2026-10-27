package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// EnvelopePrefix identifies the nxs2 envelope format. Unlike nxs1, nxs2 does
// not require the key-encryption key (KEK) to enter the gateway process: only
// a short-lived data-encryption key (DEK) is wrapped and unwrapped through an
// EnvelopeKeyProvider such as Vault Transit.
const EnvelopePrefix = "nxs2:"

// EnvelopeKeyProvider wraps and unwraps per-secret data keys without exposing
// the underlying key-encryption key. Implementations must fail closed and must
// never log or return the KEK.
type EnvelopeKeyProvider interface {
	// KeyID is a stable, non-secret identifier for the configured key
	// reference. It is stored in every nxs2 envelope so a ciphertext cannot
	// be silently decrypted with a different Transit key reference.
	KeyID() string
	// WrapDEK wraps a 32-byte DEK. aad is bound by the provider when the
	// provider supports associated data.
	WrapDEK(ctx context.Context, dek, aad []byte) (string, error)
	// UnwrapDEK unwraps a provider ciphertext and returns exactly 32 bytes.
	UnwrapDEK(ctx context.Context, wrapped string, aad []byte) ([]byte, error)
	// RewrapDEK moves a provider ciphertext to the latest provider key
	// version without returning the DEK to the caller.
	RewrapDEK(ctx context.Context, wrapped string, aad []byte) (string, error)
}

type envelopeMeta struct {
	Version int    `json:"v"`
	KeyID   string `json:"kid"`
	Nonce   string `json:"nonce"`
	Wrapped string `json:"wrapped"`
	Data    string `json:"data"`
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

// KeyID returns the non-secret identifier recorded in new envelopes.
func (c Cipher) KeyID() string {
	if c.env != nil {
		return c.env.KeyID()
	}
	return c.rawID
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
func (c Cipher) Seal(aad, plain []byte) (string, error) {
	if c.env != nil {
		return sealEnvelope(context.Background(), c.env, aad, plain)
	}
	if len(c.raw) != 32 {
		return "", errors.New("invalid master key")
	}
	return seal(c.raw, aad, plain)
}

// Open decrypts a stored envelope under the selected cipher.
func (c Cipher) Open(aad []byte, text string) ([]byte, error) {
	if strings.HasPrefix(text, EnvelopePrefix) {
		if c.env == nil {
			return nil, errors.New("nxs2 envelope requires the Vault Transit envelope provider; configure NEXAROUTE_KEY_PROVIDER=vault-transit")
		}
		return openEnvelope(context.Background(), c.env, aad, text)
	}
	if c.env != nil {
		return nil, errors.New("nxs1 envelope cannot be read in Vault Transit mode; run the explicit nxs1-to-nxs2 migration with the legacy key")
	}
	return Open(c.raw, aad, text)
}

func sealEnvelope(ctx context.Context, env EnvelopeKeyProvider, aad, plain []byte) (string, error) {
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
	data := g.Seal(nil, nonce, plain, aad)
	wrapped, err := env.WrapDEK(ctx, dek, aad)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(wrapped) == "" {
		return "", errors.New("envelope provider returned an empty wrapped data key")
	}
	meta := envelopeMeta{
		Version: 2,
		KeyID:   env.KeyID(),
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Wrapped: wrapped,
		Data:    base64.RawURLEncoding.EncodeToString(data),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", errors.New("cannot serialize envelope metadata")
	}
	return EnvelopePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func openEnvelope(ctx context.Context, env EnvelopeKeyProvider, aad []byte, text string) ([]byte, error) {
	meta, err := parseEnvelope(text)
	if err != nil {
		return nil, err
	}
	if meta.KeyID != env.KeyID() {
		return nil, errors.New("nxs2 envelope key reference does not match the configured Transit key; restore the correct key reference")
	}
	nonce, err := decodeEnvelopeField(meta.Nonce)
	if err != nil || len(nonce) != 12 {
		return nil, errors.New("invalid nxs2 envelope nonce")
	}
	data, err := decodeEnvelopeField(meta.Data)
	if err != nil || len(data) == 0 {
		return nil, errors.New("invalid nxs2 envelope ciphertext")
	}
	dek, err := env.UnwrapDEK(ctx, meta.Wrapped, aad)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
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
	plain, err := g.Open(nil, nonce, data, aad)
	if err != nil {
		return nil, errors.New("nxs2 ciphertext authentication failed")
	}
	return plain, nil
}

func parseEnvelope(text string) (envelopeMeta, error) {
	var meta envelopeMeta
	if !strings.HasPrefix(text, EnvelopePrefix) {
		return meta, errors.New("invalid nxs2 envelope prefix")
	}
	encoded := strings.TrimPrefix(text, EnvelopePrefix)
	if len(encoded) > 1<<20 {
		return meta, errors.New("nxs2 envelope exceeds the safe size limit")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return meta, errors.New("invalid nxs2 envelope encoding")
	}
	if err = json.Unmarshal(raw, &meta); err != nil {
		return meta, errors.New("invalid nxs2 envelope metadata")
	}
	if meta.Version != 2 || meta.KeyID == "" || meta.Nonce == "" || meta.Wrapped == "" || meta.Data == "" {
		return meta, errors.New("invalid nxs2 envelope metadata")
	}
	return meta, nil
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

// TransformConfigWith changes secret-bearing JSON values in-place using the
// selected cipher. It is the envelope-aware counterpart of TransformConfig.
func TransformConfigWith(c Cipher, raw []byte, encrypt bool) ([]byte, int, error) {
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
				if c.env != nil && strings.HasPrefix(s, Prefix) {
					return nil, errors.New("nxs1 envelope cannot be re-encrypted under Vault Transit without the explicit legacy migration")
				}
				return s, nil
			}
			x, e := c.Seal(AAD(owner, field), []byte(s))
			if e != nil {
				return nil, e
			}
			n++
			return x, nil
		}
		if !IsEnvelope(s) {
			return s, nil
		}
		x, e := c.Open(AAD(owner, field), s)
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
// key version without decrypting the secret plaintext in the gateway.
func RewrapConfigBytes(ctx context.Context, env EnvelopeKeyProvider, raw []byte) ([]byte, int, error) {
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
		if strings.HasPrefix(s, Prefix) {
			return nil, errors.New("nxs1 envelope cannot be rewrapped; migrate it to nxs2 first")
		}
		if !strings.HasPrefix(s, EnvelopePrefix) {
			return s, nil
		}
		meta, e := parseEnvelope(s)
		if e != nil {
			return nil, e
		}
		if meta.KeyID != env.KeyID() {
			return nil, errors.New("nxs2 envelope key reference does not match the configured Transit key")
		}
		wrapped, e := env.RewrapDEK(ctx, meta.Wrapped, AAD(owner, field))
		if e != nil {
			return nil, e
		}
		meta.Wrapped = wrapped
		x, e := encodeEnvelope(meta)
		if e != nil {
			return nil, e
		}
		n++
		return x, nil
	}
	if err := walkSecretFields(root, visit); err != nil {
		return nil, n, err
	}
	b, err := json.MarshalIndent(root, "", "  ")
	return b, n, err
}

// MigrateConfigBytes converts a stored config to nxs2. nxs1 values require
// the explicit legacy raw key; there is no implicit conversion and no
// generated substitute key. Already-migrated configs are verified and
// returned unchanged.
func MigrateConfigBytes(ctx context.Context, raw []byte, legacyRaw []byte, env EnvelopeKeyProvider) ([]byte, int, error) {
	if env == nil {
		return nil, 0, errors.New("migration requires the Vault Transit envelope provider")
	}
	hasOld := strings.Contains(string(raw), Prefix)
	hasNew := strings.Contains(string(raw), EnvelopePrefix)
	if hasOld && hasNew {
		return nil, 0, errors.New("config contains mixed nxs1 and nxs2 envelopes; refusing to migrate")
	}
	if hasNew {
		if _, _, err := TransformConfigWith(Cipher{env: env}, raw, false); err != nil {
			return nil, 0, err
		}
		return raw, 0, nil
	}
	if hasOld {
		if len(legacyRaw) != 32 {
			return nil, 0, errors.New("migration requires the explicit 32-byte legacy master key for nxs1 envelopes")
		}
		plain, _, err := TransformConfig(raw, legacyRaw, false)
		if err != nil {
			return nil, 0, err
		}
		return TransformConfigWith(Cipher{env: env}, plain, true)
	}
	return TransformConfigWith(Cipher{env: env}, raw, true)
}

// EncryptedBackupWith wraps original config bytes in an authenticated
// encrypted blob using the selected cipher.
func EncryptedBackupWith(c Cipher, raw []byte) ([]byte, error) {
	v, err := c.Seal([]byte("nexaroute-backup-v1"), raw)
	if err != nil {
		return nil, err
	}
	return []byte(v + "\n"), nil
}

// DecryptBackupWith opens a backup written by EncryptedBackupWith.
func DecryptBackupWith(c Cipher, raw []byte) ([]byte, error) {
	return c.Open([]byte("nexaroute-backup-v1"), strings.TrimSpace(string(raw)))
}

// ConfiguredEnvelope returns the configured envelope-key provider. Selecting
// Vault Transit is an explicit opt-in; missing or malformed configuration is
// an error and never falls back to a local key.
func ConfiguredEnvelope() (EnvelopeKeyProvider, bool, error) {
	mode := strings.ToLower(strings.TrimSpace(getenv("NEXAROUTE_KEY_PROVIDER")))
	if mode != "vault-transit" && mode != "transit" {
		return nil, false, nil
	}
	timeout, err := transitTimeoutFromEnv()
	if err != nil {
		return nil, false, err
	}
	p, err := NewVaultTransitProvider(
		getenv("NEXAROUTE_TRANSIT_ADDR"),
		getenv("NEXAROUTE_TRANSIT_TOKEN"),
		getenv("NEXAROUTE_TRANSIT_MOUNT"),
		getenv("NEXAROUTE_TRANSIT_KEY"),
		timeout,
	)
	if err != nil {
		return nil, false, err
	}
	return p, true, nil
}

// VaultTransitKeyID is the non-secret identifier stored in nxs2 envelopes.
func VaultTransitKeyID(mount, key string) string {
	sum := sha256.Sum256([]byte(strings.Trim(mount, "/") + "/" + strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// Ensure compile-time interface conformance for the Vault provider.
var _ EnvelopeKeyProvider = (*VaultTransitProvider)(nil)
