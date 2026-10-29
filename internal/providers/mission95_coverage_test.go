package providers

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// --- catalog / conformance ---------------------------------------------------

func TestMission95FinalizeUnknownPresetIsPartial(t *testing.T) {
	p := finalizePreset(Preset{Type: "not_a_real_dialect"})
	if len(p.Capabilities) != 0 {
		t.Fatalf("unknown dialect capabilities=%v want none", p.Capabilities)
	}
	if p.Status != PresetStatusPartial {
		t.Fatalf("unknown dialect status=%q want partial", p.Status)
	}
	if caps := capabilitiesForDialect("not_a_real_dialect"); caps != nil {
		t.Fatalf("unknown dialect caps=%v want nil", caps)
	}
}

func TestMission95ConformanceEdges(t *testing.T) {
	if _, ok := ManifestEntry("no-such-dialect"); ok {
		t.Fatal("unknown dialect must have no manifest entry")
	}
	// Unparseable evidence dates go stale instead of crashing the policy.
	stale := EffectiveState(ConformanceEntry{Status: StateFixtureVerified, EvidenceDate: "not-a-date"}, time.Now(), 180)
	if stale != StateStale {
		t.Fatalf("bad evidence date state=%q want STALE", stale)
	}

	fullDims := func(state ConformanceState) map[ConformanceDimension]DimensionClaim {
		m := map[ConformanceDimension]DimensionClaim{}
		for _, d := range ConformanceDimensions() {
			m[d] = DimensionClaim{State: state}
		}
		return m
	}
	entries := []ConformanceEntry{
		{Dialect: ""},
		{Dialect: "dup", ContractVersion: ManifestVersion, Status: StateUntested, Dimensions: fullDims(StateUntested), EvidenceDate: "2026-10-01"},
		{Dialect: "dup", ContractVersion: ManifestVersion, Status: StateUntested, Dimensions: fullDims(StateUntested), EvidenceDate: "2026-10-01"},
		{Dialect: "badversion", ContractVersion: "conformance.v999", Status: StateUntested, Dimensions: fullDims(StateUntested), EvidenceDate: "2026-10-01"},
		{Dialect: "baddate", ContractVersion: ManifestVersion, Status: StateFixtureVerified, EvidenceSource: "test", Dimensions: fullDims(StateFixtureVerified), EvidenceDate: "not-a-date"},
		{Dialect: "badstate", ContractVersion: ManifestVersion, Status: StateUntested, Dimensions: map[ConformanceDimension]DimensionClaim{DimAddressing: {State: "BOGUS"}}, EvidenceDate: "2026-10-01"},
		{Dialect: "liveinside", ContractVersion: ManifestVersion, Status: StateFixtureVerified, EvidenceSource: "test", Dimensions: map[ConformanceDimension]DimensionClaim{DimAddressing: {State: StateLiveVerified}}, EvidenceDate: "2026-10-01"},
	}
	known := map[string]bool{"dup": true, "badversion": true, "baddate": true, "badstate": true, "liveinside": true}
	problems := ValidateManifest(entries, known)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		"empty dialect",
		"duplicate manifest entry for dialect \"dup\"",
		"contract version",
		"unparseable evidence date",
		"invalid stored state",
		"claims LIVE-VERIFIED inside a FIXTURE-VERIFIED entry",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("problems=%v missing %q", problems, want)
		}
	}
}

// --- discovery -----------------------------------------------------------------

func mission95DiscoveryProvider(t *testing.T, srv *httptest.Server) config.ProviderConfig {
	t.Helper()
	return config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "fake-discovery-key", ModelsPath: "/v1/models", Enabled: true}
}

