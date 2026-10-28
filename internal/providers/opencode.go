package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// OpenCode upstream profiles. Console is pay-as-you-go; Go is the
// subscription gateway. Both speak the same three wire protocols, but
// only Console documents a models-listing endpoint, and only Go
// requires the x-opencode-session header. There is no Zen profile:
// no Zen URL appears in the documented Console/Go material.

// IsOpencodeType reports whether a provider type is an OpenCode upstream.
func IsOpencodeType(t string) bool {
	return t == "opencode-console" || t == "opencode-go"
}

// Ingress kinds for opencode path selection. The upstream protocol is
// selected by the CLIENT ingress kind, never by model id or by the
// generic /v1/... defaults.
const (
	IngressOpenAIChat      = "openai_chat"
	IngressOpenAIResponses = "openai_responses"
	IngressAnthropic       = "anthropic"
)

// OpencodePathForIngress maps (opencode type, ingress kind) to the single
// documented upstream path. It never reads the generic defaults.
func OpencodePathForIngress(kind string) (string, bool) {
	switch kind {
	case IngressOpenAIChat:
		return config.OpencodeChatPath, true
	case IngressOpenAIResponses:
		return config.OpencodeResponsesPath, true
	case IngressAnthropic:
		return config.OpencodeMessagesPath, true
	}
	return "", false
}

// OpencodeCanonicalKind maps an ingress kind to the canonical upstream
// kind the response decoders already understand.
func OpencodeCanonicalKind(kind string) string {
	switch kind {
	case IngressOpenAIResponses:
		return "openai_responses"
	case IngressAnthropic:
		return "anthropic"
	default:
		return "openai_chat"
	}
}

// ---------------------------------------------------------------------------
// Session context (Go header + credential-level session affinity).
//
// The context carries the gateway session key exactly as the router
// derived it from client-supplied session headers/body (bounded to 256
// chars by the extractor). It is never derived from an API key, email,
// prompt text, user id, or x-request-id. The adapter hashes it before
// using it as a map key or a header input, and never logs it.
// ---------------------------------------------------------------------------

type sessionKeyContextKey struct{}

// WithSessionKey attaches the router-derived session key to a dispatch
// context. Mirrors WithQuotaEstimate.
func WithSessionKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sessionKeyContextKey{}, key)
}

