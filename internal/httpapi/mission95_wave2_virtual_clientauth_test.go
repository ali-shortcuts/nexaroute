package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave-2 behavior tests: error handling, validation, auth-failure, and
// edge-case branches in virtual admin handlers and client-auth helpers that
// the merged wave-1 candidate left uncovered. Every test asserts observable
// behavior (status codes, bodies, returned values, state transitions).

// ---------- client key extraction / comparison ----------

func TestWave2ExtractClientKey(t *testing.T) {
	newReq := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://localhost/v1/chat/completions", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	if got := extractClientKey(newReq(map[string]string{"X-Api-Key": "  key-1  "})); got != "key-1" {
		t.Fatalf("x-api-key: got %q want %q", got, "key-1")
	}
	if got := extractClientKey(newReq(map[string]string{"Authorization": "BEARER tok-2  "})); got != "tok-2" {
		t.Fatalf("bearer (case-insensitive, trimmed): got %q want %q", got, "tok-2")
	}
	// A non-Bearer scheme is returned verbatim: the caller decides whether
	// it is a usable key; silently dropping it would change auth semantics.
	if got := extractClientKey(newReq(map[string]string{"Authorization": "Basic abc"})); got != "Basic abc" {
		t.Fatalf("non-bearer auth: got %q want %q", got, "Basic abc")
	}
	if got := extractClientKey(newReq(nil)); got != "" {
		t.Fatalf("no credentials: got %q want empty", got)
	}
}

func TestWave2KeyComparisonHelpers(t *testing.T) {
	if !subtleConstantTimeCompare("abc", "abc") {
		t.Error("equal strings should compare equal")
	}
	if subtleConstantTimeCompare("abc", "abd") {
		t.Error("same-length differing strings should compare unequal")
	}
	if subtleConstantTimeCompare("abc", "abcd") {
		t.Error("different-length strings should compare unequal without panicking")
	}
	if !clientKeyMatches([]string{"other", "presented"}, "presented") {
		t.Error("clientKeyMatches should match a listed key")
	}
	if clientKeyMatches([]string{"other"}, "presented") {
		t.Error("clientKeyMatches should reject an unlisted key")
	}
	if clientKeyMatches(nil, "presented") {
		t.Error("clientKeyMatches should reject with no keys configured")
	}
	digest := keyDigest("presented")
	if !digestInList([]string{"nope", digest}, digest) {
		t.Error("digestInList should match a stored digest")
	}
	if digestInList([]string{"nope"}, digest) {
		t.Error("digestInList should reject an unknown digest")
	}
	if got := keyDigest(""); len(got) != 64 {
		t.Fatalf("keyDigest should be hex SHA-256, got %q", got)
	}
}

// ---------- request prefix / top-level JSON inspection ----------

type wave2ErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *wave2ErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestWave2ReadRequestPrefix(t *testing.T) {
	if prefix, truncated, err := readRequestPrefix(nil, 10); prefix != nil || truncated || err != nil {
		t.Fatalf("nil request: got (%v, %v, %v) want (nil, false, nil)", prefix, truncated, err)
	}
	nilBody := httptest.NewRequest(http.MethodPost, "http://localhost/x", nil)
	nilBody.Body = nil
	if prefix, truncated, err := readRequestPrefix(nilBody, 10); prefix != nil || truncated || err != nil {
		t.Fatalf("nil body: got (%v, %v, %v) want (nil, false, nil)", prefix, truncated, err)
	}
	// Normal read restores the full body for the data plane.
	req := httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader("0123456789"))
	prefix, truncated, err := readRequestPrefix(req, 100)
	if err != nil || truncated || string(prefix) != "0123456789" {
		t.Fatalf("full read: prefix=%q truncated=%v err=%v", prefix, truncated, err)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil || string(rest) != "0123456789" {
		t.Fatalf("body not restored after inspection: %q err=%v", rest, err)
	}
	// Truncated read returns only the prefix but still restores everything.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader("0123456789"))
	prefix, truncated, err = readRequestPrefix(req, 4)
	if err != nil || !truncated || string(prefix) != "0123" {
		t.Fatalf("truncated read: prefix=%q truncated=%v err=%v", prefix, truncated, err)
	}
	rest, err = io.ReadAll(req.Body)
	if err != nil || string(rest) != "0123456789" {
		t.Fatalf("body not restored after truncated inspection: %q err=%v", rest, err)
	}
	// A read error is reported and the consumed bytes stay in front of the
	// body: inspection must not mutate the downstream request.
	boom := errors.New("boom")
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", nil)
	req.Body = io.NopCloser(&wave2ErrorReader{data: []byte("abc"), err: boom})
	prefix, truncated, err = readRequestPrefix(req, 100)
	if !errors.Is(err, boom) || prefix != nil || truncated {
		t.Fatalf("error read: prefix=%v truncated=%v err=%v", prefix, truncated, err)
	}
}

