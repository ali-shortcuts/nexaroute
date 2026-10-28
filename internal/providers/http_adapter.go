package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/egress"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

type credentialState struct {
	Key       string
	Name      string
	PoolIndex int
	Priority  int
	Weight    float64
	Failures  int
	Active    int64
	// CooldownUntil is the temporary exclusion deadline. It never flips
	// operator Enabled and never deletes the entry; expiry restores the
	// credential with no operator action.
	CooldownUntil time.Time
	LastStatus    int
}

type RetryAfterError struct {
	Cause error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return e.Cause.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Cause }

func RetryAfter(err error) (time.Duration, bool) {
	var e *RetryAfterError
	if errors.As(err, &e) && e.After > 0 {
		return e.After, true
	}
	return 0, false
}

type httpAdapter struct {
	p                  config.ProviderConfig
	c                  *http.Client
	streamC            *http.Client
	sem                chan struct{}
	credMu             sync.RWMutex
	creds              []credentialState
	bedrockCreds       sigV4Credentials
	rr                 uint64
	forwardAllowed     map[string]struct{}
	retryAfterCap      time.Duration
	active             atomic.Int64
	waiting            atomic.Int64
	requestLimit       atomic.Int64
	remainingRequests  atomic.Int64
	tokenLimit         atomic.Int64
	remainingTokens    atomic.Int64
	requestResetUnix   atomic.Int64
	tokenResetUnix     atomic.Int64
	rateLimitResetUnix atomic.Int64
	reservedRequests   atomic.Int64
	reservedTokens     atomic.Int64
	// requestRemainObservedUnix / tokenRemainObservedUnix record when the
	// stored remaining value for each resource was observed. Quota evidence
	// is only valid for the provider window it was observed in; these
	// timestamps let Stats and the rollover guard tell current-window
	// evidence from a closed window's leftovers.
	requestRemainObservedUnix atomic.Int64
	tokenRemainObservedUnix   atomic.Int64
	quotaDispatchSeq          atomic.Uint64
	quotaMu                   sync.Mutex
	requestLimitSeq           uint64
	requestRemainSeq          uint64
	requestResetSeq           uint64
	tokenLimitSeq             uint64
	tokenRemainSeq            uint64
	tokenResetSeq             uint64
	// lastSel records the most recent credential choice for the
	// redacted explain surface (tier / affinity / least-loaded).
	lastSelMu sync.Mutex
	lastSel   CredentialSelection
	lastSelOK bool
	// evaluationOnly marks an adapter clone reserved for Phase H live
	// evaluation. Live evaluation refuses to run against a production adapter,
	// so this flag is what makes "isolation bypassed" a hard failure.
	evaluationOnly bool
	// sessionPins maps a session SHA-256 (never a raw session key) to the
	// credential index that session last used, with a TTL. A pinned
	// credential is used only while it stays available; otherwise
	// selection falls back to the tournament and re-pins. This is
	// credential-level affinity, distinct from router deployment
	// pinning and from the x-opencode-session header value.
	sessionMu   sync.Mutex
	sessionPins map[string]sessionPin
}

type sessionPin struct {
	credIdx int
	expires time.Time
}

const (
	sessionPinTTL = time.Hour
	sessionPinMax = 1024
)

func newHTTPAdapter(p config.ProviderConfig, timeout time.Duration) (*httpAdapter, error) {
	return newHTTPAdapterWithRetryCap(p, timeout, 60*time.Second)
}