func TestMission95DiscoveryEdges(t *testing.T) {
	// Zero timeout falls back to the default and still discovers.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer good.Close()
	models, status, err := DiscoverModelsWithTimeout(context.Background(), mission95DiscoveryProvider(t, good), 0)
	if err != nil || status != 200 || len(models) != 1 || models[0] != "m1" {
		t.Fatalf("zero-timeout discovery models=%v status=%d err=%v", models, status, err)
	}

	// An unbuildable adapter fails before any network call.
	bad := config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: "://bad-url", ModelsPath: "/v1/models", Enabled: true}
	if _, _, err := DiscoverModelsWithTimeout(context.Background(), bad, time.Second); err == nil {
		t.Fatal("unbuildable adapter must fail discovery")
	}

	// A truncated body (declared length never fulfilled) is a read error.
	trunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer trunc.Close()
	if _, _, err := DiscoverModelsWithTimeout(context.Background(), mission95DiscoveryProvider(t, trunc), time.Second); err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("truncated body err=%v want read error", err)
	}

	// An over-limit body is rejected without being parsed.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxDiscoveryBodyBytes+1))
	}))
	defer huge.Close()
	if _, _, err := DiscoverModelsWithTimeout(context.Background(), mission95DiscoveryProvider(t, huge), time.Second); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body err=%v want exceeds error", err)
	}

	// A long error body is truncated with an ellipsis, never echoed whole.
	longErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("e", 3000)))
	}))
	defer longErr.Close()
	_, status, err = DiscoverModelsWithTimeout(context.Background(), mission95DiscoveryProvider(t, longErr), time.Second)
	if err == nil || status != 500 || !strings.Contains(err.Error(), "…") {
		t.Fatalf("long error body status=%d err=%v want truncated 500 error", status, err)
	}
	if len(err.Error()) >= 3000 {
		t.Fatalf("error body was not truncated: len=%d", len(err.Error()))
	}

	// A 200 with no model ids is an explicit failure, not an empty success.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer empty.Close()
	if _, _, err := DiscoverModelsWithTimeout(context.Background(), mission95DiscoveryProvider(t, empty), time.Second); err == nil || !strings.Contains(err.Error(), "no model ids") {
		t.Fatalf("empty inventory err=%v want no-model-ids error", err)
	}
}

// --- registry / provider surface ------------------------------------------------

func TestMission95QuotaEstimateEdges(t *testing.T) {
	ctx := WithQuotaEstimate(context.Background(), 2_000_000_000_000, 0)
	if got, ok := quotaEstimateFromContext(ctx); !ok || got != maxQuotaEstimateTokens {
		t.Fatalf("overflow estimate=%d ok=%v want clamped %d", got, ok, maxQuotaEstimateTokens)
	}
	if got, ok := quotaEstimateFromContext(nil); ok || got != 0 {
		t.Fatalf("nil ctx estimate=%d ok=%v want 0/false", got, ok)
	}
	neg := context.WithValue(context.Background(), quotaEstimateContextKey{}, int64(-5))
	if got, ok := quotaEstimateFromContext(neg); !ok || got != 0 {
		t.Fatalf("negative estimate=%d ok=%v want 0/true", got, ok)
	}
	big := context.WithValue(context.Background(), quotaEstimateContextKey{}, int64(2_000_000_000_000))
	if got, ok := quotaEstimateFromContext(big); !ok || got != maxQuotaEstimateTokens {
		t.Fatalf("oversized estimate=%d ok=%v want clamped", got, ok)
	}
}

func TestMission95RegistryEdges(t *testing.T) {
	badCfg := config.Default()
	badCfg.Providers = []config.ProviderConfig{{ID: "bad", Type: "openai_compatible", BaseURL: "://bad-url", Enabled: true}}
	if _, err := NewRegistry(badCfg); err == nil {
		t.Fatal("registry with an unbuildable provider must fail")
	}
	if _, err := NewAdapterWithRetryAfterCap(config.ProviderConfig{ID: "bad", Type: "openai_compatible", BaseURL: "://bad-url"}, time.Second, time.Minute); err == nil {
		t.Fatal("adapter with an unbuildable provider must fail")
	}

	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.invalid", Enabled: true}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Stat("missing"); ok {
		t.Fatal("Stat for an unknown provider must miss")
	}
	st, ok := reg.Stat("p")
	if !ok || st.ID != "p" {
		t.Fatalf("Stat hit=%v ok=%v want p", st, ok)
	}
	if reg.CredentialsMatchProvider(config.ProviderConfig{ID: "missing"}) {
		t.Fatal("credentials match for an unknown provider must be false")
	}
	stats := reg.Stats()
	if len(stats) != 1 || stats[0].ID != "p" {
		t.Fatalf("registry stats=%v want one p entry", stats)
	}
	// Replace(nil) is a no-op, never a wipe.
	reg.Replace(nil)
	if _, ok := reg.Get("p"); !ok {
		t.Fatal("Replace(nil) must not drop adapters")
	}
	// A disabled provider is skipped by Prepare; a bad provider fails it.
	disabledCfg := config.Default()
	disabledCfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "https://example.invalid", Enabled: false}}
	next, err := reg.Prepare(disabledCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.Get("p"); ok {
		t.Fatal("disabled provider must not appear in the prepared registry")
	}
	if _, err := reg.Prepare(badCfg, nil); err == nil {
		t.Fatal("Prepare with an unbuildable provider must fail")
	}
	if err := reg.Reload(badCfg); err == nil {
		t.Fatal("Reload with an unbuildable provider must fail")
	}
	// The pre-existing adapter survives a failed reload.
	if _, ok := reg.Get("p"); !ok {
		t.Fatal("failed reload must not drop the live adapter")
	}
}

