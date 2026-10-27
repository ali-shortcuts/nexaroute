package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTransitMount   = "transit"
	defaultTransitKey     = "nexaroute"
	defaultTransitTimeout = 5 * time.Second
	maxTransitTimeout     = 60 * time.Second
	maxTransitResponse    = 1 << 20
)

var transitPathSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// getenv is a package-level hook so deterministic tests can exercise
// configuration selection without mutating the process environment.
var getenv = os.Getenv

// VaultTransitProvider implements EnvelopeKeyProvider using the Vault Transit
// HTTP API. It never returns the Transit KEK and never falls back to a local
// key. The token is sent only in the X-Vault-Token header.
type VaultTransitProvider struct {
	addr    *url.URL
	token   string
	mount   string
	key     string
	keyID   string
	timeout time.Duration
	client  *http.Client
}

// NewVaultTransitProvider validates configuration before any request is sent.
func NewVaultTransitProvider(addr, token, mount, key string, timeout time.Duration) (*VaultTransitProvider, error) {
	u, err := parseTransitAddress(addr)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Vault Transit token is required (set NEXAROUTE_TRANSIT_TOKEN)")
	}
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	if mount == "" {
		mount = defaultTransitMount
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = defaultTransitKey
	}
	if !transitPathSegment.MatchString(mount) || !transitPathSegment.MatchString(key) {
		return nil, errors.New("Vault Transit mount and key must be single path segments using only letters, digits, dot, underscore, or hyphen")
	}
	if timeout <= 0 {
		timeout = defaultTransitTimeout
	}
	if timeout > maxTransitTimeout {
		return nil, fmt.Errorf("Vault Transit timeout must not exceed %s", maxTransitTimeout)
	}
	return &VaultTransitProvider{
		addr:    u,
		token:   token,
		mount:   mount,
		key:     key,
		keyID:   VaultTransitKeyID(mount, key),
		timeout: timeout,
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
		return nil, errors.New("Vault Transit address is required (set NEXAROUTE_TRANSIT_ADDR)")
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

func transitTimeoutFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(getenv("NEXAROUTE_TRANSIT_TIMEOUT_MS"))
	if raw == "" {
		return defaultTransitTimeout, nil
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 0, errors.New("NEXAROUTE_TRANSIT_TIMEOUT_MS must be a positive integer")
	}
	d := time.Duration(ms) * time.Millisecond
	if d > maxTransitTimeout {
		return 0, fmt.Errorf("NEXAROUTE_TRANSIT_TIMEOUT_MS must not exceed %d", maxTransitTimeout/time.Millisecond)
	}
	return d, nil
}

// KeyID returns the full SHA-256 of the configured mount/key reference.
func (p *VaultTransitProvider) KeyID() string { return p.keyID }

type transitEncryptRequest struct {
	Plaintext      string `json:"plaintext"`
	AssociatedData string `json:"associated_data,omitempty"`
}

type transitDecryptRequest struct {
	Ciphertext     string `json:"ciphertext"`
	AssociatedData string `json:"associated_data,omitempty"`
}

type transitRewrapRequest struct {
	Ciphertext     string `json:"ciphertext"`
	AssociatedData string `json:"associated_data,omitempty"`
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
func (p *VaultTransitProvider) WrapDEK(ctx context.Context, dek, aad []byte) (string, error) {
	if len(dek) != 32 {
		return "", errors.New("Vault Transit requires a 32-byte data key")
	}
	var out transitEncryptResponse
	if err := p.do(ctx, "encrypt", transitEncryptRequest{
		Plaintext:      base64.StdEncoding.EncodeToString(dek),
		AssociatedData: encodeTransitAAD(aad),
	}, &out); err != nil {
		return "", err
	}
	if !validTransitCiphertext(out.Data.Ciphertext) {
		return "", errors.New("Vault Transit encrypt returned an invalid ciphertext")
	}
	return out.Data.Ciphertext, nil
}

// UnwrapDEK decrypts a Transit-wrapped DEK and returns exactly 32 bytes.
func (p *VaultTransitProvider) UnwrapDEK(ctx context.Context, wrapped string, aad []byte) ([]byte, error) {
	if !validTransitCiphertext(wrapped) {
		return nil, errors.New("invalid Vault Transit ciphertext")
	}
	var out transitDecryptResponse
	if err := p.do(ctx, "decrypt", transitDecryptRequest{
		Ciphertext:     wrapped,
		AssociatedData: encodeTransitAAD(aad),
	}, &out); err != nil {
		return nil, err
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil || len(dek) != 32 {
		return nil, errors.New("Vault Transit decrypt returned an invalid data key")
	}
	return dek, nil
}

// RewrapDEK moves a wrapped DEK to the latest Transit key version.
func (p *VaultTransitProvider) RewrapDEK(ctx context.Context, wrapped string, aad []byte) (string, error) {
	if !validTransitCiphertext(wrapped) {
		return "", errors.New("invalid Vault Transit ciphertext")
	}
	var out transitRewrapResponse
	if err := p.do(ctx, "rewrap", transitRewrapRequest{
		Ciphertext:     wrapped,
		AssociatedData: encodeTransitAAD(aad),
	}, &out); err != nil {
		return "", err
	}
	if !validTransitCiphertext(out.Data.Ciphertext) {
		return "", errors.New("Vault Transit rewrap returned an invalid ciphertext")
	}
	return out.Data.Ciphertext, nil
}

func encodeTransitAAD(aad []byte) string {
	if len(aad) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(aad)
}

func validTransitCiphertext(s string) bool {
	return strings.HasPrefix(s, "vault:v") && len(s) > len("vault:v1:")
}

func (p *VaultTransitProvider) do(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return errors.New("cannot serialize Vault Transit request")
	}
	u := *p.addr
	base := strings.TrimRight(u.Path, "/")
	u.Path = base + "/v1/" + url.PathEscape(p.mount) + "/" + op + "/" + url.PathEscape(p.key)
	u.RawPath = ""
	reqCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot build Vault Transit request")
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("Vault Transit %s request failed", op)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTransitResponse+1))
	if err != nil {
		return fmt.Errorf("Vault Transit %s response could not be read", op)
	}
	if len(data) > maxTransitResponse {
		return fmt.Errorf("Vault Transit %s response exceeded the safe size limit", op)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Vault Transit %s failed: HTTP %d", op, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Vault Transit %s returned a malformed response", op)
	}
	return nil
}
