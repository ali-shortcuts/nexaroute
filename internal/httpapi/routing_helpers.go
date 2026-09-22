package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)


func routeContext(parent context.Context, streaming bool, timeout time.Duration) (context.Context, context.CancelFunc) {
	if streaming || timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func gatewayDeadlineExceeded(routeCtx, clientCtx context.Context) bool {
	return routeCtx.Err() == context.DeadlineExceeded && clientCtx.Err() == nil
}

func clientRequestGone(clientCtx context.Context) bool {
	return clientCtx.Err() != nil
}

func jitteredRetryBackoff(base time.Duration, attempt int, requestID string) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	max := base << attempt
	if max > 5*time.Second {
		max = 5 * time.Second
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(requestID))
	_, _ = h.Write([]byte{byte(attempt)})
	// Full jitter in [0,max], deterministic for a request+attempt so tests and
	// incident replay remain reproducible while concurrent clients desynchronize.
	return time.Duration(h.Sum64()%uint64(max+1))
}

func patchJSONModel(raw []byte, model string) ([]byte, error) {
	if !utf8.ValidString(model) {
		return nil, fmt.Errorf("model id is not valid UTF-8")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	obj["model"] = model
	return json.Marshal(obj)
}

func retryable(code int) bool {
	return code == 408 || code == 409 || code == 425 || code == 429 || code == 500 || code == 502 || code == 503 || code == 504 || code == 529
}
func failoverEligible(code int) bool {
	// Upstream auth/quota/not-found failures are deployment/provider failures from
	// the gateway's perspective. Trying another configured deployment is useful
	// and does not repeat the same failing upstream. 400/422 are intentionally
	// excluded because they usually indicate a request that every provider would
	// reject in the same way.
	return retryable(code) || code == 401 || code == 402 || code == 403 || code == 404
}
func hardCooldownStatus(code int) bool {
	return code == 401 || code == 402 || code == 403 || code == 429
}

func retryAfterDuration(h http.Header, max time.Duration) time.Duration {
	d := time.Duration(0)
	v := strings.TrimSpace(h.Get("Retry-After"))
	if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
		d = time.Duration(sec) * time.Second
	}
	if d == 0 && v != "" {
		if t, err := http.ParseTime(v); err == nil && t.After(time.Now()) {
			d = time.Until(t)
		}
	}
	if d <= 0 {
		d = 30 * time.Second
	}
	if max > 0 && d > max {
		d = max
	}
	return d
}

func redactProviderBody(p config.ProviderConfig, b []byte) []byte {
	out := append([]byte(nil), b...)
	for _, key := range p.ResolvedCredentials() {
		if key == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(key), []byte("[REDACTED]"))
	}
	return out
}

func redactProviderSecrets(cfg config.Config, providerID string, b []byte) []byte {
	if i := cfg.ProviderIndex(providerID); i >= 0 {
		return redactProviderBody(cfg.Providers[i], b)
	}
	return append([]byte(nil), b...)
}

func upstreamError(status int, b []byte) string {
	msg := strings.TrimSpace(string(b))
	if len(msg) > 1024 {
		msg = msg[:1024] + "…"
	}
	return fmt.Sprintf("upstream %d: %s", status, msg)
}

func copySelectedRequestHeaders(r *http.Request) http.Header {
	h := make(http.Header)
	// Copy non-sensitive client headers into an intermediate set. The provider
	// adapter still applies its explicit ForwardHeaders allowlist, so a provider
	// only receives headers the user chose to forward. Keeping the broader set
	// here allows provider-specific Anthropic beta/version headers and custom
	// compatibility headers without hard-coding every future header name.
	blocked := map[string]bool{
		"authorization": true, "proxy-authorization": true, "x-api-key": true,
		"x-admin-key": true, "cookie": true, "set-cookie": true, "connection": true,
		"proxy-connection": true, "transfer-encoding": true, "content-length": true,
		"host": true,
	}
	for k, vals := range r.Header {
		if blocked[strings.ToLower(strings.TrimSpace(k))] {
			continue
		}
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	return h
}
