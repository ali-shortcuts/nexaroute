package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Transit error classes (ADR 0007 §8.4). Gateway code, CLI output, and
// tests use these classes and never surface the raw Vault response body,
// the token, a DEK, or a wrapped DEK in an error string.
const (
	ClassTransitUnavailable            = "transit_unavailable"
	ClassTransitForbidden              = "transit_forbidden"
	ClassTransitKeyNotFound            = "transit_key_not_found"
	ClassTransitInvalidCiphertext      = "transit_invalid_ciphertext"
	ClassTransitKeyVersionUnavailable  = "transit_key_version_unavailable"
	ClassTransitBadResponse            = "transit_bad_response"
	ClassTransitConfigInvalid          = "transit_config_invalid"
	ClassTransitNotEnabled             = "transit_not_enabled"
)

// TransitError is a classified Transit failure. Class is one of the
// ClassTransit* constants; Op names the operation (encrypt/decrypt/rewrap
// or the configuration step); Message is operator-safe (no secret, no
// Vault body, no ciphertext).
type TransitError struct {
	Class   string
	Op      string
	Message string
}

func (e *TransitError) Error() string {
	if e.Op != "" {
		return e.Class + ": " + e.Op + ": " + e.Message
	}
	return e.Class + ": " + e.Message
}

func transitErrorf(class, op, format string, args ...any) *TransitError {
	return &TransitError{Class: class, Op: op, Message: fmt.Sprintf(format, args...)}
}

// Per-request timeout (ADR 0007 §8.3, normative). The overall operation
// budgets (30 s load/verify/save, 120 s migrate/rewrap) are enforced by
// the caller's context, not here.
const defaultTransitTimeout = 5 * time.Second

// maxTransitResponse caps Vault response bodies (ADR 0007 §8.2: 64 KiB).
const maxTransitResponse = 64 << 10

// maxTransitTimeout bounds the constructor timeout parameter. Selection
// (ConfiguredEnvelope) always passes defaultTransitTimeout.
const maxTransitTimeout = 60 * time.Second

// getenv is a package-level hook so deterministic tests can exercise
// configuration selection without mutating the process environment.
var getenv = os.Getenv

// VaultTransitProvider implements EnvelopeKeyProvider using the Vault
// Transit HTTP API. It never returns the Transit KEK and never falls back
// to a local key. The token is sent only in the X-Vault-Token header
// (plus X-Vault-Namespace when configured). No Transit `context`
// parameter is ever sent: for the pinned non-derived aes256-gcm96 key
// type Vault ignores it, so sending one would be false assurance
// (ADR 0007 §5.3). The data-GCM canonical AAD is the real binding.
type VaultTransitProvider struct {
	addr      *url.URL
	token     string
	namespace string
	mount     string
	key       string
	keyRef    string
	timeout   time.Duration
	client    *http.Client
}

// NewVaultTransitProvider validates configuration before any request is
// sent. The timeout is the per-request timeout; callers that select via
// ConfiguredEnvelope always pass defaultTransitTimeout (5 s).
func NewVaultTransitProvider(addr, token, namespace, mount, key string, timeout time.Duration) (*VaultTransitProvider, error) {
	u, err := parseTransitAddress(addr)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, &TransitError{Class: ClassTransitConfigInvalid, Message: "Vault Transit token is required (set VAULT_TOKEN or VAULT_TOKEN_FILE)"}
	}
	if err := ValidateTransitReference(namespace, mount, key); err != nil {
		return nil, &TransitError{Class: ClassTransitConfigInvalid, Message: err.Error()}
	}
	if timeout <= 0 {
		timeout = defaultTransitTimeout
	}
	if timeout > maxTransitTimeout {
		return nil, &TransitError{Class: ClassTransitConfigInvalid, Message: fmt.Sprintf("Vault Transit timeout must not exceed %s", maxTransitTimeout)}
	}
	return &VaultTransitProvider{
		addr:      u,
		token:     token,
		namespace: namespace,
		mount:     mount,
		key:       key,
		keyRef:    TransitKeyRefHash(namespace, mount, key),
		timeout:   timeout,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func parseTransitAddress(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("Vault Transit address is required (set VAULT_ADDR)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("Vault Transit address must be an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("Vault Transit address must use https (http is allowed only for loopback development)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Vault Transit address must not contain userinfo, query, or fragment")
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("Vault Transit address may use http only for loopback development")
		}
	}
	return u, nil
}

