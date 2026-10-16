package egress

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// SameOrigin reports whether two URLs share scheme and host, compared
// case-insensitively. It is the redirect-boundary predicate for the
// gateway's non-primary HTTP clients: any redirect that changes scheme
// or host is treated as cross-origin. It mirrors the rule the main
// provider adapter enforces (internal/providers/http_adapter.go), so all
// clients share one definition of the boundary.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// RefuseCrossOriginRedirects returns an http.Client CheckRedirect policy:
// at most maxRedirects hops, and any redirect that crosses an origin
// boundary (scheme or host change relative to the original request) is
// refused with an error instead of being followed. Refusing — rather
// than following with stripped headers — is the fail-closed posture:
// Go's stdlib strips only Authorization/Www-Authenticate/Cookie* on a
// cross-host redirect, while provider credential headers such as
// x-api-key, x-goog-api-key and api-key would be forwarded to the
// redirect target.
//
// service names the calling client in the returned errors (e.g.
// "budget webhook", "mcp", "comfyui").
func RefuseCrossOriginRedirects(maxRedirects int, service string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if maxRedirects > 0 && len(via) >= maxRedirects {
			return fmt.Errorf("%s: stopped after %d redirects", service, maxRedirects)
		}
		if !SameOrigin(via[0].URL, req.URL) {
			// Credentials and payloads attached to the original request
			// must never cross an origin boundary through an automatic
			// redirect.
			return fmt.Errorf("%s: refused cross-origin redirect", service)
		}
		return nil
	}
}