// parseEgressCIDRs converts operator-configured CIDR strings into prefixes.
// Bare IPs are treated as single-host prefixes. Invalid entries are ignored
// here because config validation already rejects them at load time.
func parseEgressCIDRs(cidrs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, c := range cidrs {
		if prefix, err := netip.ParsePrefix(c); err == nil {
			out = append(out, prefix)
		} else if addr, err := netip.ParseAddr(c); err == nil {
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return out
}

// maxAdapterConcurrency mirrors the config validation ceiling so the adapter
// semaphore can never be sized from an unbounded value.
const maxAdapterConcurrency = 4096

// newConcurrencySem sizes the in-flight semaphore. The size is checked against
// the ceiling at the allocation itself so no caller-influenced value can ever
// reach make() unbounded.
func newConcurrencySem(n int) chan struct{} {
	if n >= 1 && n <= maxAdapterConcurrency {
		return make(chan struct{}, n)
	}
	if n > maxAdapterConcurrency {
		return make(chan struct{}, maxAdapterConcurrency)
	}
	return make(chan struct{}, 32)
}

func newHTTPAdapterWithRetryCap(p config.ProviderConfig, timeout, retryAfterCap time.Duration) (*httpAdapter, error) {
	if retryAfterCap <= 0 {
		retryAfterCap = 60 * time.Second
	}
	mc := p.MaxConcurrency
	if mc <= 0 {
		mc = 32
	}
	if mc > maxAdapterConcurrency {
		mc = maxAdapterConcurrency
	}
	maxIdle := mc * 2
	if maxIdle < 64 {
		maxIdle = 64
	}
	if maxIdle > 2048 {
		maxIdle = 2048
	}
	idlePerHost := mc
	if idlePerHost < 16 {
		idlePerHost = 16
	}
	if idlePerHost > 512 {
		idlePerHost = 512
	}
	base, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("provider %s base URL: %w", p.ID, err)
	}
	allowedHosts := append([]string(nil), p.AllowedHosts...)
	if len(allowedHosts) == 0 {
		allowedHosts = []string{base.Hostname()}
	}
	if p.ProxyURL != "" {
		if proxy, proxyErr := url.Parse(p.ProxyURL); proxyErr == nil && proxy.Hostname() != "" {
			allowedHosts = append(allowedHosts, proxy.Hostname())
		}
	}
	policy := egress.Policy{AllowedHosts: allowedHosts, AllowLoopback: base.Scheme == "http"}
	policy.DenyHosts = p.EgressDenyHosts()
	policy.DenyCIDRs = parseEgressCIDRs(p.EgressDenyCIDRs())
	tr, err := policy.NewTransport(p.BaseURL, p.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("provider %s egress policy: %w", p.ID, err)
	}
	tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost = maxIdle, idlePerHost, mc
	tr.ResponseHeaderTimeout = timeout
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if !sameHTTPOrigin(via[0].URL, req.URL) {
			// API keys, custom provider headers, and allowlisted client headers
			// must never cross an origin boundary through an automatic redirect.
			return errors.New("refused cross-origin provider redirect")
		}
		return nil
	}
	a := &httpAdapter{
		p: p, c: &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: checkRedirect}, streamC: &http.Client{Transport: tr, CheckRedirect: checkRedirect},
		sem: newConcurrencySem(mc), forwardAllowed: make(map[string]struct{}),
		retryAfterCap: retryAfterCap,
	}
	a.requestLimit.Store(-1)
	a.remainingRequests.Store(-1)
	a.tokenLimit.Store(-1)
	a.remainingTokens.Store(-1)
	for _, h := range p.ForwardHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			a.forwardAllowed[h] = struct{}{}
		}
	}
	for _, e := range p.ResolvedCredentialEntries() {
		a.creds = append(a.creds, credentialState{Key: e.Key, Name: e.Name, PoolIndex: e.PoolIndex, Priority: e.Priority, Weight: e.Weight})
	}
	if p.Type == "bedrock" {
		// SigV4 needs the key pair. The secret comes only from the
		// env var NAMED in config (never a config value) and adapter
		// construction fails closed when it is unavailable — a
		// silently unsigned Bedrock request would be a credential
		// leak to a spoofed endpoint and a guaranteed 403 upstream.
		secret := os.Getenv(p.SecretKeyEnv)
		if p.SecretKeyEnv == "" || secret == "" {
			// The configured value is deliberately not echoed: a misplaced
			// literal secret in secret_key_env would otherwise reach logs.
			return nil, fmt.Errorf("provider %s bedrock secret access key: env var named by secret_key_env is not set", p.ID)
		}
		accessKey := ""
		if keys := p.ResolvedCredentials(); len(keys) > 0 {
			accessKey = keys[0]
		}
		if accessKey == "" {
			return nil, fmt.Errorf("provider %s bedrock access key id is not configured (api_key or api_key_env)", p.ID)
		}
		a.bedrockCreds = sigV4Credentials{
			AccessKey:    accessKey,
			SecretKey:    secret,
			SessionToken: os.Getenv(p.SessionTokenEnv),
		}
	}
	return a, nil
}
func (a *httpAdapter) ID() string   { return a.p.ID }
func (a *httpAdapter) Kind() string { return a.p.Type }
func (a *httpAdapter) CloseIdleConnections() {
	if a.c != nil {
		a.c.CloseIdleConnections()
	}
}
func (a *httpAdapter) Stats() ProviderStats {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	now := time.Now()
	cooling := 0
	for _, c := range a.creds {
		if !c.CooldownUntil.IsZero() && now.Before(c.CooldownUntil) {
			cooling++
		}
	}
	remainingRequests := a.remainingRequests.Load()
	remainingTokens := a.remainingTokens.Load()
	// A remaining value observed at or before its window's reset belongs
	// to a window that has since closed; once the reset has passed, that
	// evidence is stale and reports as unknown. A value observed after
	// the reset passed is current-window evidence and is kept.
	nowUnix := now.Unix()
	if reset := a.requestResetUnix.Load(); reset > 0 && nowUnix >= reset && a.requestRemainObservedUnix.Load() <= reset {
		remainingRequests = -1
	}
	if reset := a.tokenResetUnix.Load(); reset > 0 && nowUnix >= reset && a.tokenRemainObservedUnix.Load() <= reset {
		remainingTokens = -1
	}
	if obs := a.requestRemainObservedUnix.Load(); obs > 0 && nowUnix-obs > int64(quotaEvidenceMaxAge/time.Second) {
		remainingRequests = -1
	}
	if obs := a.tokenRemainObservedUnix.Load(); obs > 0 && nowUnix-obs > int64(quotaEvidenceMaxAge/time.Second) {
		remainingTokens = -1
	}
	reservedRequests := a.reservedRequests.Load()
	reservedTokens := a.reservedTokens.Load()
	effectiveRequests := remainingRequests
	if effectiveRequests >= 0 {
		effectiveRequests -= reservedRequests
		if effectiveRequests < 0 {
			effectiveRequests = 0
		}
	}
	effectiveTokens := remainingTokens
	if effectiveTokens >= 0 {
		effectiveTokens -= reservedTokens
		if effectiveTokens < 0 {
			effectiveTokens = 0
		}
	}
	return ProviderStats{
		ID: a.p.ID, MaxConcurrency: cap(a.sem),
		ActiveRequests: a.active.Load(), WaitingRequests: a.waiting.Load(),
		Credentials: len(a.creds), CredentialsCooling: cooling,
		RequestLimit: a.requestLimit.Load(), RemainingRequests: remainingRequests,
		ReservedRequests: reservedRequests, EffectiveRemainingRequests: effectiveRequests,
		TokenLimit: a.tokenLimit.Load(), RemainingTokens: remainingTokens,
		ReservedTokens: reservedTokens, EffectiveRemainingTokens: effectiveTokens,
		RequestResetUnix: a.requestResetUnix.Load(), TokenResetUnix: a.tokenResetUnix.Load(),
		RateLimitResetUnix: a.rateLimitResetUnix.Load(),
	}
}

func (a *httpAdapter) CredentialsMatch(keys []string) bool {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	if len(keys) != len(a.creds) {
		return false
	}
	for i, key := range keys {
		if a.creds[i].Key != key {
			return false
		}
	}
	return true
}