// KeyRefHash returns the full SHA-256 hex of the canonical key reference.
func (p *VaultTransitProvider) KeyRefHash() string { return p.keyRef }

// Close releases client resources. It retains no DEK, token, or wrapped
// DEK beyond the provider's own configuration lifetime.
func (p *VaultTransitProvider) Close() error { return nil }

type transitEncryptRequest struct {
	Plaintext string `json:"plaintext"`
}

type transitDecryptRequest struct {
	Ciphertext string `json:"ciphertext"`
}

type transitRewrapRequest struct {
	Ciphertext string `json:"ciphertext"`
}

type transitEncryptResponse struct {
	Data struct {
		Ciphertext string `json:"ciphertext"`
		KeyVersion int    `json:"key_version"`
	} `json:"data"`
}

type transitDecryptResponse struct {
	Data struct {
		Plaintext string `json:"plaintext"`
	} `json:"data"`
}

type transitRewrapResponse struct {
	Data struct {
		Ciphertext string `json:"ciphertext"`
		KeyVersion int    `json:"key_version"`
	} `json:"data"`
}

// WrapDEK encrypts a 32-byte DEK under the configured Transit key.
func (p *VaultTransitProvider) WrapDEK(ctx context.Context, dek []byte) (string, int, error) {
	if len(dek) != 32 {
		return "", 0, errors.New("Vault Transit requires a 32-byte data key")
	}
	var out transitEncryptResponse
	if err := p.do(ctx, "encrypt", transitEncryptRequest{
		Plaintext: base64.StdEncoding.EncodeToString(dek),
	}, &out); err != nil {
		return "", 0, err
	}
	version, err := ParseTransitCiphertextVersion(out.Data.Ciphertext)
	if err != nil {
		return "", 0, transitErrorf(ClassTransitBadResponse, "encrypt", "Vault Transit encrypt returned an invalid ciphertext")
	}
	if out.Data.KeyVersion != 0 && out.Data.KeyVersion != version {
		return "", 0, transitErrorf(ClassTransitBadResponse, "encrypt", "Vault Transit encrypt returned a key version inconsistent with its ciphertext")
	}
	if out.Data.KeyVersion == 0 {
		return "", 0, transitErrorf(ClassTransitBadResponse, "encrypt", "Vault Transit encrypt response is missing key_version")
	}
	return out.Data.Ciphertext, version, nil
}

// UnwrapDEK decrypts a Transit-wrapped DEK and returns exactly 32 bytes.
// The returned version is the version parsed from the wrapped ciphertext
// prefix (Vault decrypt responses carry no key_version field).
func (p *VaultTransitProvider) UnwrapDEK(ctx context.Context, wrapped string) ([]byte, int, error) {
	version, err := ParseTransitCiphertextVersion(wrapped)
	if err != nil {
		return nil, 0, err
	}
	var out transitDecryptResponse
	if err := p.do(ctx, "decrypt", transitDecryptRequest{
		Ciphertext: wrapped,
	}, &out); err != nil {
		return nil, 0, err
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil || len(dek) != 32 {
		return nil, 0, transitErrorf(ClassTransitBadResponse, "decrypt", "Vault Transit decrypt returned an invalid data key")
	}
	return dek, version, nil
}

// RewrapDEK moves a wrapped DEK to the latest Transit key version via the
// Transit rewrap endpoint only — never unwrap-plus-wrap.
func (p *VaultTransitProvider) RewrapDEK(ctx context.Context, wrapped string) (string, int, error) {
	if _, err := ParseTransitCiphertextVersion(wrapped); err != nil {
		return "", 0, err
	}
	var out transitRewrapResponse
	if err := p.do(ctx, "rewrap", transitRewrapRequest{
		Ciphertext: wrapped,
	}, &out); err != nil {
		return "", 0, err
	}
	version, err := ParseTransitCiphertextVersion(out.Data.Ciphertext)
	if err != nil {
		return "", 0, transitErrorf(ClassTransitBadResponse, "rewrap", "Vault Transit rewrap returned an invalid ciphertext")
	}
	if out.Data.KeyVersion != 0 && out.Data.KeyVersion != version {
		return "", 0, transitErrorf(ClassTransitBadResponse, "rewrap", "Vault Transit rewrap returned a key version inconsistent with its ciphertext")
	}
	if out.Data.KeyVersion == 0 {
		return "", 0, transitErrorf(ClassTransitBadResponse, "rewrap", "Vault Transit rewrap response is missing key_version")
	}
	return out.Data.Ciphertext, version, nil
}

// do performs one Transit operation with §8.3 retries: at most 2 retries,
// only for HTTP 429 and 503, backoff 100 ms then 300 ms plus jitter, and
// only within the caller's overall context budget. Timeouts and all other
// failures are not retried: a discarded encrypt is stateless, and decrypt
// and rewrap are idempotent for the same input, which is the retry
// justification. Vault response bodies are discarded, never logged, and
// never included in errors.
func (p *VaultTransitProvider) do(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return transitErrorf(ClassTransitBadResponse, op, "cannot serialize Vault Transit request")
	}
	backoffs := []time.Duration{100 * time.Millisecond, 300 * time.Millisecond}
	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			if ctx.Err() != nil {
				return transitErrorf(ClassTransitUnavailable, op, "Vault Transit request cancelled or overall operation budget exceeded")
			}
			jitter := time.Duration(rand.IntN(50)) * time.Millisecond
			select {
			case <-ctx.Done():
				return transitErrorf(ClassTransitUnavailable, op, "Vault Transit request cancelled or overall operation budget exceeded")
			case <-time.After(backoffs[attempt-1] + jitter):
			}
		}
		status, retryable, err := p.doOnce(ctx, op, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt == 2 {
			return err
		}
		_ = status
	}
	return lastErr
}