func TestWave2RequestTopLevelValues(t *testing.T) {
	// Duplicate keys are retained, in order: max-int enforcement must see all.
	values, ok := requestTopLevelValues([]byte(`{"model":"a","model":"b"}`), "model")
	if !ok || len(values) != 2 || string(values[0]) != `"a"` || string(values[1]) != `"b"` {
		t.Fatalf("duplicates: values=%v ok=%v", values, ok)
	}
	for _, bad := range []string{``, `not json`, `[1,2]`, `{"a":1, "b":2`, `{1:2}`, `{"a":`, `{"a":1`, `{"a":1} {}`} {
		if _, ok := requestTopLevelValues([]byte(bad), "a"); ok {
			t.Errorf("malformed %q should not parse", bad)
		}
	}
	if values, ok := requestTopLevelValues([]byte(`{"a":1,"b":2}`), "missing"); !ok || len(values) != 0 {
		t.Fatalf("absent field: values=%v ok=%v", values, ok)
	}
}

func TestWave2RequestTopLevelStringAndMaxInt(t *testing.T) {
	if s, ok := requestTopLevelString([]byte(`{"model":"  gpt-x "}`), "model"); !ok || s != "gpt-x" {
		t.Fatalf("string field: got (%q, %v)", s, ok)
	}
	if _, ok := requestTopLevelString([]byte(`{"model":123}`), "model"); ok {
		t.Error("non-string model value should not parse as a string")
	}
	if _, ok := requestTopLevelString([]byte(`{"model":"a","model":"b"}`), "model"); ok {
		t.Error("duplicate model field must be rejected, not first-wins")
	}
	if _, ok := requestTopLevelString([]byte(`{}`), "model"); ok {
		t.Error("absent model field should not parse")
	}
	if v, ok := requestTopLevelMaxInt([]byte(`{"max_tokens":5,"max_tokens":9}`), "max_tokens"); !ok || v != 9 {
		t.Fatalf("max-int must take the maximum duplicate, got (%d, %v)", v, ok)
	}
	if _, ok := requestTopLevelMaxInt([]byte(`{"max_tokens":-5}`), "max_tokens"); ok {
		t.Error("negative token value should be rejected")
	}
	if _, ok := requestTopLevelMaxInt([]byte(`{"max_tokens":"lots"}`), "max_tokens"); ok {
		t.Error("non-integer token value should be rejected")
	}
	if v, ok := requestTopLevelMaxInt([]byte(`{}`), "max_tokens"); !ok || v != 0 {
		t.Fatalf("absent field: got (%d, %v) want (0, true)", v, ok)
	}
}

