package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/policysim"
)

// Wave C-6: behavior-first coverage depth for cmd/gateway.

// checkExternal must never emit the connection value itself, and must
// reduce every failure to a class. Four branches: unconfigured,
// unset/empty env, failing ping, successful ping.
func TestDoctorCheckExternalBranches(t *testing.T) {
	newDoctor := func() (*doctor, *bytes.Buffer) {
		var w bytes.Buffer
		return &doctor{w: &w}, &w
	}
	t.Run("no env var configured", func(t *testing.T) {
		d, w := newDoctor()
		d.checkExternal("redis", "", func(ctx context.Context, value string) error { return nil })
		if d.fails != 1 || d.passes != 0 {
			t.Fatalf("counters: fails=%d passes=%d, want 1/0", d.fails, d.passes)
		}
		if got := w.String(); !strings.Contains(got, "FAIL redis:") || !strings.Contains(got, "no environment variable") {
			t.Fatalf("output: %q", got)
		}
	})
	t.Run("env var unset", func(t *testing.T) {
		const env = "NEXAROUTE_WAVEC_UNSET_DEP_URL"
		os.Unsetenv(env)
		d, w := newDoctor()
		d.checkExternal("redis", env, func(ctx context.Context, value string) error { return nil })
		if d.fails != 1 {
			t.Fatalf("fails=%d, want 1", d.fails)
		}
		if got := w.String(); !strings.Contains(got, "FAIL redis:") || strings.Contains(got, "://") {
			t.Fatalf("leaky or wrong output: %q", got)
		}
	})
	t.Run("ping fails", func(t *testing.T) {
		const env = "NEXAROUTE_WAVEC_DEP_URL"
		t.Setenv(env, "redis://localhost:6379/0")
		d, w := newDoctor()
		d.checkExternal("redis", env, func(ctx context.Context, value string) error {
			if value != "redis://localhost:6379/0" {
				t.Errorf("ping got value %q", value)
			}
			return errors.New("dial tcp: connection refused")
		})
		if d.fails != 1 {
			t.Fatalf("fails=%d, want 1", d.fails)
		}
		got := w.String()
		if !strings.Contains(got, "FAIL redis:") || !strings.Contains(got, "unreachable") {
			t.Fatalf("output: %q", got)
		}
		if strings.Contains(got, "6379") || strings.Contains(got, "refused") {
			t.Fatalf("ping error detail leaked: %q", got)
		}
	})
	t.Run("ping succeeds", func(t *testing.T) {
		const env = "NEXAROUTE_WAVEC_DEP_URL2"
		t.Setenv(env, "postgres://localhost/x")
		d, w := newDoctor()
		d.checkExternal("postgres", env, func(ctx context.Context, value string) error { return nil })
		if d.passes != 1 || d.fails != 0 {
			t.Fatalf("counters: passes=%d fails=%d, want 1/0", d.passes, d.fails)
		}
		if got := w.String(); !strings.Contains(got, "PASS postgres:") || strings.Contains(got, "postgres://localhost") {
			t.Fatalf("output: %q", got)
		}
	})
}

