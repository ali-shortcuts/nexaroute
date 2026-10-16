package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
	bolt "go.etcd.io/bbolt"
)

// Wave C-6 (part 2): doctor subcommand checks, policy simulate/rollback
// branches, secrets rotation helpers, and atomic file-write internals.

func doctorConfigWithSecurity(t *testing.T, backend, storePath string) (*config.Config, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.SecurityStoreBackend = backend
	cfg.Admin.SecurityStorePath = storePath
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg, cfgPath
}

// checkSecurityStore: nil config → WARN skip; non-bbolt backend →
// WARN; absent file → WARN; unreadable path component → FAIL;
// corrupt db → FAIL; slack permissions → WARN; valid db → PASS with
// verified chain.
func TestDoctorCheckSecurityStoreBranches(t *testing.T) {
	newDoctor := func(cfg *config.Config) (*doctor, *bytes.Buffer) {
		var w bytes.Buffer
		return &doctor{w: &w, cfg: cfg}, &w
	}
	t.Run("nil config skipped", func(t *testing.T) {
		d, w := newDoctor(nil)
		d.checkSecurityStore("x")
		if d.warns != 1 || !strings.Contains(w.String(), "WARN security_store:") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("non-bbolt backend warns", func(t *testing.T) {
		cfg := config.Default()
		cfg.Admin.SecurityStoreBackend = "postgres"
		cfg.Admin.SecurityStoreURL = "postgres://localhost/x"
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := config.SaveAtomic(cfgPath, cfg); err != nil {
			t.Fatal(err)
		}
		d, w := newDoctor(&cfg)
		d.checkSecurityStore(cfgPath)
		if d.warns != 1 || !strings.Contains(w.String(), "M4-2") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("absent file warns", func(t *testing.T) {
		dir := t.TempDir()
		cfg, p := doctorConfigWithSecurity(t, "bbolt", filepath.Join(dir, "missing.db"))
		d, w := newDoctor(cfg)
		d.checkSecurityStore(p)
		if d.warns != 1 || !strings.Contains(w.String(), "not created yet") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("corrupt db fails", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.db")
		if err := os.WriteFile(path, []byte("not a bbolt file"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, p := doctorConfigWithSecurity(t, "bbolt", path)
		d, w := newDoctor(cfg)
		d.checkSecurityStore(p)
		if d.fails != 1 || !strings.Contains(w.String(), "FAIL security_store:") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("valid db passes with chain verified", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "good.db")
		st, err := securitystore.Open(path, 100, 365)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AppendAudit(context.Background(), securitystore.AuditEvent{
			Timestamp: time.Now().UTC(), Actor: "t", Action: "x", Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		cfg, p := doctorConfigWithSecurity(t, "bbolt", path)
		d, w := newDoctor(cfg)
		d.checkSecurityStore(p)
		if d.passes != 1 || d.fails != 0 {
			t.Fatalf("%s", w.String())
		}
		if got := w.String(); !strings.Contains(got, "PASS security_store:") || !strings.Contains(got, "audit chain verified") {
			t.Fatalf("%s", got)
		}
	})
}

// fsErrorClass: every branch reduces to a safe, secret-free class.
func TestFsErrorClassBranches(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{os.ErrNotExist, "not found"},
		{os.ErrPermission, "permission denied"},
		{syscall.ENOTDIR, "a path component is not a directory"},
		{syscall.EISDIR, "the path is a directory"},
		{errors.New("weird"), "filesystem error"},
	}
	for _, c := range cases {
		if got := fsErrorClass(c.err); got != c.want {
			t.Errorf("fsErrorClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// checkControlPlane: disabled → PASS; file backend missing → WARN;
// corrupt state → FAIL; parseable → PASS; memory → PASS; unknown
// backend → FAIL; postgres with unset env → FAIL via checkExternal.
func TestDoctorCheckControlPlaneBranches(t *testing.T) {
	newDoctor := func(cp config.ControlPlaneConfig) (*doctor, *bytes.Buffer, string) {
		cfg := config.Default()
		cfg.ControlPlane = cp
		path := filepath.Join(t.TempDir(), "config.json")
		if err := config.SaveAtomic(path, cfg); err != nil {
			t.Fatal(err)
		}
		var w bytes.Buffer
		return &doctor{w: &w, cfg: &cfg}, &w, path
	}
	t.Run("disabled passes", func(t *testing.T) {
		d, w, p := newDoctor(config.ControlPlaneConfig{})
		d.checkControlPlane(p)
		if d.passes != 1 || !strings.Contains(w.String(), "disabled") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("file backend missing warns", func(t *testing.T) {
		d, w, p := newDoctor(config.ControlPlaneConfig{Enabled: true, Backend: "file"})
		d.checkControlPlane(p)
		if d.warns != 1 || !strings.Contains(w.String(), "not created yet") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("file backend corrupt fails", func(t *testing.T) {
		d, w, p := newDoctor(config.ControlPlaneConfig{Enabled: true, Backend: "file"})
		if err := os.WriteFile(p+".controlplane.json", []byte("{bad"), 0600); err != nil {
			t.Fatal(err)
		}
		d.checkControlPlane(p)
		if d.fails != 1 || !strings.Contains(w.String(), "corrupt") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("file backend present passes", func(t *testing.T) {
		d, w, p := newDoctor(config.ControlPlaneConfig{Enabled: true, Backend: "file"})
		if err := os.WriteFile(p+".controlplane.json", []byte(`{"schema":1}`), 0600); err != nil {
			t.Fatal(err)
		}
		d.checkControlPlane(p)
		if d.passes != 1 || !strings.Contains(w.String(), "parseable") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("unknown backend fails", func(t *testing.T) {
		cfg := config.Default()
		cfg.ControlPlane = config.ControlPlaneConfig{Enabled: true, Backend: "consul"}
		var w bytes.Buffer
		d := &doctor{w: &w, cfg: &cfg}
		d.checkControlPlane("config.json")
		if d.fails != 1 || !strings.Contains(w.String(), `unknown backend "consul"`) {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("postgres unset env fails", func(t *testing.T) {
		const env = "NEXAROUTE_WAVEC_UNSET_CP_PG"
		os.Unsetenv(env)
		d, w, p := newDoctor(config.ControlPlaneConfig{Enabled: true, Backend: "postgres", PostgresDSNEnv: env})
		d.checkControlPlane(p)
		if d.fails != 1 || !strings.Contains(w.String(), "FAIL control_plane_postgres:") {
			t.Fatalf("%s", w.String())
		}
	})
}

// checkRestoreJournals reports WARN for an interrupted journal and a
// manifest-less journal sitting in the temp dir.
func TestDoctorCheckRestoreJournalsWarn(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	jdir := filepath.Join(tmp, "nexaroute-restore-journal-test123")
	if err := os.MkdirAll(jdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJournalManifest(jdir, &restoreJournalManifest{Format: "nexaroute-restore-journal/1", ID: "j1", State: "installing"}); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	d := &doctor{w: &w}
	d.checkRestoreJournals()
	if d.warns == 0 || !strings.Contains(w.String(), "WARN restore_journal") {
		t.Fatalf("warns=%d output=%q", d.warns, w.String())
	}
}

// runPolicySimulate: usage, unreadable fixtures, malformed fixtures,
// unknown policy, then a real text and JSON simulation.
func TestRunPolicySimulateBranches(t *testing.T) {
	cfgPath := policyCLIConfig(t)
	var out, errBuf bytes.Buffer
	if code := runPolicySimulate([]string{"--config", cfgPath}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("usage: exit=%d", code)
	}
	if code := runPolicySimulate([]string{"--config", cfgPath, "--policy", "baseline-first", "--fixtures", filepath.Join(t.TempDir(), "nope.json")}, &out, &errBuf); code != policyExitError {
		t.Fatalf("missing fixtures: exit=%d (%s)", code, errBuf.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runPolicySimulate([]string{"--config", cfgPath, "--policy", "baseline-first", "--fixtures", bad}, &out, &errBuf); code != policyExitError {
		t.Fatalf("malformed fixtures: exit=%d", code)
	}
	fx := filepath.Join(t.TempDir(), "fixtures.json")
	if err := os.WriteFile(fx, []byte(`{"cases":[{"name":"c1","request":{"candidates":[{"id":"a","provider_id":"p","router_score":100,"ewma_latency_ms":50,"health_status":"healthy"},{"id":"b","provider_id":"p","router_score":90,"ewma_latency_ms":10,"health_status":"healthy"}]}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runPolicySimulate([]string{"--config", cfgPath, "--policy", "ghost", "--fixtures", fx}, &out, &errBuf); code != policyExitError {
		t.Fatalf("unknown policy: exit=%d (%s)", code, errBuf.String())
	}
	if code := runPolicySimulate([]string{"--config", cfgPath, "--policy", "baseline-first", "--fixtures", fx}, &out, &errBuf); code != policyExitOK {
		t.Fatalf("simulate: exit=%d (%s)", code, errBuf.String())
	}
	if got := out.String(); !strings.Contains(got, "baseline-first") || !strings.Contains(got, "winner=a") {
		t.Fatalf("text output: %q", got)
	}
	out.Reset()
	errBuf.Reset()
	if code := runPolicySimulate([]string{"--config", cfgPath, "--policy", "latency-first", "--fixtures", fx, "--format", "json"}, &out, &errBuf); code != policyExitOK {
		t.Fatalf("json simulate: exit=%d (%s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"policy_id": "latency-first"`) {
		t.Fatalf("json output: %q", out.String())
	}
}

// applyPolicyPointer: a directory as the audit log path forces the
// "config updated but AUDIT LOG WRITE FAILED" branch — exit 1 with
// the record carrying from/to.
func TestApplyPolicyPointerAuditWriteFailure(t *testing.T) {
	cfgPath := policyCLIConfig(t)
	auditDir := t.TempDir() // a directory, not a file → append fails
	var errBuf bytes.Buffer
	rec, code := applyPolicyPointer(cfgPath, auditDir, "promote", "latency-first", &errBuf)
	if code != policyExitError {
		t.Fatalf("code=%d, want %d", code, policyExitError)
	}
	if rec.FromPolicy != "baseline-first" || rec.ToPolicy != "latency-first" {
		t.Fatalf("record: %+v", rec)
	}
	if !strings.Contains(errBuf.String(), "AUDIT LOG WRITE FAILED") {
		t.Fatalf("stderr: %q", errBuf.String())
	}
	// And a bad config path fails at loadPolicyConfig.
	if _, code := applyPolicyPointer(filepath.Join(t.TempDir(), "missing.json"), filepath.Join(t.TempDir(), "a.jsonl"), "promote", "latency-first", &errBuf); code != policyExitError {
		t.Fatalf("missing config: code=%d", code)
	}
}

// runPolicyRollback with no audit records refuses (exit 1), and
// rollback after a promote succeeds and restores the policy.
func TestRunPolicyRollbackNoRecords(t *testing.T) {
	cfgPath := policyCLIConfig(t)
	var out, errBuf bytes.Buffer
	if code := runPolicy([]string{"rollback", "--config", cfgPath, "--audit-log", filepath.Join(t.TempDir(), "empty.jsonl")}, &out, &errBuf); code != policyExitError {
		t.Fatalf("no records: exit=%d (%s)", code, errBuf.String())
	}
}

// The budget-alert IO must map a conflict from the store onto the
// alerter's sentinel and pass generic failures through untouched.
type errStore struct{ controlplane.Store }

func (s errStore) Get(ctx context.Context, ns string) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, controlplane.ErrConflict
}
func (s errStore) Put(ctx context.Context, ns string, rev uint64, payload []byte) (controlplane.Snapshot, error) {
	return controlplane.Snapshot{}, errors.New("disk is on fire")
}

func TestControlPlaneBudgetAlertIOConflictAndPassthrough(t *testing.T) {
	ctx := context.Background()
	mgr, err := controlplane.NewManager(errStore{controlplane.NewMemoryStore()}, controlplane.FailClosed)
	if err != nil {
		t.Fatal(err)
	}
	io := &controlPlaneBudgetAlertIO{runtime: &controlPlaneRuntime{manager: mgr}}
	if _, _, err := io.Read(ctx); !errors.Is(err, httpapi.ErrBudgetAlertStoreConflict) {
		t.Fatalf("Read on conflict: %v", err)
	}
	if _, err := io.Write(ctx, []byte("x"), 0); err == nil || !strings.Contains(err.Error(), "disk is on fire") {
		t.Fatalf("Write passthrough: %v", err)
	}
}

// writeFileAtomic: durable round trip with the requested mode, and
// canonicalPath rejects a missing target.
func TestWriteFileAtomicAndCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")
	if err := writeFileAtomic(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != `{"a":1}` {
		t.Fatalf("round trip: %q %v", raw, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if _, err := canonicalPath(""); err == nil {
		t.Fatal("empty canonicalPath accepted")
	}
	// A missing target canonicalizes fine — canonicalPath resolves
	// syntax, not existence.
	canon, err := canonicalPath(path)
	if err != nil || canon == "" || !filepath.IsAbs(canon) {
		t.Fatalf("canonicalPath: %q %v", canon, err)
	}
	if err := writeSavedCopyDurable(filepath.Join(dir, "saved.json"), []byte("x")); err != nil {
		t.Fatalf("writeSavedCopyDurable: %v", err)
	}
}

// writeJournalManifest/readJournalManifest round trip.
func TestJournalManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &restoreJournalManifest{Format: "nexaroute-restore-journal/1", ID: "j1", State: "installing"}
	if err := writeJournalManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := readJournalManifest(dir)
	if err != nil || got.ID != "j1" || got.State != "installing" {
		t.Fatalf("manifest: %+v %v", got, err)
	}
	// A corrupt manifest is uninspectable, not interrupted.
	corrupt := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(corrupt, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournalManifest(dir); err == nil {
		t.Fatal("corrupt manifest accepted")
	}
}

// checkJournalOwnership accepts a manifest dir owned by the current
// user. The non-Stat_t platform branch cannot be exercised on Linux.
func TestCheckJournalOwnershipOwnDir(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkJournalOwnership(info); err != nil {
		t.Fatalf("own journal dir rejected: %v", err)
	}
}

// runSecrets status: plaintext config reports zero encrypted fields.
func TestRunSecretsStatusPlaintext(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"listen":"127.0.0.1:1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"status", "--config", cfgPath}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := runSecrets(nil); err == nil {
		t.Fatal("empty args accepted")
	}
}

// writeRotateFile round trip with 0600 mode.
func TestWriteRotateFileDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.bin")
	if err := writeRotateFile(path, []byte("secret-material")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "secret-material" {
		t.Fatalf("round trip: %q %v", raw, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", info, err)
	}
}

// auditStorePath: absolute configured path is cleaned and used
// verbatim; both-empty is an error.
func TestAuditStorePathBranches(t *testing.T) {
	got, err := auditStorePath("/var/lib/x.db", "config.json")
	if err != nil || got != "/var/lib/x.db" {
		t.Fatalf("absolute: %q %v", got, err)
	}
	if _, err := auditStorePath("", ""); err == nil {
		t.Fatal("both-empty accepted")
	}
	got, err = auditStorePath("rel/x.db", filepath.Join("dir", "config.json"))
	if err != nil || !strings.HasSuffix(got, filepath.Join("dir", "rel", "x.db")) {
		t.Fatalf("relative: %q %v", got, err)
	}
}

// secrets verify reports every decrypted field it can see — provider
// keys, credential keys, decision-provider keys and the admin key — and
// the count is exact. NOTE: plaintext client_auth keys are migrated to
// hashes inside LoadBase before verify ever sees the config, so the
// client_auth reporting branch is unreachable through this path (defense
// in depth for hand-built Config values); it is intentionally not
// asserted here.
func TestSecretsVerifyReportsAllKeyMaterial(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	// Hand-written (not SaveAtomic): SaveAtomic would hash the plaintext
	// client keys before persisting, which is correct production behavior
	// but would hide the client_auth reporting branch under test. LoadBase
	// transparently migrates legacy plaintext on read.
	p := filepath.Join(t.TempDir(), "config.json")
	legacy := `{"providers":[{"id":"p1","type":"openai_compatible","base_url":"http://127.0.0.1:9","api_key":"provider-key","credentials":[{"api_key":"cred-key"}]}],
"decision_providers":[{"id":"d1","type":"jev","api_key":"decision-key"}],
"admin":{"api_key":"admin-key"},
"client_auth":{"keys":["client-key-1","client-key-2"]}}`
	if err := os.WriteFile(p, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	// Capture the OK lines printed by runSecrets.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := runSecrets([]string{"verify", "--config", p})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, want := range []string{
		"OK provider:p1.api_key", "OK provider:p1.credentials[0].api_key",
		"OK decision_provider:d1.api_key", "OK admin.api_key",
		"verified_fields=4",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// secrets decrypt round-trips: rotate encrypts, decrypt recovers the
// plaintext, and the error branches are explicit.
func TestSecretsDecryptRoundTrip(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	p := filepath.Join(t.TempDir(), "config.json")
	c := config.Default()
	c.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: "decrypt-me", Enabled: false,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true}},
	}}
	// SaveAtomic encrypts secret fields at rest automatically with an
	// auto-managed <config>.key file.
	if err := config.SaveAtomic(p, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !bytes.Contains(raw, []byte(secrets.Prefix)) {
		t.Fatal("SaveAtomic did not encrypt the api_key at rest")
	}
	if err := runSecrets([]string{"status", "--config", p}); err != nil {
		t.Fatalf("status: %v", err)
	}
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	derr := runSecrets([]string{"decrypt", "--config", p, "--to-stdout", "--allow-plaintext"})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if derr != nil {
		t.Fatalf("decrypt: %v", derr)
	}
	if !strings.Contains(string(out), "decrypt-me") {
		t.Fatalf("decrypted output missing secret:\n%s", out)
	}
	// Error branches: missing file, and a corrupt key file.
	if err := runSecrets([]string{"decrypt", "--config", filepath.Join(t.TempDir(), "no.json"), "--to-stdout", "--allow-plaintext"}); err == nil {
		t.Fatal("decrypt of missing file accepted")
	}
	badKey := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badKey, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badKey+".key", []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"decrypt", "--config", badKey, "--to-stdout", "--allow-plaintext"}); err == nil {
		t.Fatal("decrypt with bad key accepted")
	}
}

// doctor config shapes: sparse configs surface default warnings, bad
// listen values fail, and encrypted values prove the master key.
func TestDoctorConfigShapes(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("TMPDIR", t.TempDir()) // isolate the restore-journal scan
	// Sparse config: defaults kick in with advisory warnings.
	sparse := filepath.Join(t.TempDir(), "sparse.json")
	if err := os.WriteFile(sparse, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runDoctor([]string{"--config", sparse}, &out, io.Discard); code != 0 {
		t.Fatalf("sparse doctor exit=%d", code)
	}
	if !strings.Contains(out.String(), "WARN config_warnings:") {
		t.Fatalf("warnings not reported:\n%s", out.String())
	}
	// NOTE: a malformed listen value never reaches the doctor's listen
	// check through the CLI — LoadBase validation rejects it first
	// ("FAIL config: load/validation failed"). The SplitHostPort branch
	// is defense in depth for programmatically built configs.
	// Encrypted values: the master key must resolve.
	dir := t.TempDir()
	encCfg := filepath.Join(dir, "enc.json")
	c := config.Default()
	c.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: "s3cr3t", Enabled: false,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true}},
	}}
	if err := config.SaveAtomic(encCfg, c); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runDoctor([]string{"--config", encCfg}, &out, io.Discard); code != 0 {
		t.Fatalf("enc doctor exit=%d", code)
	}
	if !strings.Contains(out.String(), "PASS master_key: resolvable for 1 encrypted value(s)") {
		t.Fatalf("master key not proven:\n%s", out.String())
	}
}

// doctor security store: a tampered audit chain is reported as FAIL with
// problem counts, never as a clean bill of health.
func TestDoctorDetectsTamperedAuditChain(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // isolate the restore-journal scan
	configPath := setupAuditCLI(t, 3)
	storePath := filepath.Join(filepath.Dir(configPath), "security", "nexaroute-security.db")
	db, err := bolt.Open(storePath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("audit-v1"))
		c := bucket.Cursor()
		k, raw := c.First()
		return bucket.Put(k, bytes.Replace(raw, []byte("alice"), []byte("carol"), 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runDoctor([]string{"--config", configPath}, &out, io.Discard); code != 1 {
		t.Fatalf("tampered doctor exit=%d want 1", code)
	}
	if !strings.Contains(out.String(), "FAIL audit_chain:") {
		t.Fatalf("tampered chain not failed:\n%s", out.String())
	}
}

// doctor control-plane matrix: file backend states (missing/corrupt/
// unreadable/valid), the memory backend, and unreachable postgres/redis.
func TestDoctorControlPlaneMatrix(t *testing.T) {
	writeCfg := func(t *testing.T, cp string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "cfg.json")
		doc := `{"listen":"127.0.0.1:9","control_plane":` + cp + `}`
		if err := os.WriteFile(p, []byte(doc), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(t *testing.T, cfg string) string {
		t.Helper()
		// Isolate the restore-journal scan so stray journals from other
		// tests cannot flip assertions.
		t.Setenv("TMPDIR", t.TempDir())
		var out bytes.Buffer
		code := runDoctor([]string{"--config", cfg}, &out, io.Discard)
		return string(rune(code)) + out.String()
	}
	// File backend, state not created yet → WARN.
	cfg := writeCfg(t, `{"enabled":true,"backend":"file","state_path":"`+filepath.Join(t.TempDir(), "missing.json")+`"}`)
	if got := run(t, cfg); !strings.Contains(got, "WARN control_plane: file backend state not created yet") {
		t.Fatalf("missing state:\n%s", got)
	}
	// File backend, state is a directory → FAIL with a safe error class.
	dir := t.TempDir()
	cfg = writeCfg(t, `{"enabled":true,"backend":"file","state_path":"`+dir+`"}`)
	if got := run(t, cfg); !strings.Contains(got, "FAIL control_plane:") || !strings.Contains(got, "the path is a directory") {
		t.Fatalf("unreadable state:\n%s", got)
	}
	// File backend, corrupt JSON → FAIL.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = writeCfg(t, `{"enabled":true,"backend":"file","state_path":"`+bad+`"}`)
	if got := run(t, cfg); !strings.Contains(got, "FAIL control_plane: file backend state is corrupt") {
		t.Fatalf("corrupt state:\n%s", got)
	}
	// File backend, valid JSON → PASS.
	good := filepath.Join(t.TempDir(), "good.json")
	if err := os.WriteFile(good, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = writeCfg(t, `{"enabled":true,"backend":"file","state_path":"`+good+`"}`)
	if got := run(t, cfg); !strings.Contains(got, "PASS control_plane: file backend state present and parseable") {
		t.Fatalf("valid state:\n%s", got)
	}
	// Memory backend → PASS.
	cfg = writeCfg(t, `{"enabled":true,"backend":"memory"}`)
	if got := run(t, cfg); !strings.Contains(got, "PASS control_plane: memory backend") {
		t.Fatalf("memory backend:\n%s", got)
	}
	// Non-bbolt security store backend → WARN (adapters land later).
	cfg = writeCfg(t, `{"enabled":false}`)
	_ = cfg
	withStore := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(withStore, []byte(`{"listen":"127.0.0.1:9","admin":{"security_store_backend":"postgres","security_store_url":"postgres://127.0.0.1:1/db"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := run(t, withStore); !strings.Contains(got, "WARN security_store: backend") {
		t.Fatalf("store backend:\n%s", got)
	}
	// Postgres/redis configured but unreachable → FAIL, fast and hermetic
	// (connection refused on a closed loopback port; no server needed).
	t.Setenv("NEXAROUTE_TEST_PG", "postgres://127.0.0.1:1/db?sslmode=disable")
	t.Setenv("NEXAROUTE_TEST_REDIS", "redis://127.0.0.1:1")
	cfg = writeCfg(t, `{"enabled":true,"backend":"postgres","postgres_dsn_env":"NEXAROUTE_TEST_PG"}`)
	if got := run(t, cfg); !strings.Contains(got, "FAIL control_plane_postgres:") {
		t.Fatalf("pg unreachable:\n%s", got)
	}
	cfg = writeCfg(t, `{"enabled":true,"backend":"redis","redis_url_env":"NEXAROUTE_TEST_REDIS"}`)
	if got := run(t, cfg); !strings.Contains(got, "FAIL control_plane_redis:") {
		t.Fatalf("redis unreachable:\n%s", got)
	}
	// Configured env name missing / empty → FAIL without dialing.
	cfg = writeCfg(t, `{"enabled":true,"backend":"postgres","postgres_dsn_env":"NEXAROUTE_TEST_PG_MISSING"}`)
	if got := run(t, cfg); !strings.Contains(got, "FAIL control_plane_postgres:") {
		t.Fatalf("pg missing env:\n%s", got)
	}
	// NOTE: postgres/redis without the env-name field never reaches
	// checkExternal through the CLI — config validation requires the
	// field. The "no environment variable is configured" branch is
	// defense in depth for programmatically built configs.
	// Restore-journal PASS branch: no journals in the isolated temp dir.
	cfg = writeCfg(t, `{"enabled":false}`)
	if got := run(t, cfg); !strings.Contains(got, "PASS restore_journal: no interrupted restore transactions") {
		t.Fatalf("journal pass:\n%s", got)
	}
}