func TestWave2RequestModel(t *testing.T) {
	if got := requestModel(nil); got != "" {
		t.Fatalf("nil request: got %q", got)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "http://localhost/x", strings.NewReader(`{"model":"m"}`))
		if got := requestModel(req); got != "" {
			t.Fatalf("%s: got %q want empty", method, got)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{"model":"gpt-x","messages":[]}`))
	if got := requestModel(req); got != "gpt-x" {
		t.Fatalf("model: got %q want %q", got, "gpt-x")
	}
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{broken`))
	if got := requestModel(req); got != "" {
		t.Fatalf("malformed body: got %q want empty", got)
	}
	// A body beyond the inspection limit cannot prove its model: fail closed.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(strings.Repeat("x", (2<<20)+1)))
	if got := requestModel(req); got != "" {
		t.Fatalf("truncated body: got %q want empty", got)
	}
}

func TestWave2RequestTokenEstimate(t *testing.T) {
	if got := requestTokenEstimate(nil); got != 1 {
		t.Fatalf("nil request: got %d want 1", got)
	}
	// Empty object: no output allowance, tiny input -> estimate clamps to 1.
	req := httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{}`))
	if got := requestTokenEstimate(req); got != 1 {
		t.Fatalf("empty object: got %d want 1", got)
	}
	// max_output_tokens wins over max_tokens when larger.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{"max_tokens":10,"max_output_tokens":40}`))
	if got := requestTokenEstimate(req); got != 40+len(`{"max_tokens":10,"max_output_tokens":40}`)/4 {
		t.Fatalf("max-output wins: got %d", got)
	}
	// Declared ContentLength larger than the bytes read drives the estimate.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{}`))
	req.ContentLength = 100
	if got := requestTokenEstimate(req); got != 25 {
		t.Fatalf("content-length estimate: got %d want 25", got)
	}
	// Absurd ContentLength is capped instead of overflowing the estimate.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{}`))
	req.ContentLength = (1 << 30) + 100
	if got := requestTokenEstimate(req); got != 1<<28 {
		t.Fatalf("capped content-length estimate: got %d want %d", got, 1<<28)
	}
	// Absurd output allowance is capped at 2^30.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{"max_tokens":2000000000}`))
	if got := requestTokenEstimate(req); got != 1<<30 {
		t.Fatalf("capped output estimate: got %d want %d", got, 1<<30)
	}
	// Malformed or truncated bodies fail closed to the maximum estimate.
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(`{`))
	if got := requestTokenEstimate(req); got != 1<<30 {
		t.Fatalf("malformed body: got %d want %d", got, 1<<30)
	}
	req = httptest.NewRequest(http.MethodPost, "http://localhost/x", strings.NewReader(strings.Repeat("x", (2<<20)+1)))
	if got := requestTokenEstimate(req); got != 1<<30 {
		t.Fatalf("truncated body: got %d want %d", got, 1<<30)
	}
}

// ---------- token buckets / TPM ----------

