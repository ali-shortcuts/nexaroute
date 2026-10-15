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
	"math"
	"net/http"
	"strconv"
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
	tokens     float64
	last       time.Time
	capacity   float64
	generation uint64
}

type clientAuthState struct {
	mu      sync.Mutex
	buckets map[string]*clientBucket
}

type clientIdentity struct {
	ID             string
	TenantID       string
	ProjectID      string
	TeamID         string
	Role           string
	Virtual        bool
	RequirePrivacy string
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
		return clientIdentity{ID: key.ID, TenantID: key.TenantID, ProjectID: key.ProjectID, TeamID: key.TeamID, Role: key.Role, Virtual: true, RequirePrivacy: key.RequirePrivacy}, key, true
	}
	if digestInList(cfg.ClientAuth.KeyHashes, digest) || clientKeyMatches(cfg.ClientAuth.Keys, presented) {
		return clientIdentity{ID: "legacy", Role: "admin"}, config.VirtualKeyConfig{}, true
	}
	return clientIdentity{}, config.VirtualKeyConfig{}, false
}

// digestInList reports whether digest equals any stored key digest,
// comparing every entry in constant time.
func digestInList(hashes []string, digest string) bool {
	matched := 0
	for _, h := range hashes {
		if subtleConstantTimeCompare(h, digest) {
			matched++
		}
	}
	return matched > 0
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
// requestTopLevelValues parses a complete JSON object and retains every
// occurrence of a top-level field. Duplicate security-relevant fields are
// handled conservatively because upstream providers may choose different
// duplicate-key semantics.
func requestTopLevelValues(body []byte, wanted string) ([]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	var found []json.RawMessage
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		if key == wanted {
			found = append(found, append(json.RawMessage(nil), value...))
		}
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return nil, false
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return found, true
}

func requestTopLevelString(body []byte, field string) (string, bool) {
	values, ok := requestTopLevelValues(body, field)
	if !ok || len(values) != 1 {
		return "", false
	}
	var value string
	if json.Unmarshal(values[0], &value) != nil {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// requestTopLevelMaxInt returns the maximum duplicate value, not just the
// first/last. This makes token-budget enforcement conservative across JSON
// parsers with differing duplicate-key behavior.
func requestTopLevelMaxInt(body []byte, field string) (int, bool) {
	values, ok := requestTopLevelValues(body, field)
	if !ok {
		return 0, false
	}
	maxValue := 0
	for _, raw := range values {
		var value int
		if json.Unmarshal(raw, &value) != nil || value < 0 {
			return 0, false
		}
		if value > maxValue {
			maxValue = value
		}
	}
	return maxValue, true
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
	model, ok := requestTopLevelString(prefix, "model")
	if !ok {
		return ""
	}
	return model
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
	output, ok := requestTopLevelMaxInt(prefix, "max_tokens")
	if !ok {
		return 1 << 30
	}
	maxOutput, ok := requestTopLevelMaxInt(prefix, "max_output_tokens")
	if !ok {
		return 1 << 30
	}
	if maxOutput > output {
		output = maxOutput
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
		s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "guardrail_audit", Message: "request matched privacy/safety guardrail kinds=" + strings.Join(guardrail.Kinds(findings), ",")})
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

	// When no tenant hierarchy is configured, tenant/project/team IDs are
	// legacy billing metadata and the key's own allow-lists remain authoritative.
	// Once a hierarchy is configured, every supplied scope must resolve and each
	// parent policy is a ceiling that the child key cannot widen.
	if len(cfg.ClientAuth.Tenants) == 0 {
		return true
	}
	if key.TeamID != "" && key.ProjectID == "" {
		return false
	}
	if key.TenantID == "" {
		return false
	}

	for _, tenant := range cfg.ClientAuth.Tenants {
		if tenant.ID != key.TenantID {
			continue
		}
		if key.ProjectID == "" {
			return true
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
	// Scoped keys may not rely on an unconfigured tenant.
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
		if b.capacity != capacity {
			// A configuration change starts a new quota generation. A request
			// admitted under the previous ceiling must not settle into this one.
			b.capacity = capacity
			if b.tokens > capacity {
				b.tokens = capacity
			}
			s.clientBucketGeneration++
			b.generation = s.clientBucketGeneration
		}
		if now.Sub(b.last) > clientBucketIdle {
			b.tokens = capacity
			b.last = now
			s.clientBucketGeneration++
			b.generation = s.clientBucketGeneration
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
	s.clientBucketGeneration++
	b := &clientBucket{tokens: capacity, last: now, capacity: capacity, generation: s.clientBucketGeneration}
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

// tpmReservation is the per-request TPM charge made at admission from
// the conservative request estimate. When the provider later reports
// actual usage, clientTPMSettle reconciles the bucket to the actual
// total. charged is guarded by s.clientRL.
type tpmReservation struct {
	bucketID   string
	capacity   float64
	charged    int64
	generation uint64
}

type tpmReservationContextKey struct{}

func contextWithTPMReservation(ctx context.Context, res *tpmReservation) context.Context {
	return context.WithValue(ctx, tpmReservationContextKey{}, res)
}

func tpmReservationFromContext(ctx context.Context) *tpmReservation {
	res, _ := ctx.Value(tpmReservationContextKey{}).(*tpmReservation)
	return res
}

// clientTPMReserve charges the admission estimate against the client's
// TPM bucket and returns the reservation to settle later. A nil
// reservation with ok=true means no TPM ceiling applies.
func (s *Server) clientTPMReserve(presented string, tpm, estimated int) (*tpmReservation, bool) {
	res, ok, _ := s.clientTPMReserveAt(presented, tpm, estimated, time.Now())
	return res, ok
}

// clientTPMReserveAt provides a deterministic clock for tests and returns a
// conservative Retry-After when refill could make the request admissible.
// Requests larger than the entire bucket capacity cannot be admitted by
// waiting; they fail without a Retry-After hint and do not allocate/debit a
// bucket.
func (s *Server) clientTPMReserveAt(presented string, tpm, estimated int, now time.Time) (*tpmReservation, bool, time.Duration) {
	if tpm <= 0 || estimated <= 0 {
		return nil, true, 0
	}
	capacity := float64(tpm)
	if float64(estimated) > capacity {
		return nil, false, 0
	}

	id := keyDigest(presented) + "#tpm"
	s.clientRL.Lock()
	defer s.clientRL.Unlock()
	b := s.clientBucketLocked(id, capacity, now)
	if b == nil {
		return nil, false, 0
	}
	refill := capacity / 60
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * refill
	}
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	needed := float64(estimated)
	if needed > b.tokens {
		waitSeconds := math.Ceil((needed - b.tokens) / refill)
		if waitSeconds < 1 {
			waitSeconds = 1
		}
		return nil, false, time.Duration(waitSeconds) * time.Second
	}
	b.tokens -= needed
	return &tpmReservation{
		bucketID: id, capacity: capacity, charged: int64(estimated), generation: b.generation,
	}, true, 0
}

func (s *Server) clientTPMAllow(presented string, tpm, estimated int) bool {
	_, ok := s.clientTPMReserve(presented, tpm, estimated)
	return ok
}

// clientTPMSettle reconciles a reservation with actual provider-reported
// usage. It ignores stale reservations after bucket eviction, idle reset, or
// quota configuration changes, so a late response cannot modify a new quota
// window for the same client.
func (s *Server) clientTPMSettle(res *tpmReservation, actual int64) {
	s.clientTPMSettleAt(res, actual, time.Now())
}

func (s *Server) clientTPMSettleAt(res *tpmReservation, actual int64, now time.Time) {
	if res == nil || actual < 0 {
		return
	}
	s.clientRL.Lock()
	defer s.clientRL.Unlock()
	b := s.clientBuckets[res.bucketID]
	if b == nil || b.generation != res.generation {
		return // evicted/recreated or reset quota generation
	}
	capacity := b.capacity
	if capacity <= 0 {
		capacity = res.capacity
	}
	refill := capacity / 60
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * refill
	}
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	b.tokens += float64(res.charged - actual)
	if b.tokens > capacity {
		b.tokens = capacity
	}
	if b.tokens < 0 {
		b.tokens = 0
	}
	res.charged = actual
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
	// Effective TPM: the virtual key's own ceiling wins; otherwise the
	// global client_auth.tpm applies to every client class, so legacy
	// clients are quota-bound too.
	tpm := cfg.ClientAuth.TPM
	if virtualKey.TPM > 0 {
		tpm = virtualKey.TPM
	}
	if tpm > 0 {
		res, allowed, retryAfter := s.clientTPMReserveAt(keyText, tpm, requestTokenEstimate(r), time.Now())
		if !allowed {
			message := "client token limit exceeded; reduce request size or retry later"
			if retryAfter > 0 {
				seconds := int(retryAfter / time.Second)
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				message = "client token limit exceeded; retry after " + strconv.Itoa(seconds) + " seconds"
			}
			if anthropicStyle {
				anthropicErrorJSON(w, http.StatusTooManyRequests, message)
			} else {
				errorJSON(w, http.StatusTooManyRequests, message)
			}
			return false
		}
		if res != nil {
			r2 := r.WithContext(contextWithTPMReservation(r.Context(), res))
			*r = *r2
		}
	}
	return true
}

func contextWithClientIdentity(ctx context.Context, id clientIdentity) context.Context {
	return context.WithValue(ctx, clientIdentityContextKey{}, id)
}
