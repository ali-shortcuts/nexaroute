package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/guardrail"
)

const (
	clientBucketIdle = 10 * time.Minute
	maxClientBuckets = 4096
)

type clientBucket struct {
	tokens float64
	last   time.Time
}

type clientAuthState struct {
	mu      sync.Mutex
	buckets map[string]*clientBucket
}

type clientIdentity struct {
	ID        string
	TenantID  string
	ProjectID string
	TeamID    string
	Role      string
	Virtual   bool
}

type clientIdentityContextKey struct{}

func clientIdentityFromRequest(r *http.Request) (clientIdentity, bool) {
	v, ok := r.Context().Value(clientIdentityContextKey{}).(clientIdentity)
	return v, ok
}

func newVirtualClientKey() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain := "nrk_" + base64.RawURLEncoding.EncodeToString(buf)
	return plain, keyDigest(plain), nil
}

func (s *Server) clientAuthConfig() (bool, []string, int) {
	cfg := s.currentConfig()
	return cfg.ClientAuth.Enabled, cfg.ClientAuth.Keys, cfg.ClientAuth.RPM
}

func extractClientKey(r *http.Request) string {
	if h := r.Header.Get("x-api-key"); h != "" {
		return strings.TrimSpace(h)
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):])
	}
	return auth
}

func keyDigest(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func clientKeyMatches(keys []string, presented string) bool {
	digest := keyDigest(presented)
	matched := 0
	for _, k := range keys {
		want := keyDigest(k)
		if subtleConstantTimeCompare(want, digest) {
			matched++
		}
	}
	return matched > 0
}

// Kept as a tiny wrapper so the key comparison policy is easy to audit.
func subtleConstantTimeCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *Server) findClientIdentity(presented string) (clientIdentity, config.VirtualKeyConfig, bool) {
	cfg := s.currentConfig()
	digest := keyDigest(presented)
	now := time.Now().UTC()
	for _, key := range cfg.ClientAuth.VirtualKeys {
		if key.Revoked || !subtleConstantTimeCompare(key.KeyHash, digest) {
			continue
		}
		if key.ExpiresAt != "" {
			if expiry, err := time.Parse(time.RFC3339, key.ExpiresAt); err != nil || !now.Before(expiry) {
				continue
			}
		}
		return clientIdentity{ID: key.ID, TenantID: key.TenantID, ProjectID: key.ProjectID, TeamID: key.TeamID, Role: key.Role, Virtual: true}, key, true
	}
	if clientKeyMatches(cfg.ClientAuth.Keys, presented) {
		return clientIdentity{ID: "legacy", Role: "admin"}, config.VirtualKeyConfig{}, true
	}
	return clientIdentity{}, config.VirtualKeyConfig{}, false
}

const requestMetadataReadLimit = 2 << 20

// readRequestPrefix reads only a bounded prefix for metadata inspection and
// restores every consumed byte in front of the unread request body. In
// particular, the inspection limit must never become an accidental upstream
// payload limit.
func readRequestPrefix(r *http.Request, limit int64) ([]byte, bool, error) {
	if r == nil || r.Body == nil {
		return nil, false, nil
	}
	consumed, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	// Restore the bytes even on read error: a metadata inspection failure must
	// not mutate the request that the data plane will subsequently consume.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(consumed), r.Body))
	if err != nil {
		return nil, false, err
	}
	truncated := int64(len(consumed)) > limit
	prefix := consumed
	if truncated {
		prefix = consumed[:int(limit)]
	}
	return prefix, truncated, nil
}

// requestTopLevelField decodes one top-level JSON field without requiring the
// entire document to fit in the inspection prefix. This lets us find "model"
// before a large messages/images field while still preserving the full body.
func requestTopLevelField(body []byte, wanted string) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(body))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	var found json.RawMessage
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return nil
		}
		key, ok := token.(string)
		if !ok {
			return nil
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil
		}
		// encoding/json's struct decoding uses the last duplicate field.
		// Match that behavior for token estimation, rather than letting an
		// earlier low limit hide a later larger output-token request.
		if key == wanted {
			found = append(json.RawMessage(nil), value...)
		}
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return nil
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil
	}
	return found
}

func requestTopLevelInt(body []byte, field string) int {
	raw := requestTopLevelField(body, field)
	var value int
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0
	}
	return value
}

func requestModel(r *http.Request) string {
	if r == nil || r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return ""
	}
	prefix, truncated, err := readRequestPrefix(r, requestMetadataReadLimit)
	if err != nil || truncated {
		// Model authorization must not trust a prefix that cannot prove which
		// duplicate top-level "model" field the upstream will consume. The
		// identity policy treats an unknown model as unauthorized when any
		// key/project/team has a model allow-list.
		return ""
	}
	var envelope struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(prefix, &envelope) != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Model)
}

