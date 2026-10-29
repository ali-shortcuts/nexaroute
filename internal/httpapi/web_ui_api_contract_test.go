package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	videoapi "github.com/ali-shortcuts/nexaroute/internal/video/api"
)

// Phase 13 (#413): frontend/backend API route contract.
//
// The page-registry, OpenAPI and i18n contracts pin the UI's structure and
// the spec's fidelity to the handlers. This contract closes the remaining
// side of the triangle: every API endpoint the embedded web UI actually
// calls must resolve to a real handler with the HTTP method the UI uses.
// A UI call to a nonexistent handler or method fails here instead of
// shipping a dead button.
//
// Method: statically extract (method, path) call sites from web/app.js —
// api()/videoApi()/fetch() with /admin/ or /v1/ URL expressions, including
// template literals, string concatenation, conditional URLs and conditional
// methods — then probe each pair against a real in-process server:
//   - /admin/* with a full-power admin session + CSRF pair, reusing the
//     probe the OpenAPI contract test uses (POST/PUT/DELETE carry a
//     deliberately malformed JSON body so a config-rewriting handler
//     rejects the probe with 400 before applying anything);
//   - /v1/* (data plane) unauthenticated: a 401/400/JSON-404 still proves
//     the route exists; only 405 or the mux's plain-text 404 means the UI
//     calls something the server does not implement.
// The SSE endpoint is probed with a cancellable context since it streams.
//
// What this deliberately does NOT cover: real-browser interaction flows
// (first-run empty state, provider add/edit, video job progress, RTL/EN/FA/PS
// rendering). Those need a browser that can reach the loopback test server,
// which the sandbox blocks (verified: headless Chromium 152 cannot load
// http://127.0.0.1/ here while curl can); they are tracked separately.

type webAPICall struct {
	method string
	path   string
}

// webAPIProbeToken stands in for runtime URL segments (ids, cursors) when a
// UI-built path is probed. Handlers answer unknown ids with a JSON 404,
// which counts as "route exists" — only the mux's plain-text 404 means the
// route itself is missing.
const webAPIProbeToken = "contractprobe"

var webAPICallSiteRE = regexp.MustCompile(`\b(?:api|videoApi|fetch)\(`)

// jsStringAt returns the unquoted content of the string literal starting at
// js[i] (single, double or backtick quote) and the index just past it.
func jsStringAt(js string, i int) (string, int) {
	q := js[i]
	var b strings.Builder
	for j := i + 1; j < len(js); j++ {
		c := js[j]
		if c == '\\' && j+1 < len(js) {
			b.WriteByte(js[j+1])
			j++
			continue
		}
		if c == q {
			return b.String(), j + 1
		}
		b.WriteByte(c)
	}
	return b.String(), len(js)
}

// expandTemplate replaces ${...} substitutions in a template literal's inner
// content with the probe token, skipping balanced braces.
func expandTemplate(inner string) string {
	var b strings.Builder
	for i := 0; i < len(inner); {
		if strings.HasPrefix(inner[i:], "${") {
			depth := 1
			j := i + 2
			for j < len(inner) && depth > 0 {
				switch inner[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
				j++
			}
			b.WriteString(webAPIProbeToken)
			i = j
			continue
		}
		b.WriteByte(inner[i])
		i++
	}
	return b.String()
}

// scanJS calls fn for every byte of s outside string/template literals;
// depth tracks (), [], {} nesting. It reports the index where fn returns
// false, or -1.
func scanJS(s string, fn func(i int, c byte, depth int) bool) int {
	var q byte
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			q = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		default:
			if !fn(i, c, depth) {
				return i
			}
		}
	}
	return -1
}

// findTopLevel returns the index of the first target byte at depth 0.
func findTopLevel(s string, target byte) int {
	return scanJS(s, func(i int, c byte, depth int) bool {
		return c != target || depth != 0
	})
}

// findTopLevelQ finds a ternary '?' at depth 0, skipping '?.' and '??'.
func findTopLevelQ(s string) int {
	return scanJS(s, func(i int, c byte, depth int) bool {
		if c == '?' && depth == 0 && i+1 < len(s) && s[i+1] != '.' && s[i+1] != '?' {
			return false
		}
		return true
	})
}

