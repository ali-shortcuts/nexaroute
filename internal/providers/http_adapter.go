package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// providerDialer bounds the dial phase (DNS + TCP connect) well below the
// response-header timeout so routing blackholes fail over in seconds instead
// of hanging on Go's 30s default. Keep-alives probe idle pooled connections
// so dead peers are detected by the OS instead of stalling the next request;
// DualStack races A/AAAA resolution (happy eyeballs) instead of waiting out
// a broken IPv6 path before trying IPv4.
func providerDialer() *net.Dialer {
	return &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, DualStack: true}
}

type credentialState struct {
	Key           string
	Failures      int
	Active        int64
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
	p              config.ProviderConfig
	c              *http.Client
	streamC        *http.Client
	sem            chan struct{}
	credMu         sync.RWMutex
	creds          []credentialState
	rr             uint64
	forwardAllowed map[string]struct{}
	retryAfterCap  time.Duration
	active         atomic.Int64
	waiting        atomic.Int64
}

func newHTTPAdapter(p config.ProviderConfig, timeout time.Duration) (*httpAdapter, error) {
	return newHTTPAdapterWithRetryCap(p, timeout, 60*time.Second)
}

func newHTTPAdapterWithRetryCap(p config.ProviderConfig, timeout, retryAfterCap time.Duration) (*httpAdapter, error) {
	if retryAfterCap <= 0 {
		retryAfterCap = 60 * time.Second
	}
	mc := p.MaxConcurrency
	if mc <= 0 {
		mc = 32
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
	tr := &http.Transport{
		// MaxConnsPerHost is unlimited: the adapter semaphore is the
		// concurrency gate. Capping connections at the semaphore size
		// deadlocks a slot behind a stuck TCP conn and cannot absorb a
		// burst of Claude Code sub-agent streams.
		MaxIdleConns: maxIdle, MaxIdleConnsPerHost: idlePerHost,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second,
		DialContext:       providerDialer().DialContext,
		ForceAttemptHTTP2: true,
	}
	if p.ProxyURL != "" {
		u, err := url.Parse(p.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("provider %s proxy: %w", p.ID, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	a := &httpAdapter{
		p: p, c: &http.Client{Transport: tr, Timeout: timeout}, streamC: &http.Client{Transport: tr},
		sem: make(chan struct{}, mc), forwardAllowed: make(map[string]struct{}, len(p.ForwardHeaders)),
		retryAfterCap: retryAfterCap,
	}
	for _, h := range p.ForwardHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			a.forwardAllowed[h] = struct{}{}
		}
	}
	for _, k := range p.ResolvedCredentials() {
		a.creds = append(a.creds, credentialState{Key: k})
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
	return ProviderStats{
		ID: a.p.ID, MaxConcurrency: cap(a.sem),
		ActiveRequests: a.active.Load(), WaitingRequests: a.waiting.Load(),
		Credentials: len(a.creds), CredentialsCooling: cooling,
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
	if a.p.Type == "anthropic_compatible" {
		return a.p.MessagesPath
	}
	return a.p.ChatPath
}

func (a *httpAdapter) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return a.DoPath(ctx, http.MethodPost, a.defaultPath(), payload, stream, forward)
}

func (a *httpAdapter) CountTokens(ctx context.Context, payload []byte, forward http.Header) (*http.Response, error) {
	return a.DoPath(ctx, http.MethodPost, a.p.CountTokensPath, payload, false, forward)
}

// acquireSlot waits for a provider concurrency slot. Besides request
// cancellation, the wait is bounded by the queue timeout carried on ctx (see
// WithQueueTimeout): on expiry it reports ErrProviderSaturated so the caller
// spills over to the next candidate instead of queueing behind a saturated
// provider for the whole route budget.
func (a *httpAdapter) acquireSlot(ctx context.Context) error {
	select {
	case a.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	timeout := QueueTimeoutFrom(ctx)
	if timeout <= 0 {
		select {
		case a.sem <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case a.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("provider %s: %w", a.p.ID, ErrProviderSaturated)
	}
}

func (a *httpAdapter) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	u := endpoint(a.p.BaseURL, path)
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q", u)
	}

	a.waiting.Add(1)
	if err := a.acquireSlot(ctx); err != nil {
		a.waiting.Add(-1)
		return nil, err
	}
	a.waiting.Add(-1)
	a.active.Add(1)
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
	for len(attempted) < maxCredentialAttempts {
		idx := -1
		key := ""
		if len(a.creds) > 0 {
			var ok bool
			idx, key, ok = a.reserveCredential(attempted)
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
		a.applyAuthKey(req, key)
		if a.p.Type == "anthropic_compatible" && req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}

		client := a.c
		if stream {
			client = a.streamC
		}
		resp, err := client.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			a.releaseCredential(idx)
			release()
			return nil, err
		}
		if stream && cancel != nil {
			idle := time.Duration(a.p.StreamIdleTimeoutSeconds) * time.Second
			if idle > 0 {
				resp.Body = newIdleBody(resp.Body, idle, cancel)
			} else {
				resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
			}
		}
		if idx >= 0 && credentialRetryStatus(resp.StatusCode) {
			// Peek at the error body (replayably) before cooling the key: a
			// 429 carrying insufficient_quota is account state, not a
			// transient throttle, and earns the long quota cooldown instead
			// of the short Retry-After window.
			peeked := peekResponseBody(resp, maxCredentialPeekBytes)
			uerr := ClassifyUpstreamResponse(resp.StatusCode, peeked)
			a.cooldownCredentialFor(idx, resp.StatusCode, resp.Header.Get("Retry-After"), uerr)
			if a.hasAvailableCredential(attempted) {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				a.releaseCredential(idx)
				lastErr = fmt.Errorf("credential rejected http %d: %s", resp.StatusCode, a.safeSnippet(body))
				continue
			}
		} else if idx >= 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 && !stream {
			// A 2xx status alone proves nothing: proxies may answer 200
			// with an error envelope or a paywall message inside a
			// valid-looking completion. Only mark the credential
			// successful when the peeked body shows no logical error;
			// key-scoped logical errors (dead/quota/racing keys) cool the
			// key and rotate exactly like their non-2xx equivalents.
			peeked := peekResponseBody(resp, maxCredentialPeekBytes)
			if uerr := ClassifyUpstreamResponse(resp.StatusCode, peeked); uerr != nil {
				if uerr.Class.KeyScoped() {
					a.cooldownCredential(idx, uerr.Class.CooldownStatus(), resp.Header.Get("Retry-After"))
					if a.hasAvailableCredential(attempted) {
						_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
						resp.Body.Close()
						a.releaseCredential(idx)
						lastErr = fmt.Errorf("credential served logical %s error: %s", uerr.Class, a.safeSnippet([]byte(uerr.Message)))
						continue
					}
				} else {
					a.noteCredentialFailure(idx, resp.StatusCode)
				}
				// Fall through with the replayed body: the caller runs the
				// same classifier on the full response and fails over to
				// the next deployment.
			} else {
				a.markCredentialSuccess(idx)
			}
		} else if idx >= 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.markCredentialSuccess(idx)
			// Stream headers are only a provisional success: watch for
			// in-band error chunks and correct the credential outcome if
			// the stream carries one. The watcher is strictly
			// pass-through and disables itself on any anomaly.
			resp.Body = newSSECredentialWatchBody(resp.Body, a.sseWatchProtocol(), func(class UpstreamErrorClass) {
				if class.KeyScoped() {
					a.cooldownCredential(idx, class.CooldownStatus(), "")
				} else {
					a.noteCredentialFailure(idx, resp.StatusCode)
				}
			})
		}

		// Provider and credential capacity both stay reserved until the body is
		// fully consumed or closed. This makes key-level load balancing reflect
		// long-lived SSE streams instead of only time-to-first-byte.
		resp.Body = &releaseOnDoneBody{ReadCloser: resp.Body, release: func() {
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

func (a *httpAdapter) applyHeaders(req *http.Request, forward http.Header) {
	for k, v := range a.p.Headers {
		req.Header.Set(k, v)
	}
	for k, vals := range forward {
		lk := strings.ToLower(k)
		if _, ok := a.forwardAllowed[lk]; !ok || lk == "authorization" || lk == "x-api-key" || lk == "x-admin-key" {
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
	mode := a.p.AuthMode
	if mode == "" {
		if a.p.Type == "anthropic_compatible" {
			mode = "x-api-key"
		} else {
			mode = "bearer"
		}
	}
	if mode == "x-api-key" {
		req.Header.Set("x-api-key", key)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func (a *httpAdapter) reserveCredential(excluded map[int]bool) (int, string, bool) {
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
	chosen := available[0]
	if len(available) > 1 {
		aPos := int(a.rr % uint64(len(available)))
		bPos := int((a.rr*7 + 1) % uint64(len(available)))
		if bPos == aPos {
			bPos = (bPos + 1) % len(available)
		}
		first := available[aPos]
		second := available[bPos]
		if credentialLessLoaded(a.creds[second], a.creds[first]) {
			chosen = second
		} else {
			chosen = first
		}
	}
	a.rr++
	a.creds[chosen].Active++
	return chosen, a.creds[chosen].Key, true
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

// cooldownCredentialFor refines the cooldown bucket with the classified
// failure: quota exhaustion is account state and always earns the long
// quota cooldown, even when the provider reports it as 429.
func (a *httpAdapter) cooldownCredentialFor(i, status int, retryAfter string, uerr *UpstreamLogicalError) {
	if uerr != nil && uerr.Class == UpstreamQuota {
		status = 402
	}
	a.cooldownCredential(i, status, retryAfter)
}

// noteCredentialFailure records a deployment-scoped failure against the
// serving credential (power-of-two prefers healthier keys) without cooling
// it: the key itself is fine, the deployment is not.
func (a *httpAdapter) noteCredentialFailure(i, status int) {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if i < 0 || i >= len(a.creds) {
		return
	}
	a.creds[i].Failures++
	a.creds[i].LastStatus = status
}

func (a *httpAdapter) sseWatchProtocol() string {
	if a.p.Type == "anthropic_compatible" {
		return "anthropic"
	}
	return "openai"
}

// maxCredentialPeekBytes bounds adapter-side body inspection. Error
// envelopes and injected paywall text always sit at the start of the body,
// so a small peek classifies credential outcomes without buffering whole
// (potentially multi-megabyte) completions. Peeked bytes are replayed to
// the caller untouched.
const maxCredentialPeekBytes = 64 << 10

type replayBody struct {
	io.Reader
	io.Closer
}

func peekResponseBody(resp *http.Response, n int) []byte {
	peeked, _ := io.ReadAll(io.LimitReader(resp.Body, int64(n)))
	resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(peeked), resp.Body), Closer: resp.Body}
	return peeked
}

const maxSSEWatchLineBytes = 1 << 20

// sseCredentialWatchBody observes a streamed upstream body for in-band
// error chunks and reports the first detected failure class. It never
// alters, delays, or truncates bytes; any oversized line or internal
// anomaly permanently disables observation (pass-through from then on).
type sseCredentialWatchBody struct {
	rc       io.ReadCloser
	protocol string
	onError  func(UpstreamErrorClass)
	line     []byte
	done     bool
}

func newSSECredentialWatchBody(rc io.ReadCloser, protocol string, onError func(UpstreamErrorClass)) io.ReadCloser {
	return &sseCredentialWatchBody{rc: rc, protocol: protocol, onError: onError}
}

func (b *sseCredentialWatchBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && !b.done {
		b.scan(p[:n])
	}
	return n, err
}

func (b *sseCredentialWatchBody) Close() error { return b.rc.Close() }

func (b *sseCredentialWatchBody) scan(chunk []byte) {
	for len(chunk) > 0 && !b.done {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			if len(b.line)+len(chunk) > maxSSEWatchLineBytes {
				b.line = nil
				b.done = true
				return
			}
			b.line = append(b.line, chunk...)
			return
		}
		if len(b.line)+i > maxSSEWatchLineBytes {
			b.line = nil
			b.done = true
			return
		}
		b.line = append(b.line, chunk[:i]...)
		b.checkLine(b.line)
		b.line = b.line[:0]
		chunk = chunk[i+1:]
	}
}

func (b *sseCredentialWatchBody) checkLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if uerr := ClassifySSEData(b.protocol, data); uerr != nil {
		b.done = true
		if b.onError != nil {
			b.onError(uerr.Class)
		}
	}
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
	if maxTokens < 1 {
		maxTokens = 1
	}
	body := map[string]any{"model": model, "max_tokens": maxTokens, "messages": []map[string]any{{"role": "user", "content": "OK"}}, "stream": false}
	b, err := json.Marshal(body)
	if err != nil {
		return 0, 0, fmt.Errorf("probe request encode: %w", err)
	}
	start := time.Now()
	resp, err := a.Do(ctx, b, false, nil)
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
		class := ""
		if uerr := ClassifyUpstreamResponse(resp.StatusCode, data); uerr != nil {
			class = " (" + string(uerr.Class) + ")"
		}
		return lat, resp.StatusCode, fmt.Errorf("probe http %d%s: %s", resp.StatusCode, class, a.safeSnippet(data))
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
	// Error envelopes already failed above; the classifier additionally
	// rejects error finish reasons and injected paywall/auth/throttle text
	// inside otherwise valid-looking completions. Note the max_tokens=1
	// caveat: a provider that truncates injected text to one token defeats
	// content sniffing here, but the data plane still catches the full
	// text on the first real request and quarantines the deployment.
	if uerr := ClassifyUpstreamResponse(200, data); uerr != nil {
		return fmt.Errorf("probe detected upstream %s error: %s", uerr.Class, a.safeSnippet([]byte(uerr.Message)))
	}
	switch a.p.Type {
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