func requestTokenEstimate(r *http.Request) int {
	if r == nil || r.Body == nil {
		return 1
	}
	prefix, truncated, err := readRequestPrefix(r, requestMetadataReadLimit)
	if err != nil || truncated {
		// A partial prefix cannot prove the effective value of duplicated
		// output-token fields. Fail closed for TPM-limited keys.
		return 1 << 30
	}
	output := requestTopLevelInt(prefix, "max_tokens")
	if n := requestTopLevelInt(prefix, "max_output_tokens"); n > output {
		output = n
	}
	inputBytes := len(prefix)
	if r.ContentLength >= 0 && r.ContentLength > int64(inputBytes) {
		if r.ContentLength > int64(1<<30) {
			inputBytes = 1 << 30
		} else {
			inputBytes = int(r.ContentLength)
		}
	} else if truncated && r.ContentLength < 0 {
		// A chunked/unknown-length body beyond the inspection limit cannot be
		// estimated safely from its prefix; fail closed for TPM-limited keys.
		return 1 << 30
	}
	estimate := inputBytes/4 + output
	if estimate < 1 {
		estimate = 1
	}
	if estimate > 1<<30 {
		estimate = 1 << 30
	}
	return estimate
}

func (s *Server) applyGuardrails(w http.ResponseWriter, r *http.Request, anthropicStyle bool) bool {
	cfg := s.currentConfig().Guardrails
	if cfg.Mode == "off" || r.Body == nil {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(cfg.MaxBodyBytes)+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > cfg.MaxBodyBytes {
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusRequestEntityTooLarge, "request exceeds guardrail body limit")
		} else {
			errorJSON(w, http.StatusRequestEntityTooLarge, "request exceeds guardrail body limit")
		}
		return false
	}
	findings := guardrail.Scan(body)
	filtered := findings[:0]
	for _, finding := range findings {
		if strings.HasPrefix(finding.Kind, "pii_") && cfg.RejectPII || finding.Kind == "secret_api_key" && cfg.RejectSecrets || finding.Kind == "prompt_injection" && cfg.RejectPromptInjection {
			filtered = append(filtered, finding)
		}
	}
	findings = filtered
	if len(findings) == 0 {
		return true
	}
	if cfg.Mode == "audit" {
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "guardrail_audit", Message: "request matched privacy/safety guardrail"})
		return true
	}
	if anthropicStyle {
		anthropicErrorJSON(w, http.StatusForbidden, "request blocked by gateway guardrail")
	} else {
		errorJSON(w, http.StatusForbidden, "request blocked by gateway guardrail")
	}
	return false
}

func listAllows(list []string, value string) bool {
	if len(list) == 0 {
		return true
	}
	for _, item := range list {
		if item == "*" || item == value {
			return true
		}
	}
	return false
}

// identityPolicyAllows applies every configured scope as a constraint.
// Key-level restrictions may narrow project/team access, but must never widen
// it: an explicit "*" on a key cannot override a narrower parent policy.
func identityPolicyAllows(cfg config.Config, id clientIdentity, key config.VirtualKeyConfig, path, model string) bool {
	if !id.Virtual {
		return true
	}
	if !listAllows(key.AllowedRoutes, path) || !listAllows(key.AllowedModels, model) {
		return false
	}

	// A team is nested inside a project, and a project is nested inside a
	// tenant. Missing parent identifiers make the requested scope ambiguous.
	if key.TeamID != "" && key.ProjectID == "" {
		return false
	}
	if key.ProjectID == "" {
		// Tenant-only identifiers are supported as metadata for existing
		// deployments; in that case the key's own allow-lists are authoritative.
		return key.TeamID == ""
	}
	if key.TenantID == "" {
		return false
	}

	for _, tenant := range cfg.ClientAuth.Tenants {
		if tenant.ID != key.TenantID {
			continue
		}
		for _, project := range tenant.Projects {
			if project.ID != key.ProjectID {
				continue
			}
			if !listAllows(project.AllowedRoutes, path) || !listAllows(project.AllowedModels, model) {
				return false
			}
			if key.TeamID == "" {
				return true
			}
			for _, team := range project.Teams {
				if team.ID != key.TeamID {
					continue
				}
				return listAllows(team.AllowedRoutes, path) && listAllows(team.AllowedModels, model)
			}
			// A key may not name a team outside its selected project.
			return false
		}
		// The key's project must belong to the selected tenant.
		return false
	}
	// A project-scoped key may not rely on an unconfigured tenant.
	return false
}