// splitTernary splits "cond ? then : els" at the ':' matching the first
// top-level '?'. Nested ternaries are handled via a ternary-depth counter.
func splitTernary(s string) (cond, then, els string, ok bool) {
	qi := findTopLevelQ(s)
	if qi < 0 {
		return "", "", "", false
	}
	tdepth := 0
	idx := scanJS(s[qi+1:], func(i int, c byte, depth int) bool {
		if depth != 0 {
			return true
		}
		switch c {
		case '?':
			// '?.' and '??' were already excluded by findTopLevelQ's rule;
			// re-apply it here for nested occurrences.
			if i+1 < len(s[qi+1:]) && (s[qi+1+i+1] == '.' || s[qi+1+i+1] == '?') {
				return true
			}
			tdepth++
			return true
		case ':':
			if tdepth == 0 {
				return false
			}
			tdepth--
			return true
		}
		return true
	})
	if idx < 0 {
		return "", "", "", false
	}
	ci := qi + 1 + idx
	return s[:qi], s[qi+1 : ci], s[ci+1:], true
}

// ternaryCond reports the trimmed condition of a top-level ternary.
func ternaryCond(s string) (string, bool) {
	c, _, _, ok := splitTernary(strings.TrimSpace(s))
	return strings.TrimSpace(c), ok
}

// splitTopLevel splits s on sep at depth 0.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	rest := s
	for {
		idx := findTopLevel(rest, sep)
		if idx < 0 {
			parts = append(parts, rest)
			return parts
		}
		parts = append(parts, rest[:idx])
		rest = rest[idx+1:]
	}
}

// matchParen returns the index of the ')' matching s[open] == '('. It uses
// its own scanner because scanJS consumes brackets for depth tracking and
// never reports them to the callback.
func matchParen(s string, open int) int {
	var q byte
	depth := 0
	for i := open; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			q = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// parseURLExpr evaluates the small expression grammar the UI uses for API
// URLs: string/template literals, top-level '+' concatenation, the ternary
// operator, and encodeURIComponent(...) placeholders. It returns the
// concrete probe paths, or nil when the URL is dynamically computed.
func parseURLExpr(expr string) []string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "(") && matchParen(expr, 0) == len(expr)-1 {
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	if _, then, els, ok := splitTernary(expr); ok {
		var out []string
		out = append(out, parseURLExpr(then)...)
		out = append(out, parseURLExpr(els)...)
		if out == nil {
			return nil
		}
		return out
	}
	var sb strings.Builder
	for _, piece := range splitTopLevel(expr, '+') {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		switch piece[0] {
		case '\'', '"', '`':
			lit, _ := jsStringAt(piece, 0)
			if piece[0] == '`' {
				lit = expandTemplate(lit)
			}
			sb.WriteString(lit)
		default:
			if strings.Contains(piece, "encodeURIComponent(") {
				sb.WriteString(webAPIProbeToken)
			} else {
				return nil
			}
		}
	}
	return []string{sb.String()}
}

var webAPIMethodKeyRE = regexp.MustCompile(`\bmethod\s*:`)

// methodValueExpr returns the raw text of the value after "method:" in the
// call's options expression, or "" when the call passes no method.
func methodValueExpr(optExpr string) string {
	loc := webAPIMethodKeyRE.FindStringIndex(optExpr)
	if loc == nil {
		return ""
	}
	return strings.TrimSpace(optExpr[loc[1]:])
}

// parseMethodValue reads the value after "method:" — a literal, or a ternary
// of literals (e.g. method: id ? 'PUT' : 'POST').
func parseMethodValue(val string) []string {
	val = strings.TrimSpace(val)
	if val == "" {
		return nil
	}
	switch val[0] {
	case '\'', '"', '`':
		lit, _ := jsStringAt(val, 0)
		return []string{strings.ToUpper(lit)}
	}
	if _, then, els, ok := splitTernary(val); ok {
		return append(parseMethodValue(then), parseMethodValue(els)...)
	}
	return nil
}

