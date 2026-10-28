package httpapi

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// OpenCode session header synthesis (x-opencode-session).
//
// Fallback chain, in order:
//  1. the client's own x-opencode-session, when it is a valid single
//     header value (controls, including CR/LF, make it invalid and it
//     is ignored, never repaired or rejected with a 400);
//  2. the request's existing session identifier: the session headers
//     NexaRoute already uses for affinity, in the same order as
//     sessionKeyFromRequestParts, then the body's session_id;
//  3. a fresh UUID v4 for this request only. This last resort lets the
//     request pass the gateway, but it creates no affinity: the next
//     request gets a different value.
//
// Isolation: when the caller authenticated with a virtual key, the
// final value is "<virtual-key-id>:<session>". The prefix is the
// virtual key's non-secret ID (never the presented key or its hash),
// so sessions of different virtual keys cannot collide upstream.
// Legacy (non-virtual) identities get no prefix.
//
// The synthesized value is carried on the request context, not on a
// forwarded header, so a client cannot inject it, and the adapter only
// emits it for providers explicitly opted in with
// config.ProviderConfig.OpencodeSession and only on inference POSTs.
// The three inference handlers attach it to their route context; the
// models, count_tokens, media and embedding handlers never do, so those
// calls cannot carry the header even for an opted-in provider.

func validOpencodeSessionBase(v string) bool {
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	return providers.ValidOpencodeSessionValue(v)
}

func validVirtualKeyID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func newOpencodeSessionUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read on crypto/rand only fails on a broken OS RNG; failing
		// the synthesis is safer than emitting a predictable value.
		return ""
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// resolveOpencodeSessionValue computes the final header value for one
// request. It never returns an invalid value: on the (unreachable in
// practice) UUID-generation failure it returns "" and the adapter drops
// the header rather than sending something malformed.
func resolveOpencodeSessionValue(r *http.Request, bodySessionKey string) string {
	base := ""
	candidates := []string{
		strings.TrimSpace(r.Header.Get("x-opencode-session")),
	}
	for _, h := range []string{"x-claude-code-session-id", "x-litellm-session-id", "x-litellm-trace-id", "x-session-id"} {
		candidates = append(candidates, strings.TrimSpace(r.Header.Get(h)))
	}
	candidates = append(candidates, strings.TrimSpace(bodySessionKey))
	for _, c := range candidates {
		if validOpencodeSessionBase(c) {
			base = c
			break
		}
	}
	if base == "" {
		base = newOpencodeSessionUUID()
		if base == "" {
			return ""
		}
	}
	if id, ok := clientIdentityFromRequest(r); ok && id.Virtual && validVirtualKeyID(id.ID) {
		return id.ID + ":" + base
	}
	return base
}