// doOnce performs a single HTTP round-trip. It returns (status, retryable,
// err): retryable is true only for 429/503 responses.
func (p *VaultTransitProvider) doOnce(ctx context.Context, op string, body []byte, out any) (int, bool, error) {
	u := *p.addr.JoinPath("v1", p.mount, op, p.key)
	reqCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, false, transitErrorf(ClassTransitBadResponse, op, "cannot build Vault Transit request")
	}
	req.Header.Set("X-Vault-Token", p.token)
	if p.namespace != "" {
		req.Header.Set("X-Vault-Namespace", p.namespace)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false, transitErrorf(ClassTransitUnavailable, op, "Vault Transit %s request failed (unreachable, TLS failure, or per-request timeout)", op)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTransitResponse+1))
	if err != nil {
		return 0, false, transitErrorf(ClassTransitBadResponse, op, "Vault Transit %s response could not be read", op)
	}
	if len(data) > maxTransitResponse {
		return 0, false, transitErrorf(ClassTransitBadResponse, op, "Vault Transit %s response exceeded the safe size limit", op)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(data, out); err != nil {
			return 200, false, transitErrorf(ClassTransitBadResponse, op, "Vault Transit %s returned a malformed response", op)
		}
		return 200, false, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return resp.StatusCode, false, transitErrorf(ClassTransitBadResponse, op, "Vault Transit %s was redirected; redirects are refused and the token is never forwarded", op)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, false, transitErrorf(ClassTransitForbidden, op, "Vault Transit %s denied (HTTP %d); check the token policy for encrypt/decrypt/rewrap on the configured key", op, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return resp.StatusCode, false, transitErrorf(ClassTransitKeyNotFound, op, "Vault Transit %s key not found (HTTP 404); check the mount/key/namespace configuration", op)
	case resp.StatusCode == http.StatusBadRequest:
		return resp.StatusCode, false, transitErrorf(ClassTransitInvalidCiphertext, op, "Vault Transit %s rejected the request (HTTP 400); if this is a decrypt, check the key-version checklist: rewrap status and min_decryption_version", op)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		return resp.StatusCode, true, transitErrorf(ClassTransitUnavailable, op, "Vault Transit %s temporarily unavailable (HTTP %d)", op, resp.StatusCode)
	case resp.StatusCode >= 500:
		return resp.StatusCode, false, transitErrorf(ClassTransitUnavailable, op, "Vault Transit %s failed (HTTP %d)", op, resp.StatusCode)
	default:
		return resp.StatusCode, false, transitErrorf(ClassTransitBadResponse, op, "Vault Transit %s failed (HTTP %d)", op, resp.StatusCode)
	}
}