func (a *httpAdapter) RedactBody(b []byte) []byte {
	out := append([]byte(nil), b...)
	for _, value := range a.p.Headers {
		if value != "" {
			out = bytes.ReplaceAll(out, []byte(value), []byte("[REDACTED]"))
		}
	}
	a.credMu.RLock()
	keys := make([]string, 0, len(a.creds))
	for i := range a.creds {
		if a.creds[i].Key != "" {
			keys = append(keys, a.creds[i].Key)
		}
	}
	a.credMu.RUnlock()
	for _, key := range keys {
		out = bytes.ReplaceAll(out, []byte(key), []byte("[REDACTED]"))
	}
	// Also redact the currently resolved primary env value. During credential
	// rotation this can differ briefly from the snapshot held by an in-flight
	// adapter, so covering both sides prevents transition-time leaks.
	if key := a.p.ResolvedAPIKey(); key != "" {
		out = bytes.ReplaceAll(out, []byte(key), []byte("[REDACTED]"))
	}
	return out
}

func sameHTTPOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// hostAllowed reports whether the endpoint's host is one the operator
// configured for this provider: the base URL host, or an entry in the
// provider's allowed_hosts list (compared the way the egress policy
// compares them: case-insensitive, ignoring surrounding spaces and a
// trailing dot). It mirrors the dial-time allowlist at request-build
// time; the transport remains the enforcement of last resort.
func (a *httpAdapter) hostAllowed(u *url.URL) bool {
	host := u.Hostname()
	if host == "" {
		return false
	}
	if len(a.p.AllowedHosts) == 0 {
		base, err := url.Parse(a.p.BaseURL)
		return err == nil && strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(base.Hostname()), "."), strings.TrimSuffix(host, "."))
	}
	for _, allowed := range a.p.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(allowed), "."), strings.TrimSuffix(host, ".")) {
			return true
		}
	}
	return false
}

func endpoint(base, suffix string) string {
	b := strings.TrimRight(base, "/")
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return b
	}
	if u, err := url.Parse(suffix); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.String()
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	if strings.HasSuffix(b, suffix) {
		return b
	}
	for _, known := range []string{"/v1/messages/count_tokens", "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/models", "/models"} {
		if strings.HasSuffix(b, known) {
			b = strings.TrimSuffix(b, known)
			break
		}
	}
	if strings.HasSuffix(b, "/v1") && strings.HasPrefix(suffix, "/v1/") {
		return b + strings.TrimPrefix(suffix, "/v1")
	}
	return b + suffix
}

func (a *httpAdapter) defaultPath() string {
	switch a.p.Type {
	case "anthropic_compatible":
		return a.p.MessagesPath
	case "openai_responses":
		return a.p.ResponsesPath
	default:
		return a.p.ChatPath
	}
}

type quotaReservation struct {
	a           *httpAdapter
	requests    int64
	tokens      int64
	requestOnce sync.Once
	tokenOnce   sync.Once
}

func (a *httpAdapter) reserveQuota(ctx context.Context) *quotaReservation {
	tokens, ok := quotaEstimateFromContext(ctx)
	if !ok {
		return nil
	}
	r := &quotaReservation{a: a, requests: 1, tokens: tokens}
	a.reservedRequests.Add(1)
	if tokens > 0 {
		a.reservedTokens.Add(tokens)
	}
	return r
}

func (r *quotaReservation) releaseRequests() {
	if r == nil || r.a == nil || r.requests <= 0 {
		return
	}
	r.requestOnce.Do(func() {
		r.a.reservedRequests.Add(-r.requests)
	})
}

func (r *quotaReservation) releaseTokens() {
	if r == nil || r.a == nil || r.tokens <= 0 {
		return
	}
	r.tokenOnce.Do(func() {
		r.a.reservedTokens.Add(-r.tokens)
	})
}

func (r *quotaReservation) releaseAll() {
	if r == nil {
		return
	}
	r.releaseRequests()
	r.releaseTokens()
}

// bedrockModelPath builds the Converse model-in-path endpoint (see
// canonical.BedrockModelPath for the escaping rule that keeps the
// wire path identical to the SigV4-canonicalized path).
func bedrockModelPath(model string, stream bool) string {
	return canonical.BedrockModelPath(model, stream)
}

// vertexModelPath builds the Vertex AI model-in-path endpoint for
// the configured project/location. Vertex speaks the Gemini wire
// format; streaming uses :streamGenerateContent with SSE.
func (a *httpAdapter) vertexModelPath(model string, stream bool) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	base := "/v1/projects/" + url.PathEscape(a.p.VertexProject) +
		"/locations/" + url.PathEscape(a.p.VertexLocation) +
		"/publishers/google/models/" + url.PathEscape(model)
	if stream {
		return base + ":streamGenerateContent?alt=sse"
	}
	return base + ":generateContent"
}

// azureDeploymentPath builds the Azure OpenAI per-deployment chat
// endpoint with its api-version query. The model argument is the
// Azure deployment name.
func (a *httpAdapter) azureDeploymentPath(model string) string {
	version := a.p.APIVersion
	if version == "" {
		version = "2024-10-21"
	}
	return "/openai/deployments/" + url.PathEscape(strings.TrimSpace(model)) +
		"/chat/completions?api-version=" + url.QueryEscape(version)
}

// geminiModelPath builds the model-in-path Gemini endpoint. Stream calls use
// streamGenerateContent with the SSE alt parameter.
func (a *httpAdapter) geminiModelPath(model string, stream bool) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if stream {
		return "/v1beta/models/" + url.PathEscape(model) + ":streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + url.PathEscape(model) + ":generateContent"
}

func (a *httpAdapter) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	if a.p.Type == "azure_openai" {
		// Azure addresses deployments in the URL; the deployment name
		// is the payload's model (the dispatch layer has already
		// substituted the deployment's upstream model).
		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(payload, &probe); err == nil && probe.Model != "" {
			return a.DoPath(ctx, http.MethodPost, a.azureDeploymentPath(probe.Model), payload, stream, forward)
		}
	}
	return a.DoPath(ctx, http.MethodPost, a.defaultPath(), payload, stream, forward)
}

func (a *httpAdapter) CountTokens(ctx context.Context, payload []byte, forward http.Header) (*http.Response, error) {
	return a.DoPath(ctx, http.MethodPost, a.p.CountTokensPath, payload, false, forward)
}

