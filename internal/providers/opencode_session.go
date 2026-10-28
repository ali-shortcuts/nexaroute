package providers

import (
	"context"
	"net/http"
	"strings"
)

// OpenCode session header plumbing.
//
// The gateway synthesizes the final x-opencode-session value in the HTTP
// layer (where the client header, the request's session identifiers and
// the virtual-key identity are visible) and carries it to the adapter on
// the request context. Carrying it on the context, rather than on a
// forwarded header, means a client cannot inject it: the adapter only
// ever trusts a value the gateway itself placed on the context, and only
// for a provider the operator explicitly opted in with
// ProviderConfig.OpencodeSession.

type opencodeSessionContextKey struct{}

// WithOpencodeSession attaches an already-synthesized, already-validated
// session value to ctx. Callers must validate with
// ValidOpencodeSessionValue first; the adapter re-validates defensively
// and drops anything invalid rather than sending it upstream.
func WithOpencodeSession(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, opencodeSessionContextKey{}, value)
}

func opencodeSessionFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(opencodeSessionContextKey{}).(string)
	return v, ok && v != ""
}

// ValidOpencodeSessionValue reports whether v is safe to send as a single
// HTTP header value: non-empty, bounded, and free of control characters
// (CR/LF included). Anything else is dropped, never repaired in place,
// so an invalid client value falls through to the next fallback source
// instead of reaching the upstream.
func ValidOpencodeSessionValue(v string) bool {
	if len(v) == 0 || len(v) > 512 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

// isOpencodeInferencePath reports whether an upstream request path is an
// inference call for this provider: a POST to a chat, messages or
// responses path. Configured paths win when set; the generic suffixes
// cover providers whose base URL already carries a prefix (for example
// https://opencode.ai/zen/go/v1) and Azure-style deployment paths.
// Deliberately excluded: models, embeddings, count_tokens, media and any
// non-POST call, even when the provider opted in.
func (a *httpAdapter) isOpencodeInferencePath(method, path string) bool {
	if method != "POST" {
		return false
	}
	candidates := []string{a.p.ChatPath, a.p.MessagesPath, a.p.ResponsesPath}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c != "" && strings.HasSuffix(path, c) {
			return true
		}
	}
	for _, suffix := range []string{"/chat/completions", "/messages", "/responses"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// applyOpencodeSession sets x-opencode-session on req when every gate
// holds: the provider opted in, the gateway placed a value on the
// context, the value is valid, and the request is an inference POST.
// It runs after applyHeaders, so the synthesized value wins over a
// statically configured provider header or an allowlisted client header
// for the same name; for any other provider the value is simply ignored.
func (a *httpAdapter) applyOpencodeSession(ctx context.Context, req *http.Request) {
	if !a.p.OpencodeSession {
		return
	}
	value, ok := opencodeSessionFromContext(ctx)
	if !ok || !ValidOpencodeSessionValue(value) {
		return
	}
	if !a.isOpencodeInferencePath(req.Method, req.URL.Path) {
		return
	}
	req.Header.Set("x-opencode-session", value)
}