// controlPlaneBudgetAlertIO maps control-plane sentinel errors onto
// the alerter's contract errors, and copies payloads defensively.
// Drives Read (fresh → not-found), Write→Read round trip, and a
// stale-revision Write → conflict.
func TestControlPlaneBudgetAlertIOErrorMapping(t *testing.T) {
	ctx := context.Background()
	mgr, err := controlplane.NewManager(controlplane.NewMemoryStore(), controlplane.FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	io := &controlPlaneBudgetAlertIO{runtime: &controlPlaneRuntime{manager: mgr}}
	if _, _, err := io.Read(ctx); !errors.Is(err, httpapi.ErrBudgetAlertStoreNotFound) {
		t.Fatalf("fresh Read: %v, want ErrBudgetAlertStoreNotFound", err)
	}
	rev, err := io.Write(ctx, []byte(`{"mark":"x"}`), 0)
	if err != nil || rev == 0 {
		t.Fatalf("first Write: rev=%d err=%v", rev, err)
	}
	payload, gotRev, err := io.Read(ctx)
	if err != nil || string(payload) != `{"mark":"x"}` || gotRev != rev {
		t.Fatalf("Read after Write: payload=%q rev=%d err=%v", payload, gotRev, err)
	}
	// Stale revision must surface as the alerter's conflict sentinel,
	// not a raw control-plane error.
	if _, err := io.Write(ctx, []byte("stale"), 0); !errors.Is(err, httpapi.ErrBudgetAlertStoreConflict) {
		t.Fatalf("stale Write: %v, want ErrBudgetAlertStoreConflict", err)
	}
	// Mutating the returned payload must not corrupt the stored copy.
	payload[0] = 'X'
	again, _, err := io.Read(ctx)
	if err != nil || string(again) != `{"mark":"x"}` {
		t.Fatalf("defensive copy broken: %q %v", again, err)
	}
}

// openPostgresControlPlane must fail closed without a live database:
// the secret-safe error paths (no env name; env var unset) never
// touch the network, and no DSN material may reach the error.
func TestOpenPostgresControlPlaneFailClosedBranches(t *testing.T) {
	if _, _, err := openPostgresControlPlane(config.ControlPlaneConfig{Backend: "postgres"}); err == nil ||
		!strings.Contains(err.Error(), "postgres_dsn_env") {
		t.Fatalf("missing env name: %v", err)
	}
	const env = "NEXAROUTE_WAVEC_UNSET_PG_DSN"
	os.Unsetenv(env)
	_, _, err := openPostgresControlPlane(config.ControlPlaneConfig{Backend: "postgres", PostgresDSNEnv: env})
	if err == nil || !strings.Contains(err.Error(), env) {
		t.Fatalf("unset env var: %v", err)
	}
	if strings.Contains(err.Error(), "postgres://") || strings.Contains(err.Error(), "password") {
		t.Fatalf("error leaks DSN material: %v", err)
	}
}

// An invalid Redis URL must fail at parse time with a secret-safe
// error that does not echo the URL.
func TestOpenRedisControlPlaneInvalidURL(t *testing.T) {
	const env = "NEXAROUTE_WAVEC_BAD_REDIS_URL"
	t.Setenv(env, "://not a url \x7f")
	_, _, err := openRedisControlPlane(config.ControlPlaneConfig{Backend: "redis", RedisURLEnv: env})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid URL: %v", err)
	}
	if strings.Contains(err.Error(), "not a url") {
		t.Fatalf("error echoes URL material: %v", err)
	}
}

func writeReplayTraces(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func replayRecordJSON(t *testing.T, candidates []decision.Candidate, recordedWinner string) string {
	t.Helper()
	rec := policysim.ReplayRecord{
		Request:        decision.DecisionRequest{Candidates: candidates},
		RecordedWinner: recordedWinner,
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// readReplayRecords: missing file errors, blank lines are skipped,
// and a malformed line is reported with its line number.
func TestReadReplayRecordsErrors(t *testing.T) {
	if _, err := readReplayRecords(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("missing file accepted")
	}
	path := writeReplayTraces(t,
		replayRecordJSON(t, []decision.Candidate{{ID: "a", ProviderID: "p", RouterScore: 1, EWMALatencyMS: 1, HealthStatus: "healthy"}}, "a"),
		"",
		"   ",
		"{not json",
	)
	_, err := readReplayRecords(path)
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("malformed line: %v", err)
	}
	ok := writeReplayTraces(t,
		replayRecordJSON(t, []decision.Candidate{{ID: "a", ProviderID: "p", RouterScore: 1, EWMALatencyMS: 1, HealthStatus: "healthy"}}, "a"),
		"",
		replayRecordJSON(t, []decision.Candidate{{ID: "b", ProviderID: "p", RouterScore: 1, EWMALatencyMS: 1, HealthStatus: "healthy"}}, "b"),
	)
	records, err := readReplayRecords(ok)
	if err != nil || len(records) != 2 || records[1].RecordedWinner != "b" {
		t.Fatalf("records: %+v %v", records, err)
	}
}

// runPolicyReplay usage contract: missing flags → exit 2 with the
// usage line; unreadable traces → exit 1.
func TestRunPolicyReplayUsageAndErrors(t *testing.T) {
	cfgPath := policyCLIConfig(t)
	var out, errBuf bytes.Buffer
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "baseline-first"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("missing traces: exit=%d, want %d (%s)", code, policyExitUsage, errBuf.String())
	}
	if code := runPolicyReplay([]string{"--config", cfgPath, "--traces", "x"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("missing policy: exit=%d, want %d", code, policyExitUsage)
	}
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "baseline-first", "--traces", "x", "--format", "yaml"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("bad format: exit=%d, want %d", code, policyExitUsage)
	}
	if got := errBuf.String(); !strings.Contains(got, "usage: nexaroute policy replay") {
		t.Fatalf("usage line missing: %q", got)
	}
	errBuf.Reset()
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "baseline-first", "--traces", missing}, &out, &errBuf); code != policyExitError {
		t.Fatalf("missing traces file: exit=%d, want %d (%s)", code, policyExitError, errBuf.String())
	}
	bad := writeReplayTraces(t, "{bad json")
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "baseline-first", "--traces", bad}, &out, &errBuf); code != policyExitError {
		t.Fatalf("malformed traces: exit=%d, want %d", code, policyExitError)
	}
}