func (a *httpAdapter) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	u := endpoint(a.p.BaseURL, path)
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid endpoint: must be an absolute http(s) URL")
	}
	if parsed.User != nil {
		// Credentials embedded in a URL would be sent as Basic auth to
		// whatever host the endpoint names; provider auth is configured
		// separately and never travels in the URL.
		return nil, errors.New("invalid endpoint: user information is not allowed")
	}
	if !a.hostAllowed(parsed) {
		// The destination host must be one the operator configured for
		// this provider (the base URL host or an allowed_hosts entry).
		// The egress transport enforces the same set at dial time;
		// refusing here keeps a misdirected endpoint from ever carrying
		// provider credentials.
		return nil, errors.New("invalid endpoint: host is not in the provider's allowed hosts")
	}

	a.waiting.Add(1)
	select {
	case a.sem <- struct{}{}:
		a.waiting.Add(-1)
		a.active.Add(1)
	case <-ctx.Done():
		a.waiting.Add(-1)
		return nil, ctx.Err()
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			a.active.Add(-1)
			<-a.sem
		})
	}

	var lastErr error
	attempted := map[int]bool{}
	maxCredentialAttempts := len(a.creds)
	if maxCredentialAttempts == 0 {
		maxCredentialAttempts = 1
	}
	// Credential-level session affinity: the session hash (never the
	// raw session key) pins consecutive requests of one conversation to
	// one credential while it stays healthy+eligible. Absent context
	// means no pin, never a synthetic session.
	sessionHash := ""
	if sk, ok := SessionKeyFromContext(ctx); ok {
		sessionHash = SessionHash(sk)
	}
	for len(attempted) < maxCredentialAttempts {
		idx := -1
		key := ""
		if len(a.creds) > 0 {
			var ok bool
			idx, key, ok = a.reserveCredentialWithSession(attempted, sessionHash)
			if !ok {
				if len(attempted) == 0 {
					release()
					err := errors.New("all configured provider credentials are cooling down")
					if d, retryOK := a.nextCredentialRetryAfter(); retryOK {
						return nil, &RetryAfterError{Cause: err, After: d}
					}
					return nil, err
				}
				break
			}
			attempted[idx] = true
		} else {
			attempted[-1] = true
		}

		reqCtx := ctx
		var cancel context.CancelFunc
		if stream {
			reqCtx, cancel = context.WithCancel(ctx)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, u, bytes.NewReader(payload))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			a.releaseCredential(idx)
			release()
			return nil, err
		}
		if len(payload) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		a.applyHeaders(req, forward)
		a.applyOpencodeSession(req, reqCtx, payload)
		if ct := forward.Get("Content-Type"); ct != "" {
			// Media uploads (multipart/form-data) must keep the
			// client's content type — including its boundary — or the
			// upstream cannot parse the form. The JSON default set
			// above loses to an explicitly forwarded content type.
			req.Header.Set("Content-Type", ct)
		}
		a.applyAuthKey(req, key)
		if a.p.Type == "anthropic_compatible" && req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		if a.p.Type == "bedrock" {
			region := a.p.AWSRegion
			if region == "" {
				region = "us-east-1"
			}
			signV4(req, payload, a.bedrockCreds, region, "bedrock", time.Now())
		}

		client := a.c
		if stream {
			client = a.streamC
		}
		reservation := a.reserveQuota(reqCtx)
		quotaSeq := a.quotaDispatchSeq.Add(1)
		// The request URL is assembled only from operator-written provider
		// configuration (base URL and path fields,
		// validated at config load: http/https scheme, host required, userinfo
		// rejected). Client request data never selects the host — model IDs
		// enter as escaped path segments on those configured templates. The
		// transport enforces the egress policy at dial time: destinations are
		// pinned to the operator's allowed_hosts (default: the base URL host)
		// minus deny hosts/CIDRs, and redirects refuse to cross origins. An
		// absolute-URL path override is a tested operator feature and remains
		// bounded by the same dial-time allowlist.
		resp, err := client.Do(req) // codeql[go/request-forgery] operator-configured destination, egress-allowlisted at dial time — see block above.
		if err != nil {
			reservation.releaseAll()
			if cancel != nil {
				cancel()
			}
			a.releaseCredential(idx)
			release()
			return nil, err
		}
		observed := a.observeRateLimitHeaders(resp.Header, quotaSeq)
		if observed.RequestRemaining {
			reservation.releaseRequests()
		}
		if observed.TokenRemaining {
			reservation.releaseTokens()
		}
		if stream && cancel != nil {
			idle := time.Duration(a.p.StreamIdleTimeoutSeconds) * time.Second
			if idle > 0 {
				resp.Body = newIdleBody(resp.Body, idle, cancel)
			} else {
				resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
			}
		}
		if idx >= 0 && IsOpencodeType(a.p.Type) && resp.StatusCode == http.StatusForbidden {
			// Model-entitlement denial, NOT a credential failure: the
			// credential is not cooled, and the loop below must not
			// rotate to another credential for the same restricted
			// model (that would be a bypass attempt). Only the last
			// status is recorded, so the state surface can show the
			// denial without poisoning other models on this key. The
			// next request re-evaluates independently. A genuinely
			// invalid opencode key returning 403 surfaces the same
			// diagnostic on that request; see the docs limitation.
			a.noteOpencodeEntitlementDenial(idx)
		}
		if idx >= 0 && credentialRetryStatus(resp.StatusCode) && !(IsOpencodeType(a.p.Type) && resp.StatusCode == http.StatusForbidden) {
			a.cooldownCredential(idx, resp.StatusCode, resp.Header.Get("Retry-After"))
			if a.hasAvailableCredential(attempted) {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				reservation.releaseAll()
				a.releaseCredential(idx)
				lastErr = fmt.Errorf("credential rejected http %d: %s", resp.StatusCode, a.safeSnippet(body))
				continue
			}
		} else if idx >= 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.markCredentialSuccess(idx)
		}

		// Provider and credential capacity both stay reserved until the body is
		// fully consumed or closed. This makes key-level load balancing reflect
		// long-lived SSE streams instead of only time-to-first-byte.
		inner := resp.Body
		if a.p.Type == "bedrock" && stream {
			// ConverseStream speaks the AWS binary event stream; the
			// gateway's stream pump speaks SSE. Translate at the edge.
			inner = newEventStreamSSEReader(inner)
		}
		resp.Body = &releaseOnDoneBody{ReadCloser: inner, release: func() {
			reservation.releaseAll()
			a.releaseCredential(idx)
			release()
		}}
		return resp, nil
	}
	release()
	if lastErr == nil {
		lastErr = errors.New("no usable credentials")
	}
	return nil, lastErr
}