// clientBucketLocked returns a live bucket while holding clientRL. Both RPM
// and TPM buckets share one hard memory bound so expired/rotated keys cannot
// grow the map indefinitely. A full table fails closed for a new identity.
func (s *Server) clientBucketLocked(id string, capacity float64, now time.Time) *clientBucket {
	if s.clientBuckets == nil {
		s.clientBuckets = make(map[string]*clientBucket)
	}
	if b := s.clientBuckets[id]; b != nil {
		if now.Sub(b.last) > clientBucketIdle {
			b.tokens = capacity
			b.last = now
		}
		return b
	}
	if len(s.clientBuckets) >= maxClientBuckets {
		for key, b := range s.clientBuckets {
			if now.Sub(b.last) > clientBucketIdle {
				delete(s.clientBuckets, key)
			}
		}
	}
	if len(s.clientBuckets) >= maxClientBuckets {
		return nil
	}
	b := &clientBucket{tokens: capacity, last: now}
	s.clientBuckets[id] = b
	return b
}

// clientRPMAllow enforces the per-key requests-per-minute ceiling with a
// token bucket. RPM 0 means unlimited.
func (s *Server) clientRPMAllow(presented string, rpm int) bool {
	if rpm <= 0 {
		return true
	}
	id := keyDigest(presented)
	now := time.Now()
	s.clientRL.Lock()
	defer s.clientRL.Unlock()
	capacity := float64(rpm)
	b := s.clientBucketLocked(id, capacity, now)
	if b == nil {
		return false
	}
	refill := float64(rpm) / 60.0
	b.tokens += now.Sub(b.last).Seconds() * refill
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Server) clientTPMAllow(presented string, tpm, estimated int) bool {
	if tpm <= 0 || estimated <= 0 {
		return true
	}
	id := keyDigest(presented) + "#tpm"
	now := time.Now()
	s.clientRL.Lock()
	defer s.clientRL.Unlock()
	capacity := float64(tpm)
	b := s.clientBucketLocked(id, capacity, now)
	if b == nil {
		return false
	}
	refill := capacity / 60
	b.tokens += now.Sub(b.last).Seconds() * refill
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	if float64(estimated) > b.tokens {
		return false
	}
	b.tokens -= float64(estimated)
	return true
}

func (s *Server) clientAuthAllowed(w http.ResponseWriter, r *http.Request, anthropicStyle bool) bool {
	enabled, keys, globalRPM := s.clientAuthConfig()
	if !enabled {
		return true
	}
	keyText := extractClientKey(r)
	if keyText == "" {
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		} else {
			errorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		}
		return false
	}
	identity, virtualKey, ok := s.findClientIdentity(keyText)
	if !ok && !clientKeyMatches(keys, keyText) {
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		} else {
			errorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		}
		return false
	}
	if !ok {
		identity = clientIdentity{ID: "legacy", Role: "admin"}
	}
	cfg := s.currentConfig()
	if !s.applyGuardrails(w, r, anthropicStyle) {
		return false
	}
	model := requestModel(r)
	if !identityPolicyAllows(cfg, identity, virtualKey, r.URL.Path, model) {
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusForbidden, "client key is not authorized for this model or route")
		} else {
			errorJSON(w, http.StatusForbidden, "client key is not authorized for this model or route")
		}
		return false
	}
	r2 := r.WithContext(contextWithClientIdentity(r.Context(), identity))
	*r = *r2
	rpm := globalRPM
	if virtualKey.RPM > 0 {
		rpm = virtualKey.RPM
	}
	if !s.clientRPMAllow(keyText, rpm) {
		w.Header().Set("Retry-After", "60")
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusTooManyRequests, "client rate limit exceeded; retry after 60 seconds")
		} else {
			errorJSON(w, http.StatusTooManyRequests, "client rate limit exceeded; retry after 60 seconds")
		}
		return false
	}
	if virtualKey.TPM > 0 && !s.clientTPMAllow(keyText, virtualKey.TPM, requestTokenEstimate(r)) {
		w.Header().Set("Retry-After", "60")
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusTooManyRequests, "client token limit exceeded; retry after 60 seconds")
		} else {
			errorJSON(w, http.StatusTooManyRequests, "client token limit exceeded; retry after 60 seconds")
		}
		return false
	}
	return true
}

func contextWithClientIdentity(ctx context.Context, id clientIdentity) context.Context {
	return context.WithValue(ctx, clientIdentityContextKey{}, id)
}