// --- evaluation -------------------------------------------------------------------

// mission95FakeAdapter implements Adapter without being an *httpAdapter.
type mission95FakeAdapter struct{}

func (mission95FakeAdapter) ID() string   { return "fake" }
func (mission95FakeAdapter) Kind() string { return "fake" }
func (mission95FakeAdapter) Stats() ProviderStats {
	return ProviderStats{ID: "fake"}
}
func (mission95FakeAdapter) CredentialsMatch([]string) bool { return false }
func (mission95FakeAdapter) RedactBody(b []byte) []byte     { return b }
func (mission95FakeAdapter) Do(context.Context, []byte, bool, http.Header) (*http.Response, error) {
	return nil, fmt.Errorf("fake do")
}
func (mission95FakeAdapter) DoPath(context.Context, string, string, []byte, bool, http.Header) (*http.Response, error) {
	return nil, fmt.Errorf("fake do path")
}
func (mission95FakeAdapter) CountTokens(context.Context, []byte, http.Header) (*http.Response, error) {
	return nil, fmt.Errorf("fake count")
}
func (mission95FakeAdapter) Probe(context.Context, string, int) (time.Duration, int, error) {
	return 0, 0, fmt.Errorf("fake probe")
}

func TestMission95EvaluationTwinEdges(t *testing.T) {
	if _, ok := EvaluationTwin(mission95FakeAdapter{}); ok {
		t.Fatal("a non-HTTP adapter must not yield an evaluation twin")
	}
	if got := LiveCapable(nil); got != nil {
		t.Fatal("LiveCapable(nil) must be nil")
	}
	if _, err := LiveComplete(context.Background(), mission95FakeAdapter{}, "m", "hi", 10); err == nil {
		t.Fatal("live completion on a non-HTTP adapter must fail")
	}
	// A hand-built adapter without redirect policies gets the fail-closed
	// fallback in the twin instead of a nil policy.
	orig := &httpAdapter{
		p:       config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: "https://example.invalid"},
		c:       &http.Client{Transport: http.DefaultTransport},
		streamC: &http.Client{Transport: http.DefaultTransport},
		sem:     make(chan struct{}, 4),
		creds:   []credentialState{{Key: "fake-key-1"}},
	}
	twin, ok := EvaluationTwin(orig)
	if !ok {
		t.Fatal("hand-built HTTP adapter must yield a twin")
	}
	ha := twin.(*httpAdapter)
	if !ha.evaluationOnly {
		t.Fatal("twin must be evaluation-only")
	}
	if ha.c.CheckRedirect == nil || ha.streamC.CheckRedirect == nil {
		t.Fatal("twin must inherit a fail-closed redirect policy when the original has none")
	}
	if cap(ha.sem) != 4 {
		t.Fatalf("twin semaphore cap=%d want 4", cap(ha.sem))
	}
	if len(ha.creds) != 1 || ha.creds[0].Key != "fake-key-1" {
		t.Fatalf("twin creds=%v want the original key carried over", ha.creds)
	}
	if got := LiveCapable(twin); got == nil {
		t.Fatal("twin must be live-capable")
	}
	if got := LiveCapable(orig); got != nil {
		t.Fatal("a production adapter must never be live-capable")
	}
}