func parseQuotaIntHeader(h http.Header, names ...string) (int64, bool) {
	for _, name := range names {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}

func parseQuotaResetHeader(h http.Header, names ...string) (int64, bool) {
	now := time.Now()
	for _, name := range names {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(now) {
			return t.Unix(), true
		}
		if t, err := http.ParseTime(v); err == nil && t.After(now) {
			return t.Unix(), true
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return now.Add(d).Unix(), true
		}
	}
	return 0, false
}

// quotaEvidenceMaxAge bounds how long provider quota evidence
// (remaining requests/tokens from rate-limit headers) stays
// actionable (#435). Window-rollover invalidation handles the
// case where a reset is known; this bound covers providers that
// omit reset headers or report a far-future reset, where an
// observation would otherwise steer routing and reporting
// indefinitely. Rate-limit windows are minutes-to-hours and the
// next response refreshes the evidence, so an hour-old
// observation reports as unknown rather than as current fact.
const quotaEvidenceMaxAge = time.Hour

type quotaObservation struct {
	RequestRemaining bool
	TokenRemaining   bool
}

func (a *httpAdapter) observeRateLimitHeaders(h http.Header, dispatchSeq uint64) quotaObservation {
	requestLimit, requestLimitOK := parseQuotaIntHeader(h,
		"x-ratelimit-limit-requests", "anthropic-ratelimit-requests-limit",
	)
	requestRemaining, requestRemainingOK := parseQuotaIntHeader(h,
		"x-ratelimit-remaining-requests", "anthropic-ratelimit-requests-remaining",
	)
	tokenLimit, tokenLimitOK := parseQuotaIntHeader(h,
		"x-ratelimit-limit-tokens", "anthropic-ratelimit-tokens-limit",
	)
	tokenRemaining, tokenRemainingOK := parseQuotaIntHeader(h,
		"x-ratelimit-remaining-tokens", "anthropic-ratelimit-tokens-remaining",
	)
	requestReset, requestResetOK := parseQuotaResetHeader(h,
		"x-ratelimit-reset-requests", "anthropic-ratelimit-requests-reset",
	)
	tokenReset, tokenResetOK := parseQuotaResetHeader(h,
		"x-ratelimit-reset-tokens", "anthropic-ratelimit-tokens-reset",
	)

	obs := quotaObservation{
		RequestRemaining: requestRemainingOK,
		TokenRemaining:   tokenRemainingOK,
	}

	// Concurrent requests can complete out of order. A response from an older
	// dispatch must never overwrite quota evidence already observed from a
	// newer dispatch, or remaining headroom can jump backwards to a stale,
	// overly optimistic value. Serialize the small observation update so each
	// field's dispatch sequence and value move together.
	a.quotaMu.Lock()
	defer a.quotaMu.Unlock()
	nowUnix := time.Now().Unix()

	if requestLimitOK && dispatchSeq >= a.requestLimitSeq {
		a.requestLimitSeq = dispatchSeq
		a.requestLimit.Store(requestLimit)
	}
	requestRemainingAccepted := false
	if requestRemainingOK && dispatchSeq >= a.requestRemainSeq {
		a.requestRemainSeq = dispatchSeq
		a.remainingRequests.Store(requestRemaining)
		a.requestRemainObservedUnix.Store(nowUnix)
		requestRemainingAccepted = true
	}
	if requestResetOK && dispatchSeq >= a.requestResetSeq {
		oldReset := a.requestResetUnix.Load()
		a.requestResetSeq = dispatchSeq
		a.requestResetUnix.Store(requestReset)
		// Window rollover: the reset advanced past the previously known
		// window boundary, but this response carries no fresh remaining
		// for the resource. The stored remaining was observed in the
		// closed window and must not be paired with the new window's
		// future reset — stale evidence is unknown, never proof of
		// exhaustion. Discard it (unless a newer dispatch already
		// recorded fresher remaining evidence).
		if oldReset > 0 && requestReset > oldReset && !requestRemainingAccepted && dispatchSeq >= a.requestRemainSeq {
			a.remainingRequests.Store(-1)
			a.requestRemainSeq = dispatchSeq
		}
	}
	if tokenLimitOK && dispatchSeq >= a.tokenLimitSeq {
		a.tokenLimitSeq = dispatchSeq
		a.tokenLimit.Store(tokenLimit)
	}
	tokenRemainingAccepted := false
	if tokenRemainingOK && dispatchSeq >= a.tokenRemainSeq {
		a.tokenRemainSeq = dispatchSeq
		a.remainingTokens.Store(tokenRemaining)
		a.tokenRemainObservedUnix.Store(nowUnix)
		tokenRemainingAccepted = true
	}
	if tokenResetOK && dispatchSeq >= a.tokenResetSeq {
		oldReset := a.tokenResetUnix.Load()
		a.tokenResetSeq = dispatchSeq
		a.tokenResetUnix.Store(tokenReset)
		// Same window-rollover rule as for requests, per resource.
		if oldReset > 0 && tokenReset > oldReset && !tokenRemainingAccepted && dispatchSeq >= a.tokenRemainSeq {
			a.remainingTokens.Store(-1)
			a.tokenRemainSeq = dispatchSeq
		}
	}

	// The legacy summary is always derived from the latest accepted
	// resource-specific reset values. A response containing only one reset
	// therefore cannot accidentally erase a later deadline from the other.
	reset := a.requestResetUnix.Load()
	if token := a.tokenResetUnix.Load(); token > reset {
		reset = token
	}
	if reset > 0 {
		a.rateLimitResetUnix.Store(reset)
	}
	return obs
}

// applyOpencodeSession injects the derived x-opencode-session header
// for the Go profile only. A value already on the request (client-
// supplied via ForwardHeaders, or an operator static header) is
// forwarded UNCHANGED and never overwritten. Console never receives a
// gateway-derived header. No session value is ever logged.
func (a *httpAdapter) applyOpencodeSession(req *http.Request, ctx context.Context, payload []byte) {
	if a.p.Type != "opencode-go" {
		return
	}
	if req.Header.Get("x-opencode-session") != "" {
		return
	}
	sessionKey, _ := SessionKeyFromContext(ctx)
	req.Header.Set("x-opencode-session", DeriveOpencodeSessionValueCached(a.p, sessionKey, modelFromPayload(payload)))
}

// noteOpencodeEntitlementDenial records a model-restricted 403 on the
// credential's last status only: no failure count, no cooldown, so
// other models on the same key are not poisoned.
func (a *httpAdapter) noteOpencodeEntitlementDenial(i int) {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if i >= 0 && i < len(a.creds) {
		a.creds[i].LastStatus = http.StatusForbidden
	}
}

// CredentialStates returns the redacted live state of every adapter
// credential: index, pool index, name, tier, weight, last status,
// failures, in-flight count, and cooldown state. No keys, no emails.
func (a *httpAdapter) CredentialStates() []CredentialState {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	now := time.Now()
	out := make([]CredentialState, 0, len(a.creds))
	for i, c := range a.creds {
		cooling := !c.CooldownUntil.IsZero() && now.Before(c.CooldownUntil)
		out = append(out, CredentialState{
			Index: i, PoolIndex: c.PoolIndex, Name: c.Name, Enabled: true,
			Priority: c.Priority, Weight: credentialEffectiveWeight(c),
			LastStatus: c.LastStatus, Failures: c.Failures, Active: c.Active,
			Cooling: cooling, CooldownUntil: c.CooldownUntil,
		})
	}
	return out
}

// LastCredentialSelection returns the redacted explanation of the most
// recent credential choice, if any.
func (a *httpAdapter) LastCredentialSelection() (CredentialSelection, bool) {
	a.lastSelMu.Lock()
	defer a.lastSelMu.Unlock()
	return a.lastSel, a.lastSelOK
}

func (a *httpAdapter) applyHeaders(req *http.Request, forward http.Header) {
	for k, v := range a.p.Headers {
		req.Header.Set(k, v)
	}
	for k, vals := range forward {
		lk := strings.ToLower(k)
		// Credential headers are never forwarded from client input, even
		// when listed in ForwardHeaders: authorization, x-api-key,
		// x-goog-api-key (the Gemini credential injected by applyAuthKey),
		// and the gateway's own x-admin-key. Configured static provider
		// headers and applyAuthKey injection are unaffected.
		if _, ok := a.forwardAllowed[lk]; !ok || lk == "authorization" || lk == "x-api-key" || lk == "x-goog-api-key" || lk == "api-key" || lk == "x-admin-key" {
			continue
		}
		req.Header.Del(k)
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
}
func (a *httpAdapter) applyAuthKey(req *http.Request, key string) {
	if a.p.AuthMode == "none" {
		return
	}
	if key == "" {
		key = a.p.ResolvedAPIKey()
	}
	if key == "" {
		return
	}
	if a.p.Type == "bedrock" {
		return // SigV4 signing in DoPath replaces static header auth.
	}
	mode := a.p.AuthMode
	if mode == "" {
		switch a.p.Type {
		case "anthropic_compatible":
			mode = "x-api-key"
		case "gemini":
			mode = "x-goog-api-key"
		case "azure_openai":
			mode = "api-key"
		default:
			mode = "bearer"
		}
	}
	switch mode {
	case "x-api-key":
		req.Header.Set("x-api-key", key)
	case "x-goog-api-key":
		req.Header.Set("x-goog-api-key", key)
	case "api-key":
		req.Header.Set("api-key", key)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func (a *httpAdapter) reserveCredential(excluded map[int]bool) (int, string, bool) {
	return a.reserveCredentialWithSession(excluded, "")
}

// reserveCredentialWithSession is the single credential-selection
// discipline, in Stage C order:
//  1. eligibility: not excluded, not cooling (cooldown expiry restores
//     the credential with no operator action; Enabled=false entries
//     never reach the adapter at all);
//  2. minimum Priority tier among available credentials;
//  3. credential-level session-affinity pin, only while the pinned
//     credential is in the winning tier and available;
//  4. the existing two-candidate tournament within the tier, with
//     effective load Active/Weight (Weight<=0 counts as 1), then
//     fewer Failures, then never-failed.
//
// With every Priority 0 and Weight 1 this reduces exactly to the
// pre-opencode tournament: same eligible set, same seeding, same
// tiebreaks. The choice reason is recorded for the redacted explain
// surface; no key material is ever recorded.
func (a *httpAdapter) reserveCredentialWithSession(excluded map[int]bool, sessionHash string) (int, string, bool) {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	now := time.Now()
	available := make([]int, 0, len(a.creds))
	for i := range a.creds {
		if excluded[i] {
			continue
		}
		if a.creds[i].CooldownUntil.IsZero() || now.After(a.creds[i].CooldownUntil) {
			available = append(available, i)
		}
	}
	if len(available) == 0 {
		return 0, "", false
	}
	minTier := a.creds[available[0]].Priority
	for _, i := range available {
		if a.creds[i].Priority < minTier {
			minTier = a.creds[i].Priority
		}
	}
	tier := make([]int, 0, len(available))
	for _, i := range available {
		if a.creds[i].Priority == minTier {
			tier = append(tier, i)
		}
	}
	chosen := -1
	reason := "least-loaded"
	if sessionHash != "" {
		if pin, ok := a.sessionPinLocked(sessionHash, now); ok {
			for _, i := range tier {
				if i == pin && !excluded[i] {
					chosen = i
					reason = "affinity"
					break
				}
			}
		}
	}
	if chosen < 0 {
		if len(tier) == 1 {
			chosen = tier[0]
			reason = "only-available"
		} else {
			aPos := int(a.rr % uint64(len(tier)))         // #nosec G115 -- the modulo result is strictly less than len(tier).
			bPos := int((a.rr*7 + 1) % uint64(len(tier))) // #nosec G115 -- the modulo result is strictly less than len(tier).
			if bPos == aPos {
				bPos = (bPos + 1) % len(tier)
			}
			first := tier[aPos]
			second := tier[bPos]
			chosen = first
			if credentialLessLoadedWeighted(a.creds[second], a.creds[first]) {
				chosen = second
			}
		}
	}
	a.rr++
	a.creds[chosen].Active++
	if sessionHash != "" {
		a.setSessionPinLocked(sessionHash, chosen, now)
	}
	sel := CredentialSelection{Index: chosen, PoolIndex: a.creds[chosen].PoolIndex, Name: a.creds[chosen].Name, Tier: minTier, Reason: reason}
	a.lastSelMu.Lock()
	a.lastSel = sel
	a.lastSelOK = true
	a.lastSelMu.Unlock()
	return chosen, a.creds[chosen].Key, true
}

// sessionPinLocked reads a live pin. Caller holds no pin lock; this
// helper takes sessionMu. It is only called from selection, which
// holds credMu, so lock order is always credMu -> sessionMu.
func (a *httpAdapter) sessionPinLocked(sessionHash string, now time.Time) (int, bool) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.sessionPins == nil {
		return 0, false
	}
	pin, ok := a.sessionPins[sessionHash]
	if !ok || !now.Before(pin.expires) {
		return 0, false
	}
	return pin.credIdx, true
}

func (a *httpAdapter) setSessionPinLocked(sessionHash string, credIdx int, now time.Time) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.sessionPins == nil {
		a.sessionPins = map[string]sessionPin{}
	}
	if len(a.sessionPins) >= sessionPinMax {
		for k, p := range a.sessionPins {
			if !now.Before(p.expires) {
				delete(a.sessionPins, k)
			}
		}
		for k := range a.sessionPins {
			if len(a.sessionPins) < sessionPinMax {
				break
			}
			delete(a.sessionPins, k)
			break
		}
	}
	a.sessionPins[sessionHash] = sessionPin{credIdx: credIdx, expires: now.Add(sessionPinTTL)}
}

func credentialEffectiveWeight(c credentialState) float64 {
	if c.Weight <= 0 {
		return 1
	}
	return c.Weight
}

func credentialLessLoadedWeighted(a, b credentialState) bool {
	loadA := float64(a.Active) / credentialEffectiveWeight(a)
	loadB := float64(b.Active) / credentialEffectiveWeight(b)
	if loadA != loadB {
		return loadA < loadB
	}
	if a.Failures != b.Failures {
		return a.Failures < b.Failures
	}
	return a.LastStatus == 0 && b.LastStatus != 0
}

func credentialLessLoaded(a, b credentialState) bool {
	if a.Active != b.Active {
		return a.Active < b.Active
	}
	if a.Failures != b.Failures {
		return a.Failures < b.Failures
	}
	return a.LastStatus == 0 && b.LastStatus != 0
}

func (a *httpAdapter) hasAvailableCredential(excluded map[int]bool) bool {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	now := time.Now()
	for i := range a.creds {
		if excluded[i] {
			continue
		}
		if a.creds[i].CooldownUntil.IsZero() || now.After(a.creds[i].CooldownUntil) {
			return true
		}
	}
	return false
}

func (a *httpAdapter) releaseCredential(i int) {
	if i < 0 {
		return
	}
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if i >= len(a.creds) {
		return
	}
	if a.creds[i].Active > 0 {
		a.creds[i].Active--
	}
}
func (a *httpAdapter) markCredentialSuccess(i int) {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if i >= 0 && i < len(a.creds) {
		a.creds[i].Failures = 0
		a.creds[i].CooldownUntil = time.Time{}
		a.creds[i].LastStatus = 0
	}
}
func (a *httpAdapter) cooldownCredential(i, status int, retryAfter string) {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if i < 0 || i >= len(a.creds) {
		return
	}
	d := 10 * time.Minute
	switch status {
	case 402:
		d = time.Hour
	case 429:
		d = parseRetryAfterBounded(retryAfter, 60*time.Second, a.retryAfterCap)
	case 401, 403:
		d = 15 * time.Minute
	}
	a.creds[i].Failures++
	a.creds[i].LastStatus = status
	a.creds[i].CooldownUntil = time.Now().Add(d)
}

func (a *httpAdapter) nextCredentialRetryAfter() (time.Duration, bool) {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	now := time.Now()
	var min time.Duration
	found := false
	for _, c := range a.creds {
		if c.LastStatus != http.StatusTooManyRequests || c.CooldownUntil.IsZero() || !now.Before(c.CooldownUntil) {
			continue
		}
		d := time.Until(c.CooldownUntil)
		if d <= 0 {
			continue
		}
		if !found || d < min {
			min = d
			found = true
		}
	}
	return min, found
}
func (a *httpAdapter) safeSnippet(b []byte) string {
	msg := strings.TrimSpace(string(b))
	if len(msg) > 768 {
		msg = msg[:768] + "…"
	}
	a.credMu.RLock()
	keys := make([]string, 0, len(a.creds))
	for i := range a.creds {
		if a.creds[i].Key != "" {
			keys = append(keys, a.creds[i].Key)
		}
	}
	a.credMu.RUnlock()
	for _, key := range keys {
		msg = strings.ReplaceAll(msg, key, "[REDACTED]")
	}
	if k := a.p.ResolvedAPIKey(); k != "" {
		msg = strings.ReplaceAll(msg, k, "[REDACTED]")
	}
	return msg
}

func credentialRetryStatus(code int) bool {
	return code == 401 || code == 402 || code == 403 || code == 429
}
func parseRetryAfterBounded(v string, fallback, cap time.Duration) time.Duration {
	if cap <= 0 {
		cap = 60 * time.Second
	}
	if fallback <= 0 || fallback > cap {
		fallback = cap
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	if seconds, err := strconv.ParseInt(v, 10, 64); err == nil && seconds > 0 {
		maxSeconds := int64(cap / time.Second)
		if maxSeconds < 1 || seconds >= maxSeconds {
			return cap
		}
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > cap {
				return cap
			}
			return d
		}
	}
	return fallback
}

func (a *httpAdapter) Probe(ctx context.Context, model string, maxTokens int) (time.Duration, int, error) {
	// Health-check token budget is permanently pinned to exactly one token.
	// The maxTokens argument is accepted for interface compatibility but
	// deliberately ignored so that no config value, provider metadata, or
	// future caller can ever raise the cost of a synthetic probe.
	_ = maxTokens
	const probeTokens = 1
	var body map[string]any
	switch a.p.Type {
	case "bedrock":
		body = map[string]any{
			"messages":        []map[string]any{{"role": "user", "content": []map[string]any{{"text": "OK"}}}},
			"inferenceConfig": map[string]any{"maxTokens": probeTokens},
		}
	case "gemini", "vertex":
		body = map[string]any{
			"contents":         []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "OK"}}}},
			"generationConfig": map[string]any{"maxOutputTokens": probeTokens},
		}
	case "openai_responses":
		body = map[string]any{
			"model": model, "input": "Reply with the single word: OK",
			"max_output_tokens": probeTokens, "stream": false,
		}
	default:
		body = map[string]any{"model": model, "max_tokens": probeTokens, "messages": []map[string]any{{"role": "user", "content": "OK"}}, "stream": false}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return 0, 0, fmt.Errorf("probe request encode: %w", err)
	}
	start := time.Now()
	var resp *http.Response
	switch a.p.Type {
	case "bedrock":
		resp, err = a.DoPath(ctx, http.MethodPost, bedrockModelPath(model, false), b, false, nil)
	case "gemini":
		resp, err = a.DoPath(ctx, http.MethodPost, a.geminiModelPath(model, false), b, false, nil)
	case "vertex":
		resp, err = a.DoPath(ctx, http.MethodPost, a.vertexModelPath(model, false), b, false, nil)
	case "openai_responses":
		resp, err = a.DoPath(ctx, http.MethodPost, a.p.ResponsesPath, b, false, nil)
	default:
		resp, err = a.Do(ctx, b, false, nil)
	}
	lat := time.Since(start)
	if err != nil {
		return lat, 0, err
	}
	defer resp.Body.Close()
	const maxProbeResponseBytes = 1 << 20
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes+1))
	if readErr != nil {
		return lat, resp.StatusCode, fmt.Errorf("probe response read: %w", readErr)
	}
	if len(data) > maxProbeResponseBytes {
		return lat, resp.StatusCode, fmt.Errorf("probe response exceeds %d bytes", maxProbeResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return lat, resp.StatusCode, fmt.Errorf("probe http %d: %s", resp.StatusCode, a.safeSnippet(data))
	}
	if err := a.validateProbeResponse(data); err != nil {
		return lat, resp.StatusCode, err
	}
	return lat, resp.StatusCode, nil
}