// SessionKeyFromContext returns the session key when present and non-empty.
func SessionKeyFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(sessionKeyContextKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// SessionHash is the SHA-256 hex of a session key: the only form ever
// used as a map key for credential-level session affinity.
func SessionHash(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Derived x-opencode-session header (Go profile ONLY).
//
// Format: "ses_" + first 32 hex chars of SHA-256(sessionKey + "|" + salt)
// for a live session, or SHA-256(providerID + "|" + model + "|" + salt)
// as the deterministic fallback when no session key exists. The salt is
// the operator-configured OpencodeSessionSalt or the documented default;
// it is a domain separator, never a credential. Client-supplied
// x-opencode-session values (via ForwardHeaders) always win unchanged.
// Console never receives a gateway-derived header.
// ---------------------------------------------------------------------------

func opencodeSessionSalt(p config.ProviderConfig) string {
	if strings.TrimSpace(p.OpencodeSessionSalt) != "" {
		return p.OpencodeSessionSalt
	}
	return config.DefaultOpencodeSessionSalt
}

// DeriveOpencodeSessionValue computes the header value without caching.
// DeriveOpencodeSessionValueCached is the dispatch path; it caches by the
// SHA-256 of the derivation input, never by the raw session key.
func DeriveOpencodeSessionValue(p config.ProviderConfig, sessionKey, model string) string {
	salt := opencodeSessionSalt(p)
	var input string
	if sessionKey != "" {
		input = sessionKey + "|" + salt
	} else {
		input = p.ID + "|" + model + "|" + salt
	}
	sum := sha256.Sum256([]byte(input))
	return "ses_" + hex.EncodeToString(sum[:])[:32]
}

type sessionValueCacheEntry struct {
	value   string
	expires time.Time
}

type sessionValueCache struct {
	mu      sync.Mutex
	entries map[string]sessionValueCacheEntry
	order   []string
}

const (
	sessionValueCacheMax = 1024
	sessionValueCacheTTL = time.Hour
)

var opencodeSessionCache = &sessionValueCache{entries: map[string]sessionValueCacheEntry{}}

func DeriveOpencodeSessionValueCached(p config.ProviderConfig, sessionKey, model string) string {
	salt := opencodeSessionSalt(p)
	var input string
	if sessionKey != "" {
		input = sessionKey + "|" + salt
	} else {
		input = p.ID + "|" + model + "|" + salt
	}
	keySum := sha256.Sum256([]byte(input))
	key := hex.EncodeToString(keySum[:])
	now := time.Now()
	opencodeSessionCache.mu.Lock()
	defer opencodeSessionCache.mu.Unlock()
	if e, ok := opencodeSessionCache.entries[key]; ok && now.Before(e.expires) {
		return e.value
	}
	value := DeriveOpencodeSessionValue(p, sessionKey, model)
	if len(opencodeSessionCache.entries) >= sessionValueCacheMax {
		// Evict expired entries first, then the oldest insertion.
		for k, e := range opencodeSessionCache.entries {
			if !now.Before(e.expires) {
				delete(opencodeSessionCache.entries, k)
			}
		}
		for len(opencodeSessionCache.entries) >= sessionValueCacheMax && len(opencodeSessionCache.order) > 0 {
			oldest := opencodeSessionCache.order[0]
			opencodeSessionCache.order = opencodeSessionCache.order[1:]
			delete(opencodeSessionCache.entries, oldest)
		}
	}
	opencodeSessionCache.entries[key] = sessionValueCacheEntry{value: value, expires: now.Add(sessionValueCacheTTL)}
	opencodeSessionCache.order = append(opencodeSessionCache.order, key)
	return value
}

// modelFromPayload extracts the "model" field best-effort from a JSON
// payload for the session fallback. Empty on any parse failure.
func modelFromPayload(payload []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// ---------------------------------------------------------------------------
// Redacted per-credential state + email masking.
// ---------------------------------------------------------------------------

// MaskEmail renders an address in masked form only: first character of
// the local part, ellipsis, and the full domain. Unparseable values
// collapse to the placeholder. Never returns the full address.
func MaskEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at >= len(email)-1 {
		return "••••"
	}
	return email[:1] + "•••@" + email[at+1:]
}

// CredentialState is the redacted live state of one adapter credential.
// It carries no key material and no email address.
type CredentialState struct {
	Index         int       `json:"index"`
	PoolIndex     int       `json:"pool_index"`
	Name          string    `json:"name"`
	Enabled       bool      `json:"enabled"`
	Priority      int       `json:"priority"`
	Weight        float64   `json:"weight"`
	LastStatus    int       `json:"last_status"`
	Failures      int       `json:"failures"`
	Active        int64     `json:"active"`
	Cooling       bool      `json:"cooling"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
}

// CredentialSelection explains the most recent credential choice on an
// adapter: which tier won, whether session affinity pinned it, or that
// it was the least-loaded pick. No key material.
type CredentialSelection struct {
	Index     int    `json:"index"`
	PoolIndex int    `json:"pool_index"`
	Name      string `json:"name"`
	Tier      int    `json:"tier"`
	Reason    string `json:"reason"` // "affinity" | "least-loaded" | "only-available"
}

// CredentialStateProvider is the optional adapter surface for redacted
// per-credential state. It is deliberately not part of Adapter so test
// fakes implementing Adapter keep compiling.
type CredentialStateProvider interface {
	CredentialStates() []CredentialState
	LastCredentialSelection() (CredentialSelection, bool)
}