func mission95Twin(t *testing.T, srv *httptest.Server, ptype string) Adapter {
	t.Helper()
	p := config.ProviderConfig{ID: "p", Type: ptype, BaseURL: srv.URL, APIKey: "fake-eval-key", Enabled: true}
	if ptype == "bedrock" {
		t.Setenv("MISSION95_BEDROCK_SECRET", "fake-bedrock-secret")
		p.SecretKeyEnv = "MISSION95_BEDROCK_SECRET"
	}
	if ptype == "vertex" {
		p.VertexProject = "proj"
		p.VertexLocation = "us-central1"
	}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	twin, ok := EvaluationTwin(a)
	if !ok {
		t.Fatal("adapter must yield a twin")
	}
	return twin
}

func TestMission95LiveCompleteOutcomes(t *testing.T) {
	// Timeout: the upstream never answers, the caller context expires.
	// The handler also listens on hangDone: after the client times out,
	// the transport may keep the TCP connection open, so the server-side
	// request context alone is not a reliable unblock signal.
	hangDone := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hangDone:
		}
	}))
	defer hang.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := LiveComplete(ctx, mission95Twin(t, hang, "openai_compatible"), "m", "hi", 0)
	close(hangDone)
	if err != nil {
		t.Fatalf("timeout must be recorded, not returned: %v", err)
	}
	if out.ErrorType != LiveErrTimeout || out.Message != "upstream deadline exceeded" || !out.UpstreamAttempt {
		t.Fatalf("timeout outcome=%+v", out)
	}

	// Truncated body: declared length never fulfilled.
	trunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer trunc.Close()
	out, err = LiveComplete(context.Background(), mission95Twin(t, trunc, "openai_compatible"), "m", "hi", 10)
	if err != nil || out.ErrorType != LiveErrTransport || out.Message != "upstream response could not be read" {
		t.Fatalf("truncated outcome=%+v err=%v", out, err)
	}

	// Oversized body: beyond the evaluation read bound.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxLiveResponseBytes+1))
	}))
	defer huge.Close()
	out, err = LiveComplete(context.Background(), mission95Twin(t, huge, "openai_compatible"), "m", "hi", 10)
	if err != nil || out.ErrorType != LiveErrTransport || !strings.Contains(out.Message, "exceeds") {
		t.Fatalf("oversized outcome=%+v err=%v", out, err)
	}

	// Empty completion text is recorded as empty evidence, not success.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}],"usage":{"prompt_tokens":1,"completion_tokens":0}}`))
	}))
	defer empty.Close()
	out, err = LiveComplete(context.Background(), mission95Twin(t, empty, "openai_compatible"), "m", "hi", 10)
	if err != nil || out.ErrorType != LiveErrEmpty || out.Output != "" {
		t.Fatalf("empty outcome=%+v err=%v", out, err)
	}

	// Bedrock native shape decodes text and token counts.
	bedrock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"message":{"content":[{"text":"hello"}]}},"usage":{"inputTokens":3,"outputTokens":4}}`))
	}))
	defer bedrock.Close()
	out, err = LiveComplete(context.Background(), mission95Twin(t, bedrock, "bedrock"), "anthropic.claude-x", "hi", 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "hello" || out.PromptTokens != 3 || out.OutputTokens != 4 || out.ErrorType != LiveErrNone {
		t.Fatalf("bedrock outcome=%+v want hello/3/4", out)
	}
}

func TestMission95LiveRequestPathsAndBedrockDecode(t *testing.T) {
	bedrockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bedrockSrv.Close()
	ha := mission95Twin(t, bedrockSrv, "bedrock").(*httpAdapter)
	body, path, err := ha.liveCompletionRequest("model-x", "hi", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "inferenceConfig") {
		t.Fatalf("bedrock body=%s want inferenceConfig", body)
	}
	if path != bedrockModelPath("model-x", false) {
		t.Fatalf("bedrock path=%q want %q", path, bedrockModelPath("model-x", false))
	}
	vertexHa := mission95Twin(t, bedrockSrv, "vertex").(*httpAdapter)
	body, path, err = vertexHa.liveCompletionRequest("model-y", "hi", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "generationConfig") {
		t.Fatalf("vertex body=%s want generationConfig", body)
	}
	if path != vertexHa.vertexModelPath("model-y", false) {
		t.Fatalf("vertex path=%q want %q", path, vertexHa.vertexModelPath("model-y", false))
	}

	text, in, out := decodeLiveCompletion("bedrock",
		[]byte(`{"output":{"message":{"content":[{"text":"a"},{"text":"b"}]}},"usage":{"inputTokens":5,"outputTokens":6}}`))
	if text != "ab" || in != 5 || out != 6 {
		t.Fatalf("bedrock decode=%q %d %d want ab/5/6", text, in, out)
	}
	if got := (&httpAdapter{}).redactString(""); got != "" {
		t.Fatalf("redact empty=%q want empty", got)
	}
}

// --- event stream ---------------------------------------------------------------

func mission95EventHeader(name string, typ byte, val []byte) []byte {
	out := []byte{byte(len(name))}
	out = append(out, name...)
	out = append(out, typ)
	return append(out, val...)
}

func mission95EventStrHeader(name, val string) []byte {
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(val)))
	return mission95EventHeader(name, 7, append(lb[:], val...))
}

func mission95EventFrame(headers, payload []byte) []byte {
	total := 12 + len(headers) + len(payload) + 4
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))
	full := append(append([]byte{}, prelude...), headers...)
	full = append(full, payload...)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc32.ChecksumIEEE(full))
	return append(full, tail[:]...)
}

func TestMission95EventStreamFrames(t *testing.T) {
	// Prelude CRC mismatch fails loudly.
	frame := mission95EventFrame(nil, []byte(`{}`))
	frame[10] ^= 0xff
	if _, err := readEventStreamMessage(strings.NewReader(string(frame))); err == nil || !strings.Contains(err.Error(), "prelude CRC") {
		t.Fatalf("bad prelude CRC err=%v", err)
	}
	// Impossible lengths fail before any allocation.
	badLen := make([]byte, 12)
	binary.BigEndian.PutUint32(badLen[0:4], 10)
	binary.BigEndian.PutUint32(badLen[4:8], 0)
	binary.BigEndian.PutUint32(badLen[8:12], crc32.ChecksumIEEE(badLen[0:8]))
	if _, err := readEventStreamMessage(strings.NewReader(string(badLen))); err == nil || !strings.Contains(err.Error(), "invalid frame lengths") {
		t.Fatalf("bad lengths err=%v", err)
	}
	// A header parse failure propagates instead of decoding garbage.
	// Header claims a 5-byte name but only 2 bytes follow (valid CRCs).
	headers := []byte{5, 'a', 'b'}
	good := mission95EventFrame(headers, []byte(`{}`))
	// Recompute is unnecessary: the frame builder already produced valid
	// CRCs over these exact header bytes, so the failure must come from
	// the header parser itself.
	if _, err := readEventStreamMessage(strings.NewReader(string(good))); err == nil || !strings.Contains(err.Error(), "truncated header") {
		t.Fatalf("bad headers err=%v want truncated header", err)
	}
	// Exception-type fallbacks: explicit type, error code, then unknown.
	for _, tc := range []struct {
		headers []byte
		want    string
	}{
		{append(mission95EventStrHeader(":message-type", "exception"), mission95EventStrHeader(":exception-type", "FooErr")...), "FooErr"},
		{append(mission95EventStrHeader(":message-type", "exception"), mission95EventStrHeader(":error-code", "BarErr")...), "BarErr"},
		{mission95EventStrHeader(":message-type", "exception"), "UnknownException"},
	} {
		msg, err := readEventStreamMessage(strings.NewReader(string(mission95EventFrame(tc.headers, []byte(`{"m":1}`)))))
		if err != nil {
			t.Fatal(err)
		}
		if msg.EventType != tc.want {
			t.Fatalf("exception event type=%q want %q", msg.EventType, tc.want)
		}
	}
}

func TestMission95EventStreamHeaderTypes(t *testing.T) {
	uuid := make([]byte, 16)
	for i := range uuid {
		uuid[i] = byte(i)
	}
	var i16 [2]byte
	binary.BigEndian.PutUint16(i16[:], uint16(0xfffe))
	var i32 [4]byte
	binary.BigEndian.PutUint32(i32[:], uint32(0xfffffffd))
	var i64 [8]byte
	binary.BigEndian.PutUint64(i64[:], uint64(0xfffffffffffffffb))
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], 123456789)
	headers := []byte{}
	headers = append(headers, mission95EventHeader("btrue", 0, nil)...)
	headers = append(headers, mission95EventHeader("bfalse", 1, nil)...)
	headers = append(headers, mission95EventHeader("i8", 2, []byte{0xff})...)
	headers = append(headers, mission95EventHeader("i16", 3, i16[:])...)
	headers = append(headers, mission95EventHeader("i32", 4, i32[:])...)
	headers = append(headers, mission95EventHeader("i64", 5, i64[:])...)
	headers = append(headers, mission95EventHeader("ts", 8, ts[:])...)
	headers = append(headers, mission95EventStrHeader("s", "hi")...)
	var bl [2]byte
	binary.BigEndian.PutUint16(bl[:], 3)
	headers = append(headers, mission95EventHeader("bytes", 6, append(bl[:], []byte("abc")...))...)
	headers = append(headers, mission95EventHeader("id", 9, uuid)...)
	got, err := parseEventStreamHeaders(headers)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"btrue": "true", "bfalse": "false", "i8": "-1", "i16": "-2", "i32": "-3",
		"i64": "-5", "ts": "123456789", "s": "hi", "bytes": "abc",
		"id": "000102030405060708090a0b0c0d0e0f",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("header %s=%q want %q (all=%v)", k, got[k], v, got)
		}
	}
	// Every truncation shape fails instead of reading past the buffer.
	truncs := [][]byte{
		{5, 'a', 'b'},                                             // truncated name
		mission95EventHeader("i8", 2, nil),                        // truncated int8
		mission95EventHeader("i16", 3, []byte{1}),                 // truncated int16
		mission95EventHeader("i32", 4, []byte{1, 2}),              // truncated int32
		mission95EventHeader("i64", 5, []byte{1, 2, 3, 4}),        // truncated int64
		mission95EventHeader("bytes", 6, []byte{0}),               // truncated bytes length
		mission95EventHeader("bytes", 6, []byte{0, 10, 'a', 'b'}), // truncated bytes value
		mission95EventHeader("id", 9, []byte{1, 2, 3}),            // truncated uuid
		mission95EventHeader("x", 99, nil),                        // unknown type
	}
	for i, tc := range truncs {
		if _, err := parseEventStreamHeaders(tc); err == nil {
			t.Fatalf("truncation case %d must fail", i)
		}
	}
}

type mission95CloseCounter struct {
	io.Reader
	closed *int
}

func (c mission95CloseCounter) Close() error {
	*c.closed++
	return nil
}

func TestMission95EventStreamSSEReader(t *testing.T) {
	// An empty payload becomes an empty JSON object in the SSE frame.
	headers := mission95EventStrHeader(":event-type", "message")
	frame := mission95EventFrame(headers, nil)
	closed := 0
	r := newEventStreamSSEReader(mission95CloseCounter{Reader: strings.NewReader(string(frame)), closed: &closed})
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"payload":{}`) {
		t.Fatalf("SSE frame=%s want empty-object payload", data)
	}
	if err := r.Close(); err != nil || closed != 1 {
		t.Fatalf("SSE close err=%v closed=%d want delegated once", err, closed)
	}
	// Invalid JSON payloads fail loudly instead of leaking into SSE.
	badFrame := mission95EventFrame(headers, []byte(`{not json`))
	r2 := newEventStreamSSEReader(mission95CloseCounter{Reader: strings.NewReader(string(badFrame)), closed: &closed})
	if _, err := io.ReadAll(r2); err == nil || !strings.Contains(err.Error(), "invalid JSON payload") {
		t.Fatalf("bad payload err=%v want invalid JSON payload", err)
	}
}

// --- sigv4 ------------------------------------------------------------------------

func TestMission95SigV4EncodingEdges(t *testing.T) {
	if got := sigV4Escape("a b+c"); got != "a%20b%2Bc" {
		t.Fatalf("escape=%q want a%%20b%%2Bc", got)
	}
	if got := sigV4CanonicalURI(""); got != "/" {
		t.Fatalf("canonical empty=%q want /", got)
	}
	if got := percentDecodeSegment("x%41y"); got != "xAy" {
		t.Fatalf("percent decode=%q want xAy", got)
	}
	if got := percentDecodeSegment("100%"); got != "100%" {
		t.Fatalf("trailing percent=%q want literal", got)
	}
	if got := percentDecodeSegment("%ZZ"); got != "%ZZ" {
		t.Fatalf("bad hex=%q want literal", got)
	}
	if got := percentDecodeSegment("a%2"); got != "a%2" {
		t.Fatalf("short percent=%q want literal", got)
	}
}