func TestWave2ClientBucketLocked(t *testing.T) {
	s := &Server{}
	now := time.Now()
	b := s.clientBucketLocked("k1", 10, now)
	if b == nil || b.tokens != 10 || b.capacity != 10 {
		t.Fatalf("new bucket: %+v", b)
	}
	// A capacity change starts a new quota generation and clamps tokens.
	b.tokens = 99
	b2 := s.clientBucketLocked("k1", 5, now)
	if b2 != b || b.tokens != 5 || b.capacity != 5 {
		t.Fatalf("capacity change: tokens=%v capacity=%v", b.tokens, b.capacity)
	}
	// Idleness past the idle window refills the bucket and bumps generation.
	gen := b.generation
	b3 := s.clientBucketLocked("k1", 5, now.Add(11*time.Minute))
	if b3 != b || b.tokens != 5 || b.last != now.Add(11*time.Minute) || b.generation == gen {
		t.Fatalf("idle reset: tokens=%v generation %v -> %v", b.tokens, gen, b.generation)
	}
	// A full table evicts only stale buckets; a full table of fresh buckets
	// fails closed for a new identity.
	s2 := &Server{}
	for i := 0; i < maxClientBuckets; i++ {
		s2.clientBucketLocked(string(rune('a'+i%26))+string(rune('0'+i%10))+itoa(i), 1, now.Add(-11*time.Minute))
	}
	if got := s2.clientBucketLocked("new-after-stale", 1, now); got == nil {
		t.Fatal("stale buckets should be evicted to admit a new identity")
	}
	s3 := &Server{}
	for i := 0; i < maxClientBuckets; i++ {
		s3.clientBucketLocked(itoa(i), 1, now)
	}
	if got := s3.clientBucketLocked("new-when-full", 1, now); got != nil {
		t.Fatal("a full table of fresh buckets must fail closed")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func TestWave2ClientTPMReserveAt(t *testing.T) {
	s := &Server{}
	now := time.Now()
	if res, ok, wait := s.clientTPMReserveAt("k", 0, 10, now); res != nil || !ok || wait != 0 {
		t.Fatalf("tpm<=0: got (%v, %v, %v)", res, ok, wait)
	}
	if res, ok, wait := s.clientTPMReserveAt("k", 100, 0, now); res != nil || !ok || wait != 0 {
		t.Fatalf("estimated<=0: got (%v, %v, %v)", res, ok, wait)
	}
	if res, ok, wait := s.clientTPMReserveAt("k", 100, 101, now); res != nil || ok || wait != 0 {
		t.Fatalf("estimate over capacity: got (%v, %v, %v)", res, ok, wait)
	}
	res, ok, wait := s.clientTPMReserveAt("k", 100, 60, now)
	if !ok || res == nil || wait != 0 || res.charged != 60 {
		t.Fatalf("reserve: got (%v, %v, %v)", res, ok, wait)
	}
	// 40 tokens remain; asking for 60 more must wait, not overdraw.
	if res2, ok2, wait2 := s.clientTPMReserveAt("k", 100, 60, now); res2 != nil || ok2 || wait2 <= 0 {
		t.Fatalf("overdraw: got (%v, %v, %v)", res2, ok2, wait2)
	}
	// A full bucket table fails the reservation closed.
	s2 := &Server{}
	for i := 0; i < maxClientBuckets; i++ {
		s2.clientBucketLocked(itoa(i), 1, now)
	}
	if res, ok, _ := s2.clientTPMReserveAt("other", 100, 10, now); res != nil || ok {
		t.Fatalf("full table reserve: got (%v, %v)", res, ok)
	}
}

func TestWave2ClientTPMSettleAt(t *testing.T) {
	now := time.Now()
	s := &Server{clientBuckets: map[string]*clientBucket{}}
	// Nil reservation and negative actuals are no-ops, never panics.
	s.clientTPMSettleAt(nil, 10, now)
	s.clientTPMSettleAt(&tpmReservation{bucketID: "x", generation: 1}, -1, now)
	// Wrong generation (evicted/recreated bucket) settles nothing.
	s.clientBuckets["b1"] = &clientBucket{tokens: 50, last: now, capacity: 100, generation: 2}
	s.clientTPMSettleAt(&tpmReservation{bucketID: "b1", generation: 1, charged: 10, capacity: 100}, 5, now)
	if s.clientBuckets["b1"].tokens != 50 {
		t.Fatalf("stale generation settled: tokens=%v", s.clientBuckets["b1"].tokens)
	}
	// A bucket with zero recorded capacity falls back to the reservation's.
	s.clientBuckets["b2"] = &clientBucket{tokens: 10, last: now, capacity: 0, generation: 1}
	s.clientTPMSettleAt(&tpmReservation{bucketID: "b2", generation: 1, charged: 20, capacity: 100}, 5, now)
	if s.clientBuckets["b2"].tokens != 25 {
		t.Fatalf("capacity fallback settle: tokens=%v want 25", s.clientBuckets["b2"].tokens)
	}
	// Refunds clamp at capacity; overuse clamps at zero.
	s.clientBuckets["b3"] = &clientBucket{tokens: 95, last: now, capacity: 100, generation: 1}
	s.clientTPMSettleAt(&tpmReservation{bucketID: "b3", generation: 1, charged: 20, capacity: 100}, 5, now)
	if s.clientBuckets["b3"].tokens != 100 {
		t.Fatalf("refund clamp: tokens=%v want 100", s.clientBuckets["b3"].tokens)
	}
	s.clientBuckets["b4"] = &clientBucket{tokens: 5, last: now, capacity: 100, generation: 1}
	s.clientTPMSettleAt(&tpmReservation{bucketID: "b4", generation: 1, charged: 10, capacity: 100}, 30, now)
	if s.clientBuckets["b4"].tokens != 0 {
		t.Fatalf("overuse clamp: tokens=%v want 0", s.clientBuckets["b4"].tokens)
	}
}

// ---------- identity policy ----------

func TestWave2IdentityPolicyAllows(t *testing.T) {
	nonVirtual := clientIdentity{ID: "legacy", Role: "admin"}
	if !identityPolicyAllows(config.Config{}, nonVirtual, config.VirtualKeyConfig{}, "/v1/chat/completions", "m") {
		t.Error("non-virtual identities are unconstrained by key policy")
	}
	key := config.VirtualKeyConfig{AllowedModels: []string{"allowed"}}
	if identityPolicyAllows(config.Config{}, clientIdentity{Virtual: true}, key, "/v1/chat/completions", "denied") {
		t.Error("model outside the key allow-list must be denied")
	}
	if !identityPolicyAllows(config.Config{}, clientIdentity{Virtual: true}, key, "/v1/chat/completions", "allowed") {
		t.Error("model inside the key allow-list must be allowed")
	}
	// Tenant hierarchy: a key naming a configured tenant but no project is
	// allowed at tenant scope; a team outside the project is denied.
	cfg := config.Config{}
	cfg.ClientAuth.Tenants = []config.TenantConfig{{
		ID: "t1",
		Projects: []config.ProjectConfig{{
			ID: "p1",
			Teams: []config.TeamConfig{
				{ID: "team-a", AllowedModels: []string{"m"}},
			},
		}},
	}}
	tenantKey := config.VirtualKeyConfig{TenantID: "t1"}
	if !identityPolicyAllows(cfg, clientIdentity{Virtual: true}, tenantKey, "/x", "m") {
		t.Error("tenant-scoped key without a project should be allowed")
	}
	teamOutside := config.VirtualKeyConfig{TenantID: "t1", ProjectID: "p1", TeamID: "team-z"}
	if identityPolicyAllows(cfg, clientIdentity{Virtual: true}, teamOutside, "/x", "m") {
		t.Error("a team outside the selected project must be denied")
	}
	teamInside := config.VirtualKeyConfig{TenantID: "t1", ProjectID: "p1", TeamID: "team-a"}
	if !identityPolicyAllows(cfg, clientIdentity{Virtual: true}, teamInside, "/x", "m") {
		t.Error("a team inside the selected project should be allowed")
	}
	if identityPolicyAllows(cfg, clientIdentity{Virtual: true}, config.VirtualKeyConfig{TenantID: "nope"}, "/x", "m") {
		t.Error("an unconfigured tenant must be denied")
	}
}

// ---------- guardrails + client auth admission ----------

func TestWave2ApplyGuardrailsOverLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Guardrails.Mode = "block"
	cfg.Guardrails.MaxBodyBytes = 4
	s := testGateway(t, cfg)
	for _, anthropicStyle := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader("12345678"))
		rr := httptest.NewRecorder()
		if s.applyGuardrails(rr, req, anthropicStyle) {
			t.Fatalf("anthropicStyle=%v: over-limit body should be rejected", anthropicStyle)
		}
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("anthropicStyle=%v: status %d want 413", anthropicStyle, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "guardrail body limit") {
			t.Fatalf("anthropicStyle=%v: body %q should name the limit", anthropicStyle, rr.Body.String())
		}
		if anthropicStyle && !strings.Contains(rr.Body.String(), "request_too_large") {
			t.Fatalf("anthropic error shape should carry request_too_large, got %q", rr.Body.String())
		}
	}
}