// runPolicyReplay end-to-end: one agreeing and one diverging
// record. The text report must show the divergence line; the JSON
// report must carry the agreed/diverged counts.
func TestRunPolicyReplayAgreeAndDiverge(t *testing.T) {
	cfgPath := policyCLIConfig(t)
	cands := []decision.Candidate{
		{ID: "a", ProviderID: "p", RouterScore: 100, EWMALatencyMS: 50, HealthStatus: "healthy"},
		{ID: "b", ProviderID: "p", RouterScore: 90, EWMALatencyMS: 10, HealthStatus: "healthy"},
	}
	traces := writeReplayTraces(t,
		replayRecordJSON(t, cands, "a"), // agrees with baseline-first
		replayRecordJSON(t, cands, "b"), // diverges
	)
	var out, errBuf bytes.Buffer
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "baseline-first", "--traces", traces}, &out, &errBuf); code != policyExitOK {
		t.Fatalf("replay: exit=%d (%s)", code, errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "replay 2 decision(s): 1 agree, 1 diverge") {
		t.Fatalf("summary line: %q", got)
	}
	if !strings.Contains(got, "DIVERGE") || !strings.Contains(got, "recorded=b") || !strings.Contains(got, "policy=a") {
		t.Fatalf("diverge detail: %q", got)
	}
	out.Reset()
	errBuf.Reset()
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "baseline-first", "--traces", traces, "--format", "json"}, &out, &errBuf); code != policyExitOK {
		t.Fatalf("json replay: exit=%d (%s)", code, errBuf.String())
	}
	var rep struct {
		PolicyID string `json:"policy_id"`
		Total    int    `json:"total"`
		Agreed   int    `json:"agreed"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json decode: %v (%s)", err, out.String())
	}
	if rep.PolicyID != "baseline-first" || rep.Total != 2 || rep.Agreed != 1 {
		t.Fatalf("json report: %+v", rep)
	}
}

// Policy CLI error branches: dispatch, flag parsing, config load
// failures, and the rollback audit-log guards (corrupt log, missing
// config, unknown policy in the log).
func TestPolicyErrorBranches(t *testing.T) {
	var out, errBuf bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.json")
	// Dispatch reaches replay.
	if code := runPolicy([]string{"replay", "--config", missing, "--policy", "p", "--traces", missing}, &out, &errBuf); code == 0 {
		t.Fatal("replay dispatch accepted missing config")
	}
	// Simulate: flag parse error, usage, load failure.
	if code := runPolicySimulate([]string{"--bogus"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("simulate bad flag: %d", code)
	}
	if code := runPolicySimulate([]string{"--config", missing}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("simulate no flags: %d", code)
	}
	if code := runPolicySimulate([]string{"--config", missing, "--policy", "p", "--fixtures", missing}, &out, &errBuf); code != policyExitError {
		t.Fatalf("simulate missing config: %d", code)
	}
	// Replay: usage, load failure.
	if code := runPolicyReplay([]string{"--config", missing}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("replay no flags: %d", code)
	}
	if code := runPolicyReplay([]string{"--config", missing, "--policy", "p", "--traces", missing}, &out, &errBuf); code != policyExitError {
		t.Fatalf("replay missing config: %d", code)
	}
	// Promote: flag parse error, usage.
	if code := runPolicyPromote([]string{"--bogus"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("promote bad flag: %d", code)
	}
	if code := runPolicyPromote([]string{"--config", missing}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("promote no policy: %d", code)
	}
	// Rollback: flag parse error.
	if code := runPolicyRollback([]string{"--bogus"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("rollback bad flag: %d", code)
	}
}

func TestPolicyRollbackAuditGuards(t *testing.T) {
	var out, errBuf bytes.Buffer
	path := policyCLIConfig(t)
	// Promote once so the audit log exists.
	if code := runPolicy([]string{"promote", "--config", path, "--policy", "latency-first"}, &out, &errBuf); code != 0 {
		t.Fatalf("promote: %d (%s)", code, errBuf.String())
	}
	// A directory as the audit log is a hard read error, not "no records".
	if code := runPolicy([]string{"rollback", "--config", path, "--audit-log", t.TempDir()}, &out, &errBuf); code != policyExitError {
		t.Fatalf("rollback dir audit log: %d", code)
	}
	if !strings.Contains(errBuf.String(), "policy rollback:") {
		t.Fatalf("stderr: %q", errBuf.String())
	}
	// A corrupt audit log is reported as corrupt.
	corrupt := filepath.Join(t.TempDir(), "corrupt.jsonl")
	if err := os.WriteFile(corrupt, []byte("{nope\n"), 0600); err != nil {
		t.Fatal(err)
	}
	errBuf.Reset()
	if code := runPolicy([]string{"rollback", "--config", path, "--audit-log", corrupt}, &out, &errBuf); code != policyExitError {
		t.Fatalf("rollback corrupt audit log: %d", code)
	}
	if !strings.Contains(errBuf.String(), "audit log is corrupt") {
		t.Fatalf("stderr: %q", errBuf.String())
	}
	// Missing config with a valid audit log: the hash check fails before
	// any write is attempted.
	errBuf.Reset()
	if code := runPolicy([]string{"rollback", "--config", filepath.Join(t.TempDir(), "no.json"), "--audit-log", defaultAuditLogPath(path)}, &out, &errBuf); code != policyExitError {
		t.Fatalf("rollback missing config: %d", code)
	}
	if !strings.Contains(errBuf.String(), "hash config") {
		t.Fatalf("stderr: %q", errBuf.String())
	}
	// An audit log whose from_policy is no longer defined: the pointer
	// application refuses instead of writing a dangling reference.
	ghost := filepath.Join(t.TempDir(), "ghost.jsonl")
	rec := policyAuditRecord{Timestamp: "2026-10-16T00:00:00Z", Action: "promote", FromPolicy: "ghost-policy", ToPolicy: "latency-first"}
	// The digest check must pass: compute the current config digest.
	sha, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.ConfigSHA256After = sha
	raw, _ := json.Marshal(rec)
	if err := os.WriteFile(ghost, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	errBuf.Reset()
	if code := runPolicy([]string{"rollback", "--config", path, "--audit-log", ghost}, &out, &errBuf); code != policyExitError {
		t.Fatalf("rollback ghost policy: %d", code)
	}
	if !strings.Contains(errBuf.String(), "is not defined in decision_policies") {
		t.Fatalf("stderr: %q", errBuf.String())
	}
	// The config is untouched by the refused rollback.
	if got := loadDecisionPolicy(t, path); got != "latency-first" {
		t.Fatalf("refused rollback mutated config: %q", got)
	}
}