// ConfiguredEnvelope returns the configured envelope-key provider
// (ADR 0007 §10). Selection is explicit and conflict-is-error: there is
// no fallback to a local key, no silent precedence winner, and no
// default mount/key. The feature gate NEXAROUTE_TRANSIT_ENABLED must be
// exactly "true"; any other set value is a configuration error, and
// gate-off with Transit selected is transit_not_enabled.
func ConfiguredEnvelope() (EnvelopeKeyProvider, bool, error) {
	provider := strings.TrimSpace(getenv("NEXAROUTE_KEY_PROVIDER"))
	gate := getenv("NEXAROUTE_TRANSIT_ENABLED")
	if provider != "transit" {
		if gate == "true" {
			return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "NEXAROUTE_TRANSIT_ENABLED=true requires NEXAROUTE_KEY_PROVIDER=transit; a lone gate flag must not silently do nothing"}
		}
		if gate != "" && gate != "false" {
			return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: `NEXAROUTE_TRANSIT_ENABLED must be exactly "true" or "false" when set`}
		}
		return nil, false, nil
	}
	if gate == "" || gate == "false" {
		return nil, false, &TransitError{Class: ClassTransitNotEnabled, Message: "Vault Transit is not enabled; set NEXAROUTE_TRANSIT_ENABLED=true to select Transit mode"}
	}
	if gate != "true" {
		return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: `NEXAROUTE_TRANSIT_ENABLED must be exactly "true" to enable Transit mode`}
	}
	for _, conflict := range []string{
		"NEXAROUTE_MASTER_KEY", "NEXAROUTE_MASTER_KEY_FILE",
		"NEXAROUTE_KEY_PROVIDER_ENV", "NEXAROUTE_KEY_PROVIDER_COMMAND",
		"NEXAROUTE_KEY_PROVIDER_REF",
	} {
		if strings.TrimSpace(getenv(conflict)) != "" {
			return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "NEXAROUTE_KEY_PROVIDER=transit conflicts with " + conflict + "; there is no precedence winner between a raw key and Transit"}
		}
	}
	tokenEnv := strings.TrimSpace(getenv("VAULT_TOKEN"))
	tokenFile := strings.TrimSpace(getenv("VAULT_TOKEN_FILE"))
	if tokenEnv != "" && tokenFile != "" {
		return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "set exactly one of VAULT_TOKEN and VAULT_TOKEN_FILE"}
	}
	var token string
	switch {
	case tokenEnv != "":
		token = tokenEnv
	case tokenFile != "":
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "cannot read VAULT_TOKEN_FILE"}
		}
		st, err := os.Stat(tokenFile)
		if err != nil || st.Mode().Perm() != 0600 {
			return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "VAULT_TOKEN_FILE must have mode exactly 0600"}
		}
		content := strings.TrimSuffix(string(b), "\n")
		if strings.TrimSpace(content) != content || content == "" {
			return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "VAULT_TOKEN_FILE must contain the token with only a trailing newline trimmed; other whitespace is rejected"}
		}
		token = content
	default:
		return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "Vault Transit requires VAULT_TOKEN or VAULT_TOKEN_FILE"}
	}
	addr := strings.TrimSpace(getenv("VAULT_ADDR"))
	mount := strings.TrimSpace(getenv("NEXAROUTE_TRANSIT_MOUNT"))
	key := strings.TrimSpace(getenv("NEXAROUTE_TRANSIT_KEY"))
	namespace := strings.TrimSpace(getenv("VAULT_NAMESPACE"))
	if addr == "" {
		return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "Vault Transit requires VAULT_ADDR"}
	}
	if mount == "" {
		return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "Vault Transit requires NEXAROUTE_TRANSIT_MOUNT"}
	}
	if key == "" {
		return nil, false, &TransitError{Class: ClassTransitConfigInvalid, Message: "Vault Transit requires NEXAROUTE_TRANSIT_KEY"}
	}
	p, err := NewVaultTransitProvider(addr, token, namespace, mount, key, defaultTransitTimeout)
	if err != nil {
		return nil, false, err
	}
	return p, true, nil
}

// Ensure compile-time interface conformance for the Vault provider.
var _ EnvelopeKeyProvider = (*VaultTransitProvider)(nil)