func (a *httpAdapter) validateProbeResponse(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("probe returned malformed JSON: %w", err)
	}
	if raw, ok := root["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("probe returned error envelope: %s", a.safeSnippet(raw))
	}
	switch a.p.Type {
	case "bedrock":
		if raw, ok := root["output"]; !ok || len(raw) == 0 {
			return fmt.Errorf("probe returned no Bedrock output")
		}
	case "anthropic_compatible":
		var typ, role string
		if raw := root["type"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &typ)
		}
		if raw := root["role"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &role)
		}
		var content []json.RawMessage
		if raw := root["content"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &content); err != nil {
				return fmt.Errorf("probe returned invalid Anthropic content: %w", err)
			}
		}
		if typ != "message" || role != "assistant" || content == nil {
			return errors.New("probe returned an invalid Anthropic message envelope")
		}
	case "openai_responses":
		var status string
		if raw := root["status"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &status)
		}
		rawOutput, hasOutput := root["output"]
		if status == "" || !hasOutput {
			return errors.New("probe returned an invalid Responses envelope: status/output missing")
		}
		var output []json.RawMessage
		if err := json.Unmarshal(rawOutput, &output); err != nil {
			return fmt.Errorf("probe returned invalid Responses output: %w", err)
		}
	case "gemini", "vertex":
		var candidates []map[string]json.RawMessage
		raw := root["candidates"]
		if len(raw) == 0 {
			if pf, ok := root["promptFeedback"]; ok && len(pf) > 0 {
				return nil // blocked prompt is a valid Gemini envelope
			}
			return errors.New("probe returned an invalid Gemini envelope: candidates missing")
		}
		if err := json.Unmarshal(raw, &candidates); err != nil || len(candidates) == 0 {
			return fmt.Errorf("probe returned invalid Gemini candidates: %w", err)
		}
	default:
		var choices []map[string]json.RawMessage
		raw := root["choices"]
		if len(raw) == 0 {
			return errors.New("probe returned an invalid OpenAI chat-completion envelope: choices missing")
		}
		if err := json.Unmarshal(raw, &choices); err != nil {
			return fmt.Errorf("probe returned invalid OpenAI choices: %w", err)
		}
		if len(choices) == 0 || len(choices[0]["message"]) == 0 {
			return errors.New("probe returned an invalid OpenAI chat-completion envelope")
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(choices[0]["message"], &message); err != nil || message == nil {
			if err != nil {
				return fmt.Errorf("probe returned an invalid OpenAI message object: %w", err)
			}
			return errors.New("probe returned an invalid OpenAI message object")
		}
	}
	return nil
}

type releaseOnDoneBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseOnDoneBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *releaseOnDoneBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

type idleBody struct {
	rc     io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelFunc
	mu     sync.Mutex
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	b := &idleBody{rc: rc, idle: idle, cancel: cancel}
	b.timer = time.AfterFunc(idle, cancel)
	return b
}
func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		if b.timer != nil {
			b.timer.Reset(b.idle)
		}
		b.mu.Unlock()
	}
	return n, err
}
func (b *idleBody) Close() error {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	b.cancel()
	return b.rc.Close()
}