func TestWave2ClientAuthAllowed(t *testing.T) {
	newServer := func() *Server {
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.Keys = []string{"legacy-key"}
		return testGateway(t, cfg)
	}
	call := func(s *Server, key, body string, anthropicStyle bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rr := httptest.NewRecorder()
		if ok := s.clientAuthAllowed(rr, req, anthropicStyle); ok {
			rr.Code = 200
		}
		return rr
	}
	// Missing key, Anthropic error shape.
	if rr := call(newServer(), "", `{"model":"m"}`, true); rr.Code != 401 || !strings.Contains(rr.Body.String(), "authentication_error") {
		t.Fatalf("missing key anthropic: %d %s", rr.Code, rr.Body.String())
	}
	// Legacy key passes and leaves no error body behind.
	if rr := call(newServer(), "legacy-key", `{"model":"m"}`, false); rr.Code != 200 {
		t.Fatalf("legacy key: %d %s", rr.Code, rr.Body.String())
	}
	// Virtual key restricted to another model: Anthropic 403.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "vk1", KeyHash: keyDigest("vk-secret"), AllowedModels: []string{"other-model"},
	}}
	s := testGateway(t, cfg)
	rr := call(s, "vk-secret", `{"model":"model-x"}`, true)
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), "permission_error") {
		t.Fatalf("model denial anthropic: %d %s", rr.Code, rr.Body.String())
	}
	// Per-key RPM overrides the global ceiling: second request in the same
	// minute is rejected with Retry-After 60.
	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.ClientAuth.Enabled = true
	cfg2.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "vk-rpm", KeyHash: keyDigest("vk-rpm-secret"), RPM: 1,
	}}
	s2 := testGateway(t, cfg2)
	if rr := call(s2, "vk-rpm-secret", `{"model":"m"}`, false); rr.Code != 200 {
		t.Fatalf("first RPM request: %d %s", rr.Code, rr.Body.String())
	}
	rr = call(s2, "vk-rpm-secret", `{"model":"m"}`, false)
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "60" {
		t.Fatalf("second RPM request: %d Retry-After=%q body=%s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
}

// ---------- virtual admin handler validation ----------

func wave2AdminServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	return testGateway(t, cfg)
}