// extractWebAPICalls finds every api()/videoApi()/fetch() call site in the
// UI source whose URL expression statically resolves to an /admin/ or /v1/
// path, paired with the HTTP method(s) the call site uses.
func extractWebAPICalls(t *testing.T, js string) []webAPICall {
	t.Helper()
	seen := map[webAPICall]bool{}
	var out []webAPICall
	var skipped []string
	for _, loc := range webAPICallSiteRE.FindAllStringIndex(js, -1) {
		// Skip the api()/videoApi() function *definitions*.
		if loc[0] >= len("async function ") && js[loc[0]-len("async function "):loc[0]] == "async function " {
			continue
		}
		open := loc[1] - 1
		close := matchParen(js, open)
		if close < 0 {
			continue
		}
		args := js[open+1 : close]
		comma := findTopLevel(args, ',')
		var urlExpr, optExpr string
		if comma < 0 {
			urlExpr = args
		} else {
			urlExpr, optExpr = args[:comma], args[comma+1:]
		}
		urls := parseURLExpr(urlExpr)
		if urls == nil {
			skipped = append(skipped, strings.TrimSpace(urlExpr))
			continue
		}
		methodVal := methodValueExpr(optExpr)
		var methods []string
		if methodVal != "" {
			methods = parseMethodValue(methodVal)
		} else {
			methods = []string{http.MethodGet}
		}
		if methods == nil {
			skipped = append(skipped, strings.TrimSpace(urlExpr)+" (dynamic method)")
			continue
		}
		// Pair (method, path) candidates. When the URL and the method are
		// ternaries over the same condition (e.g. id ? PUT-on-/providers/{id}
		// : POST-on-/providers), the branches are correlated: pair
		// then-with-then and else-with-else. The Cartesian product would
		// invent call sites the UI never makes.
		type pair struct{ method, path string }
		var pairs []pair
		if uCond, uOk := ternaryCond(urlExpr); uOk {
			if mCond, mOk := ternaryCond(methodVal); mOk && uCond == mCond && len(urls) == 2 && len(methods) == 2 {
				pairs = append(pairs, pair{methods[0], urls[0]}, pair{methods[1], urls[1]})
			}
		}
		if pairs == nil {
			for _, u := range urls {
				for _, m := range methods {
					pairs = append(pairs, pair{m, u})
				}
			}
		}
		for _, pr := range pairs {
			u := pr.path
			if i := strings.Index(u, "?"); i >= 0 {
				u = u[:i]
			}
			if !strings.HasPrefix(u, "/admin/") && !strings.HasPrefix(u, "/v1/") {
				continue
			}
			c := webAPICall{method: pr.method, path: u}
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	if len(skipped) > 0 {
		t.Logf("skipped %d dynamically-computed call sites: %q", len(skipped), skipped)
	}
	return out
}

// doStream probes the SSE endpoint with a cancellable context: the handler
// streams until the request context is done, so a plain probe would block.
func (p *adminProbe) doStream(path string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil).WithContext(ctx)
	req.RemoteAddr = p.ip + ":10001"
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: p.session})
	rr := httptest.NewRecorder()
	p.handler.ServeHTTP(rr, req)
	return rr
}

// probeWebAPICall issues one in-process request for a UI (method, path)
// pair. A fresh server per probe keeps each answer attributable: several
// handlers legitimately mutate configuration, and logout destroys the probe
// session. The /v1/video/ prefix is served by an optionally attached
// handler; the probe attaches the real video API handler (with no
// dependencies, so sub-handlers answer 503/JSON-404 — both prove the route
// exists) exactly as cmd/gateway does in production.
func probeWebAPICall(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	srv := testGateway(t, config.Default())
	if strings.HasPrefix(path, "/v1/video/") {
		srv.AttachVideoHandler(&videoapi.Handler{})
	}
	if strings.HasPrefix(path, "/admin/api/nexaflow/") {
		task8AttachProbeService(t, srv)
	}
	if strings.HasPrefix(path, "/admin/") {
		p := newAdminProbe(t, srv)
		if path == "/admin/api/events/stream" {
			return p.doStream(path)
		}
		csrf := method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
		return p.do(method, path, csrf)
	}
	req := httptest.NewRequest(method, "http://gateway"+path, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func TestWebUIAPICallsResolveToHandlers(t *testing.T) {
	js := readAppJS(t)
	calls := extractWebAPICalls(t, js)
	// Guard against a silently broken extractor: the UI currently makes
	// ~35 API calls; far fewer means the scan broke, not the UI.
	if len(calls) < 20 {
		t.Fatalf("extracted only %d UI API call sites; the extractor is broken, not the UI", len(calls))
	}
	t.Logf("probing %d distinct UI API (method, path) pairs", len(calls))
	for _, c := range calls {
		rr := probeWebAPICall(t, c.method, c.path)
		if !implemented(rr) {
			t.Errorf("UI calls %s %s but no handler implements it (probe answered %d %q)",
				c.method, c.path, rr.Code, truncateForLog(rr.Body.String(), 80))
		}
	}
}

// TestWebAPIProbeDetectsMissingRoute is the red side of the contract: it
// proves the probe machinery actually reports a route the server does not
// implement, so TestWebUIAPICallsResolveToHandlers cannot pass vacuously.
func TestWebAPIProbeDetectsMissingRoute(t *testing.T) {
	for _, tc := range []webAPICall{
		{http.MethodGet, "/admin/api/this-route-does-not-exist-413"},
		{http.MethodPost, "/admin/api/this-route-does-not-exist-413"},
		{http.MethodGet, "/v1/this-route-does-not-exist-413"},
	} {
		if rr := probeWebAPICall(t, tc.method, tc.path); implemented(rr) {
			t.Errorf("probe reports %s %s as implemented; the detector is broken", tc.method, tc.path)
		}
	}
}
