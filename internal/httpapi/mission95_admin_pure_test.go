package httpapi

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

func intPtrMission95(v int) *int { return &v }

func TestMission95AdminPureHelpers(t *testing.T) {
	// clientBaseURLSource classifies the configured base URL.
	if got := clientBaseURLSource(""); got != "browser-origin-fallback" {
		t.Errorf("empty base source = %q", got)
	}
	if got := clientBaseURLSource("  "); got != "browser-origin-fallback" {
		t.Errorf("blank base source = %q", got)
	}
	if got := clientBaseURLSource("https://api.example.com"); got != "configured" {
		t.Errorf("custom base source = %q", got)
	}
	// secretSource precedence: pool > env > literal > none.
	if got := secretSource(config.ProviderConfig{Credentials: []config.CredentialConfig{{APIKey: "x"}}, APIKeyEnv: "K", APIKey: "y"}); got != "pool" {
		t.Errorf("pool source = %q", got)
	}
	if got := secretSource(config.ProviderConfig{APIKeyEnv: "K"}); got != "env" {
		t.Errorf("env source = %q", got)
	}
	if got := secretSource(config.ProviderConfig{APIKey: "y"}); got != "literal" {
		t.Errorf("literal source = %q", got)
	}
	if got := secretSource(config.ProviderConfig{}); got != "none" {
		t.Errorf("none source = %q", got)
	}
	// providerHeaderNames exposes sorted names only, never values.
	names := providerHeaderNames(config.ProviderConfig{Headers: map[string]string{"X-B": "secret", "X-A": "secret"}})
	if len(names) != 2 || names[0] != "X-A" || names[1] != "X-B" {
		t.Errorf("header names = %v", names)
	}
	// proxyStructure strips credentials, path, query and fragment.
	scheme, host, display := proxyStructure("http://user:pass@proxy.example.com:8080/path?q=1#f")
	if scheme != "http" || host != "proxy.example.com:8080" || display != "http://proxy.example.com:8080" {
		t.Errorf("proxy structure = %q %q %q", scheme, host, display)
	}
	if s, h, d := proxyStructure(""); s != "" || h != "" || d != "" {
		t.Errorf("empty proxy = %q %q %q", s, h, d)
	}
	if s, h, d := proxyStructure("://bad"); s != "" || h != "" || d != "" {
		t.Errorf("bad proxy = %q %q %q", s, h, d)
	}
	if s, h, d := proxyStructure("HTTPS://Proxy.Example.COM"); s != "https" || h != "Proxy.Example.COM" || d != "https://Proxy.Example.COM" {
		t.Errorf("proxy case = %q %q %q", s, h, d)
	}
	// slug normalizes ids and falls back to "model".
	if got := slug("GPT 4/Turbo:XL"); got != "gpt-4-turbo-xl" {
		t.Errorf("slug = %q", got)
	}
	if got := slug("!!!"); got != "model" {
		t.Errorf("empty slug = %q", got)
	}
	if got := slug("  spaced_name.v2  "); got != "spaced_name.v2" {
		t.Errorf("slug trim = %q", got)
	}
	// uniqueStrings trims, dedupes and sorts.
	if got := uniqueStrings([]string{"b", " a ", "", "b", "a"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("uniqueStrings = %v", got)
	}
	// parseModelList delegates to the providers parser.
	models := parseModelList([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	if len(models) != 2 || models[0] != "m1" {
		t.Errorf("model list = %v", models)
	}
	// providerSummary counts model states and never leaks secrets.
	summary := providerSummary(config.ProviderConfig{
		ID: "p1", Name: "P", Type: "openai", APIKey: "secret-value", APIKeyEnv: "",
		Models: []config.ModelConfig{
			{Model: "m1", Source: config.ModelSourceDiscovered, TestState: config.ModelTestPassed},
			{Model: "m2", TestState: config.ModelTestFailed},
			{Model: "m3"},
		},
	})
	states, _ := summary["model_states"].(map[string]int)
	if summary["model_count"] != 3 || states["discovered"] != 1 || states["manual"] != 2 ||
		states["passed"] != 1 || states["failed"] != 1 || states["untested"] != 1 || states["tested"] != 2 {
		t.Fatalf("summary states = %v (summary=%v)", states, summary)
	}
	if summary["has_secret"] != true {
		t.Error("has_secret false for keyed provider")
	}
	// snapshotConfigProviders mirrors ids and per-model costs.
	snap := snapshotConfigProviders([]config.ProviderConfig{{ID: "p1", DataHandling: config.DataHandlingConfig{TrainsOnData: "no"}, Models: []config.ModelConfig{{Model: "m1", InputCostPerMTok: 2, OutputCostPerMTok: 3}}}})
	if len(snap) != 1 || snap[0]["id"] != "p1" {
		t.Fatalf("snapshot providers = %v", snap)
	}
	// providerModelsMeta defaults nil aliases to an empty list.
	meta := providerModelsMeta(config.ProviderConfig{Models: []config.ModelConfig{{Model: "m1", ID: "", Aliases: nil, Enabled: true, Weight: 2}}})
	if len(meta) != 1 {
		t.Fatalf("models meta = %v", meta)
	}
	if aliases, _ := meta[0]["aliases"].([]string); len(aliases) != 0 {
		t.Errorf("nil aliases not defaulted: %v", meta[0]["aliases"])
	}
	// providerPressure mirrors stats fields.
	pressure := providerPressure([]providers.ProviderStats{{ID: "p1", ActiveRequests: 3, MaxConcurrency: 10, RequestLimit: 100, RemainingRequests: 40}})
	if len(pressure) != 1 || pressure[0]["active"] != int64(3) || pressure[0]["capacity"] != 10 {
		t.Fatalf("pressure = %v", pressure)
	}
	// scopeHealthRows skips all-healthy and scopeless states, reports worst.
	rows := scopeHealthRows([]health.State{
		{Deployment: "ok", Scopes: map[string]health.ScopeState{"a": {Status: health.Healthy}}},
		{Deployment: "bare"},
		{Deployment: "bad", Scopes: map[string]health.ScopeState{
			"a": {Status: health.Degraded, ConsecutiveFailures: 2},
			"b": {Status: health.Cooldown, ConsecutiveFailures: 5},
		}},
	})
	if len(rows) != 1 || rows[0]["deployment"] != "bad" || rows[0]["status"] != health.Cooldown || rows[0]["consecutive_failures"] != 5 {
		t.Fatalf("scope rows = %v", rows)
	}
	if scopes, _ := rows[0]["scopes"].([]string); len(scopes) != 2 || scopes[0] != "a" || scopes[1] != "b" {
		t.Errorf("scope names = %v", rows[0]["scopes"])
	}
}

func TestMission95MergeAndNormalizeProvider(t *testing.T) {
	cfg := config.Config{Providers: []config.ProviderConfig{{
		ID: "p1", APIKey: "old-key", APIKeyEnv: "OLD_ENV", ProxyURL: "http://proxy:8080",
		Headers:     map[string]string{"X-H": "v"},
		Credentials: []config.CredentialConfig{{APIKey: "pool-key"}},
	}}}
	// mergeExistingSecret copies stored secrets for a known id, no-op unknown.
	p := config.ProviderConfig{ID: "p1"}
	mergeExistingSecret(cfg, &p)
	if p.APIKey != "old-key" || p.APIKeyEnv != "OLD_ENV" || len(p.Credentials) != 1 {
		t.Fatalf("merged secret = %+v", p)
	}
	unknown := config.ProviderConfig{ID: "nope", APIKey: "mine"}
	mergeExistingSecret(cfg, &unknown)
	if unknown.APIKey != "mine" {
		t.Error("merge touched an unknown provider")
	}
	// mergePreservedFields honors each preserve flag independently.
	form := providerForm{PreserveSecret: true, PreserveHeaders: true, PreserveProxy: false, Provider: config.ProviderConfig{ID: "p1"}}
	mergePreservedFields(cfg, &form)
	if form.Provider.APIKey != "old-key" || form.Provider.Headers["X-H"] != "v" || form.Provider.ProxyURL != "" {
		t.Fatalf("preserved fields = %+v", form.Provider)
	}
	form2 := providerForm{PreserveProxy: true, Provider: config.ProviderConfig{ID: "p1"}}
	mergePreservedFields(cfg, &form2)
	if form2.Provider.ProxyURL != "http://proxy:8080" || form2.Provider.APIKey != "" {
		t.Fatalf("proxy-only preserve = %+v", form2.Provider)
	}
	// dropEnvResolvedLiteral drops an env-sourced literal, keeps a fallback.
	t.Setenv("MISSION95_TEST_KEY", "env-secret")
	in := config.ProviderConfig{APIKeyEnv: "MISSION95_TEST_KEY", APIKey: "env-secret"}
	dropEnvResolvedLiteral(&in, config.ProviderConfig{})
	if in.APIKey != "" {
		t.Error("env-resolved literal not dropped")
	}
	in2 := config.ProviderConfig{APIKeyEnv: "MISSION95_TEST_KEY", APIKey: "different"}
	dropEnvResolvedLiteral(&in2, config.ProviderConfig{})
	if in2.APIKey != "different" {
		t.Error("non-env literal wrongly dropped")
	}
	in3 := config.ProviderConfig{APIKeyEnv: "MISSION95_MISSING_ENV_XYZ", APIKey: "fallback"}
	dropEnvResolvedLiteral(&in3, config.ProviderConfig{})
	if in3.APIKey != "fallback" {
		t.Error("unset-env fallback literal dropped")
	}
	in4 := config.ProviderConfig{APIKey: "only-literal"}
	dropEnvResolvedLiteral(&in4, config.ProviderConfig{})
	if in4.APIKey != "only-literal" {
		t.Error("literal without env ref dropped")
	}
	// normalizeProvider trims models, slugs ids, defaults weight to 1.
	np := config.ProviderConfig{Models: []config.ModelConfig{{Model: "  m1 ", Weight: 0}, {Model: "m2", ID: "custom", Weight: 3}}}
	normalizeProvider(&np)
	if np.Models[0].Model != "m1" || np.Models[0].ID != "m1" || np.Models[0].Weight != 1 {
		t.Fatalf("normalized model 0 = %+v", np.Models[0])
	}
	if np.Models[1].ID != "custom" || np.Models[1].Weight != 3 {
		t.Fatalf("normalized model 1 = %+v", np.Models[1])
	}
}

func TestMission95CredentialPoolUpdate(t *testing.T) {
	old := []config.CredentialConfig{{Name: "a", APIKey: "k1", Enabled: true}, {Name: "b", APIKeyEnv: "ENV_B", Enabled: false}}
	boolPtr := func(v bool) *bool { return &v }
	// Replace mode: index validation, dup detection, both-fields rejection.
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Replace: true, Entries: []credentialPoolEntry{{Index: intPtrMission95(5), APIKey: "x"}}}); err == nil {
		t.Error("replace with out-of-range index accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Replace: true, Entries: []credentialPoolEntry{{Index: intPtrMission95(0), APIKey: "x"}, {Index: intPtrMission95(0), APIKey: "y"}}}); err == nil {
		t.Error("replace with duplicate index accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Replace: true, Entries: []credentialPoolEntry{{APIKey: "x", APIKeyEnv: "E"}}}); err == nil {
		t.Error("entry with both key and env accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Replace: true, Entries: []credentialPoolEntry{{Name: "c"}}}); err == nil {
		t.Error("new entry without key or env accepted")
	}
	out, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Replace: true, Entries: []credentialPoolEntry{
		{Index: intPtrMission95(0), Name: " a-renamed ", Enabled: boolPtr(false)},
		{Index: intPtrMission95(1), APIKeyEnv: "ENV_B2"},
	}})
	if err != nil || len(out) != 2 || out[0].Name != "a-renamed" || out[0].Enabled != false || out[0].APIKey != "k1" || out[1].APIKeyEnv != "ENV_B2" || out[1].APIKey != "" {
		t.Fatalf("replace result = %+v, %v", out, err)
	}
	// Merge mode: removals, updates, appends, and all conflict errors.
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Remove: []int{0, 0}}); err == nil {
		t.Error("duplicate removal accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Remove: []int{9}}); err == nil {
		t.Error("out-of-range removal accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Entries: []credentialPoolEntry{{Index: intPtrMission95(0), APIKey: "x"}, {Index: intPtrMission95(0), APIKey: "y"}}}); err == nil {
		t.Error("duplicate update accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Remove: []int{1}, Entries: []credentialPoolEntry{{Index: intPtrMission95(1), APIKey: "x"}}}); err == nil {
		t.Error("update+remove conflict accepted")
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Entries: []credentialPoolEntry{{Index: intPtrMission95(7), APIKey: "x"}}}); err == nil {
		t.Error("out-of-range update accepted")
	}
	out, err = applyCredentialPoolUpdate(old, credentialPoolUpdate{
		Remove:  []int{1},
		Entries: []credentialPoolEntry{{Index: intPtrMission95(0), APIKey: "k1-new"}, {Name: "c", APIKeyEnv: "ENV_C"}},
	})
	if err != nil || len(out) != 2 || out[0].APIKey != "k1-new" || out[1].Name != "c" {
		t.Fatalf("merge result = %+v, %v", out, err)
	}
	if _, err := applyCredentialPoolUpdate(old, credentialPoolUpdate{Entries: []credentialPoolEntry{{Name: "d"}}}); err == nil {
		t.Error("append without key or env accepted")
	}
}

func TestMission95ServerAdmissionAndLogging(t *testing.T) {
	// admissionLimit defaults to 128, honors config, clamps below 1.
	s := &Server{}
	if got := s.admissionLimit(); got != 128 {
		t.Errorf("default limit = %d", got)
	}
	s.cfg.Routing.MaxInflightRequests = 3
	if got := s.admissionLimit(); got != 3 {
		t.Errorf("configured limit = %d", got)
	}
	s.cfg.Routing.MaxInflightRequests = 0
	if got := s.admissionLimit(); got != 128 {
		t.Errorf("zero limit = %d", got)
	}
	// tryAcquire/release: admission stops at the limit and never goes negative.
	s.cfg.Routing.MaxInflightRequests = 2
	if !s.tryAcquireDataPlane() || !s.tryAcquireDataPlane() || s.tryAcquireDataPlane() {
		t.Error("admission limit not enforced")
	}
	s.releaseDataPlane()
	if !s.tryAcquireDataPlane() {
		t.Error("release did not free capacity")
	}
	s.releaseDataPlane()
	s.releaseDataPlane()
	s.releaseDataPlane()
	if s.inflight.Load() != 0 {
		t.Errorf("inflight went negative: %d", s.inflight.Load())
	}
	// shouldLogRequest: off disables; errors always log; slow + modes.
	s.cfg.Logging.AccessMode = "off"
	if s.shouldLogRequest(500, 0, false, 1) {
		t.Error("off mode logged an error")
	}
	s.cfg.Logging.AccessMode = ""
	if !s.shouldLogRequest(500, 0, false, 1) {
		t.Error("error not logged")
	}
	s.cfg.Logging.SlowRequestMS = 100
	if !s.shouldLogRequest(200, 150*time.Millisecond, false, 1) {
		t.Error("slow request not logged")
	}
	if s.shouldLogRequest(200, 150*time.Millisecond, true, 1) {
		t.Error("slow streaming request wrongly logged by slow rule")
	}
	s.cfg.Logging.AccessMode = "all"
	if !s.shouldLogRequest(200, 0, false, 7) {
		t.Error("all mode did not log")
	}
	s.cfg.Logging.AccessMode = "sampled"
	s.cfg.Logging.SuccessSampleEvery = 10
	if !s.shouldLogRequest(200, 0, false, 20) || s.shouldLogRequest(200, 0, false, 21) {
		t.Error("sampled mode sampling wrong")
	}
	s.cfg.Logging.SuccessSampleEvery = 0
	if !s.shouldLogRequest(200, 0, false, 1000) {
		t.Error("sampled mode default divisor wrong")
	}
	s.cfg.Logging.AccessMode = "weird"
	if s.shouldLogRequest(200, 0, false, 1) {
		t.Error("unknown mode logged")
	}
	// rejectOverloaded: counter increments, Retry-After set, per-path envelope.
	busServer := &Server{bus: events.New(10)}
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		busServer.rejectOverloaded(rec, req, "req-1")
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("path %s: code=%d retry-after=%q", path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if busServer.overloadRejects.Load() != 3 {
		t.Errorf("overload counter = %d", busServer.overloadRejects.Load())
	}
	// isDataPlaneRequest classification.
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses", "/v1/video/jobs", "/v1/embeddings", "/v1/images/generations", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/rerank"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if !isDataPlaneRequest(req) {
			t.Errorf("data-plane path %s not classified", path)
		}
	}
	if isDataPlaneRequest(httptest.NewRequest(http.MethodGet, "/v1/messages", nil)) {
		t.Error("GET classified as data-plane")
	}
	if isDataPlaneRequest(httptest.NewRequest(http.MethodPost, "/admin/api/x", nil)) {
		t.Error("admin path classified as data-plane")
	}
	// adminAllow token bucket: burst capacity then refill-limited denial.
	allowServer := &Server{}
	allowed := 0
	for i := 0; i < 200; i++ {
		if allowServer.adminAllow("10.0.0.1", 1) {
			allowed++
		}
	}
	if allowed != 90 {
		t.Errorf("bucket burst = %d, want 90", allowed)
	}
	// A fresh IP gets its own full bucket, independent of the drained one.
	if !allowServer.adminAllow("10.0.0.2", 1) || !allowServer.adminAllow("10.0.0.2", 1) {
		t.Error("fresh IP bucket denied immediately")
	}
	if !allowServer.adminAllow("10.0.0.1", 0) {
		t.Error("zero-cost request denied at empty bucket")
	}
}

func TestMission95StatusWriterAndSecurityHelpers(t *testing.T) {
	// statusWriter: implicit 200 on Write, sticky WriteHeader, Flush, ReadFrom.
	rec := httptest.NewRecorder()
	w := &statusWriter{ResponseWriter: rec}
	if responseCommitted(w) || w.Committed() {
		t.Error("fresh writer reports committed")
	}
	if w.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap did not return the inner writer")
	}
	n, err := w.Write([]byte("hi"))
	if err != nil || n != 2 || rec.Code != http.StatusOK || !w.Committed() || !responseCommitted(w) {
		t.Fatalf("implicit write: n=%d err=%v code=%d committed=%v", n, err, rec.Code, w.Committed())
	}
	w.WriteHeader(http.StatusTeapot)
	if rec.Code != http.StatusOK {
		t.Error("second WriteHeader overwrote the first")
	}
	rec2 := httptest.NewRecorder()
	w2 := &statusWriter{ResponseWriter: rec2}
	w2.Flush()
	if rec2.Code != http.StatusOK {
		t.Errorf("flush code = %d", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	w3 := &statusWriter{ResponseWriter: rec3}
	m, err := w3.ReadFrom(strings.NewReader("abc"))
	if err != nil || m != 3 || rec3.Body.String() != "abc" || rec3.Code != http.StatusOK {
		t.Fatalf("ReadFrom = %d, %v, body=%q code=%d", m, err, rec3.Body.String(), rec3.Code)
	}
	if responseCommitted(httptest.NewRecorder()) {
		t.Error("plain recorder reports committed")
	}
	// isStateChanging.
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if isStateChanging(m) {
			t.Errorf("%s classified as state-changing", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if !isStateChanging(m) {
			t.Errorf("%s not classified as state-changing", m)
		}
	}
	// forwardedProtoHTTPS: exactly one unambiguous https token.
	if !forwardedProtoHTTPS([]string{"https"}) || !forwardedProtoHTTPS([]string{" HTTPS "}) {
		t.Error("single https token rejected")
	}
	for _, v := range [][]string{{"http"}, {"https", "https"}, {}, {"https, http"}, {""}} {
		if forwardedProtoHTTPS(v) {
			t.Errorf("values %v accepted as https", v)
		}
	}
	// requestIsSecure: TLS wins; untrusted proxies never count.
	tlsReq := httptest.NewRequest(http.MethodGet, "https://gw/", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if !requestIsSecure(tlsReq) {
		t.Error("TLS request not secure")
	}
	plain := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	plain.Header.Set("X-Forwarded-Proto", "https")
	if requestIsSecure(plain) {
		t.Error("untrusted forwarded proto counted as secure")
	}
	// sameOrigin: empty origin/referer passes; mismatch/scheme/userinfo fail.
	same := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	same.Host = "gw"
	same.Header.Set("Origin", "http://gw")
	if !sameOrigin(same) {
		t.Error("matching origin rejected")
	}
	same.Header.Set("Origin", "https://gw")
	if sameOrigin(same) {
		t.Error("https origin on plain request accepted")
	}
	same.Header.Set("Origin", "http://evil")
	if sameOrigin(same) {
		t.Error("foreign origin accepted")
	}
	same.Header.Del("Origin")
	same.Header.Set("Referer", "http://gw/page")
	if !sameOrigin(same) {
		t.Error("matching referer rejected")
	}
	noOrigin := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	if !sameOrigin(noOrigin) {
		t.Error("origin-less request rejected")
	}
	// adminCSRFAllowed: no cookie + no origin = allowed (API clients);
	// cookie present requires a matching token header.
	if !adminCSRFAllowed(noOrigin) {
		t.Error("origin-less CSRF check failed")
	}
	cookieReq := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	cookieReq.Host = "gw"
	cookieReq.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: "tok123"})
	cookieReq.Header.Set("Origin", "http://gw")
	if adminCSRFAllowed(cookieReq) {
		t.Error("cookie without token header allowed")
	}
	cookieReq.Header.Set("X-NexaRoute-CSRF", "tok123")
	if !adminCSRFAllowed(cookieReq) {
		t.Error("matching CSRF token rejected")
	}
	cookieReq.Header.Set("X-NexaRoute-CSRF", "wrong!")
	if adminCSRFAllowed(cookieReq) {
		t.Error("wrong CSRF token allowed")
	}
	// adminRemoteIP / adminHostAllowed basics via Server helpers.
	remoteReq := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	remoteReq.RemoteAddr = "203.0.113.9:1234"
	if got := adminRemoteIP(remoteReq); got != "203.0.113.9" {
		t.Errorf("remote IP = %q", got)
	}
	bareReq := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	bareReq.RemoteAddr = "203.0.113.9"
	if got := adminRemoteIP(bareReq); got != "203.0.113.9" {
		t.Errorf("bare remote IP = %q", got)
	}
	for _, host := range []string{"localhost", "localhost:8080", "127.0.0.1", "127.0.0.1:8080", "::1", "[::1]:8080"} {
		hr := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
		hr.Host = host
		if !adminHostAllowed(hr) {
			t.Errorf("loopback host %q rejected", host)
		}
	}
	for _, host := range []string{"", "example.com", "192.0.2.1", "evil.localhost.com"} {
		hr := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
		hr.Host = host
		if adminHostAllowed(hr) {
			t.Errorf("host %q accepted as loopback", host)
		}
	}
	// withTrustedProxyContext: only a peer inside a configured proxy CIDR
	// earns trust, and only then does X-Forwarded-Proto count.
	proxyServer := &Server{}
	proxyServer.cfg.TLS.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	trustedReq := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	trustedReq.RemoteAddr = "10.1.2.3:9999"
	trustedReq.Header.Set("X-Forwarded-Proto", "https")
	if requestIsSecure(proxyServer.withTrustedProxyContext(trustedReq)) != true {
		t.Error("trusted-proxy forwarded proto not honored")
	}
	directReq := httptest.NewRequest(http.MethodGet, "http://gw/", nil)
	directReq.RemoteAddr = "203.0.113.9:9999"
	directReq.Header.Set("X-Forwarded-Proto", "https")
	if requestIsSecure(proxyServer.withTrustedProxyContext(directReq)) {
		t.Error("untrusted peer earned proxy trust")
	}
	untrustedServer := &Server{}
	if requestIsSecure(untrustedServer.withTrustedProxyContext(trustedReq)) {
		t.Error("no-proxy server trusted a forwarded proto")
	}
	// Client-certificate gates: not required by default, required blocks
	// cert-less requests and passes verified chains.
	certServer := &Server{}
	if !certServer.adminClientCertificateAuthorized(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("cert gate blocked when not required")
	}
	certServer.cfg.TLS.RequireClientCertAdmin = true
	if certServer.adminClientCertificateAuthorized(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("cert gate passed without TLS")
	}
	verified := httptest.NewRequest(http.MethodGet, "/", nil)
	verified.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	if !certServer.adminClientCertificateAuthorized(verified) {
		t.Error("cert gate blocked a verified chain")
	}
	certServer.cfg.TLS.RequireClientCertDataPlane = true
	if certServer.dataPlaneClientCertificateAuthorized(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("data-plane cert gate passed without TLS")
	}
	if !certServer.dataPlaneClientCertificateAuthorized(verified) {
		t.Error("data-plane cert gate blocked a verified chain")
	}
	// adminCSRFToken: wrong method rejected; GET issues a cookie + token.
	csrfServer := &Server{}
	rec4 := httptest.NewRecorder()
	csrfServer.adminCSRFToken(rec4, httptest.NewRequest(http.MethodPost, "/admin/api/csrf", nil))
	if rec4.Code != http.StatusMethodNotAllowed {
		t.Errorf("CSRF POST code = %d", rec4.Code)
	}
	rec5 := httptest.NewRecorder()
	csrfServer.adminCSRFToken(rec5, httptest.NewRequest(http.MethodGet, "/admin/api/csrf", nil))
	if rec5.Code != http.StatusOK || rec5.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("CSRF GET code=%d cache=%q", rec5.Code, rec5.Header().Get("Cache-Control"))
	}
	cookies := rec5.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminCSRFCookie || cookies[0].Value == "" || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("CSRF cookie = %v", cookies)
	}
}