func wave2Call(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func wave2CallDirect(handler func(http.ResponseWriter, *http.Request), method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func TestWave2VirtualAdminValidationErrors(t *testing.T) {
	s := wave2AdminServer(t)
	// Virtual endpoints: missing fields, duplicate, bad JSON, unknown id.
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{"id":"ve1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "public_model is required") {
		t.Fatalf("VE missing public_model: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{"id":"ve1","public_model":"m"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "route_profile is required") {
		t.Fatalf("VE missing route_profile: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{"id":"ve1","public_model":"m","route_profile":"profile1"}`); rr.Code != 201 {
		t.Fatalf("VE create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{"id":"ve1","public_model":"m","route_profile":"profile1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("VE duplicate: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{broken`); rr.Code != 400 {
		t.Fatalf("VE bad JSON: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminVirtualEndpointByID, "PUT", "/admin/api/virtual-endpoints/nope", `{"public_model":"m","route_profile":"profile1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("VE PUT unknown: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminVirtualEndpointByID, "GET", "/admin/api/virtual-endpoints/", ""); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id required") {
		t.Fatalf("VE empty id: %d %s", rr.Code, rr.Body.String())
	}
	// Route profiles: missing pool, duplicate, unknown PUT/DELETE.
	if rr := wave2Call(s, "POST", "/admin/api/route-profiles", `{"id":"rp1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "candidate_pool is required") {
		t.Fatalf("RP missing pool: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/admin/api/route-profiles", `{"id":"profile1","candidate_pool":"pool1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("RP duplicate: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminRouteProfileByID, "PUT", "/admin/api/route-profiles/nope", `{"candidate_pool":"pool1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("RP PUT unknown: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminRouteProfileByID, "GET", "/admin/api/route-profiles/", ""); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id required") {
		t.Fatalf("RP empty id: %d %s", rr.Code, rr.Body.String())
	}
	// Candidate pools: missing id, mismatch, unknown PUT, referenced DELETE.
	if rr := wave2Call(s, "POST", "/admin/api/candidate-pools", `{"mode":"all"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id is required") {
		t.Fatalf("pool missing id: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminCandidatePoolByID, "PUT", "/admin/api/candidate-pools/pool1", `{"id":"other","mode":"all"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "must match") {
		t.Fatalf("pool PUT mismatch: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminCandidatePoolByID, "PUT", "/admin/api/candidate-pools/nope", `{"mode":"all"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("pool PUT unknown: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminCandidatePoolByID, "GET", "/admin/api/candidate-pools/", ""); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id required") {
		t.Fatalf("pool empty id: %d %s", rr.Code, rr.Body.String())
	}
	// Fallback chains: missing id, mismatch, unknown PUT.
	if rr := wave2Call(s, "POST", "/admin/api/fallback-chains", `{"pools":["pool1"]}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id is required") {
		t.Fatalf("chain missing id: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminFallbackChainByID, "PUT", "/admin/api/fallback-chains/nope", `{"id":"other","pools":["pool1"]}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "must match") {
		t.Fatalf("chain PUT mismatch: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminFallbackChainByID, "PUT", "/admin/api/fallback-chains/nope", `{"pools":["pool1"]}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("chain PUT unknown: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminFallbackChainByID, "GET", "/admin/api/fallback-chains/", ""); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id required") {
		t.Fatalf("chain empty id: %d %s", rr.Code, rr.Body.String())
	}
	// A pool still referenced by a fallback chain cannot be deleted (409).
	s2 := wave2AdminServer(t)
	if rr := wave2Call(s2, "POST", "/admin/api/fallback-chains", `{"id":"chain1","pools":["pool1"]}`); rr.Code != 201 {
		t.Fatalf("chain create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s2.adminCandidatePoolByID, "DELETE", "/admin/api/candidate-pools/pool1", ""); rr.Code != 409 || !strings.Contains(rr.Body.String(), "still referenced") {
		t.Fatalf("pool DELETE referenced: %d %s", rr.Code, rr.Body.String())
	}
}

func TestWave2RouteProfileMissingID(t *testing.T) {
	s := wave2AdminServer(t)
	if rr := wave2Call(s, "POST", "/admin/api/route-profiles", `{}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "id is required") {
		t.Fatalf("route-profile missing id: %d %s want 400 id is required", rr.Code, rr.Body.String())
	}
}

func TestWave2PoolDeleteFallbackChainOnly(t *testing.T) {
	s := wave2AdminServer(t)
	// pool1 is referenced by profile1, so the route-profile guard would
	// fire first; pool2 is referenced ONLY by a fallback chain, which
	// exercises the fallback-chain guard instead.
	if rr := wave2Call(s, "POST", "/admin/api/candidate-pools", `{"id":"pool2","mode":"all"}`); rr.Code != 201 {
		t.Fatalf("pool2 create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/admin/api/fallback-chains", `{"id":"chain2","pools":["pool2"]}`); rr.Code != 201 {
		t.Fatalf("chain2 create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2CallDirect(s.adminCandidatePoolByID, "DELETE", "/admin/api/candidate-pools/pool2", ""); rr.Code != 409 || !strings.Contains(rr.Body.String(), "still referenced by fallback chain") {
		t.Fatalf("pool2 DELETE: %d %s want 409 fallback-chain reference", rr.Code, rr.Body.String())
	}
}

func TestWave2VirtualAdminResolverBranches(t *testing.T) {
	// Without a resolver, the list falls back to configured endpoints with
	// no pool counts attached.
	cfgNil := config.Default()
	cfgNil.Probe.Enabled = false
	cfgNil.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "plain", PublicModel: "plain-model", RouteProfile: "profile1"}}
	s := testGateway(t, cfgNil)
	s.routeResolver = nil
	rr := wave2CallDirect(s.adminVirtualEndpoints, "GET", "/admin/api/virtual-endpoints", "")
	if rr.Code != 200 {
		t.Fatalf("nil-resolver list: %d %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		VirtualEndpoints []map[string]any `json:"virtual_endpoints"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.VirtualEndpoints) != 1 || listed.VirtualEndpoints[0]["id"] != "plain" {
		t.Fatalf("nil-resolver list: %s", rr.Body.String())
	}
	if _, hasCount := listed.VirtualEndpoints[0]["pool_member_count"]; hasCount {
		t.Fatalf("nil-resolver list must not attach pool counts: %s", rr.Body.String())
	}
	// With the real resolver, a virtual endpoint whose route profile does
	// not exist resolves to zero pool members instead of failing the list.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "dangling", PublicModel: "ghost", RouteProfile: "no-such-profile"}}
	s2 := testGateway(t, cfg)
	rr = wave2CallDirect(s2.adminVirtualEndpoints, "GET", "/admin/api/virtual-endpoints", "")
	if rr.Code != 200 {
		t.Fatalf("dangling list: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ve := range listed.VirtualEndpoints {
		if ve["id"] == "dangling" {
			found = true
			if ve["pool_member_count"] != float64(0) {
				t.Fatalf("dangling VE pool_member_count = %v want 0", ve["pool_member_count"])
			}
		}
	}
	if !found {
		t.Fatalf("dangling VE missing from list: %s", rr.Body.String())
	}
}

func TestWave2VirtualAdminEndpointLegacy(t *testing.T) {
	// GET falls back to the first virtual endpoint's model when no default
	// exists and no routing model is configured.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.PublicModel = ""
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "first", PublicModel: "first-model", Enabled: &trueVal}}
	s := testGateway(t, cfg)
	rr := wave2CallDirect(s.adminEndpoint, "GET", "/admin/api/endpoint", "")
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got["model"] != "first-model" {
		t.Fatalf("legacy GET fallback: %d %s err=%v", rr.Code, rr.Body.String(), err)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("legacy endpoint should be no-store, got %q", rr.Header().Get("Cache-Control"))
	}
	// POST with an empty model and no endpoints or routing model defaults
	// to "nexaroute" instead of rejecting the request.
	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.Routing.PublicModel = ""
	s2 := testGateway(t, cfg2)
	rr = wave2CallDirect(s2.adminEndpoint, "POST", "/admin/api/endpoint", `{}`)
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got["model"] != "nexaroute" {
		t.Fatalf("legacy POST default model: %d %s err=%v", rr.Code, rr.Body.String(), err)
	}
	// Any other method is rejected.
	rr = wave2CallDirect(s2.adminEndpoint, "PATCH", "/admin/api/endpoint", "")
	if rr.Code != 405 {
		t.Fatalf("legacy PATCH: %d want 405", rr.Code)
	}
}

func TestWave2CandidatePoolExpandedCounts(t *testing.T) {
	// With an explicit pool backed by a real deployment, the resolver
	// reports expanded membership on both list and by-id reads.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "coding", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	rr := wave2CallDirect(s.adminCandidatePools, "GET", "/admin/api/candidate-pools", "")
	var listed struct {
		CandidatePools []map[string]any `json:"candidate_pools"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cp := range listed.CandidatePools {
		if cp["id"] == "coding" {
			found = true
			if cp["expanded_count"] != float64(1) {
				t.Fatalf("list expanded_count = %v want 1 (body %s)", cp["expanded_count"], rr.Body.String())
			}
		}
	}
	if !found {
		t.Fatalf("coding pool missing from list: %s", rr.Body.String())
	}
	rr = wave2CallDirect(s.adminCandidatePoolByID, "GET", "/admin/api/candidate-pools/coding", "")
	var one map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one["expanded_count"] != float64(1) {
		t.Fatalf("by-id expanded_count = %v want 1 (body %s)", one["expanded_count"], rr.Body.String())
	}
}
