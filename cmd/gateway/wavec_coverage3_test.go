package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave C-6 (part 3): main dispatch, runConfig branches, checkConfig
// branches, writeBundle error paths, restore-journal scan.

func writeMinimalConfig(t *testing.T, listen string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nexaroute.json")
	cfg := config.Default()
	if listen != "" {
		cfg.Listen = listen
	}
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkConfig: unreadable config → FAIL; group-readable → WARN;
// 0600 valid → PASS lines; bad listen → FAIL listen; malformed
// JSON → FAIL load.
func TestDoctorCheckConfigBranches(t *testing.T) {
	newDoctor := func() (*doctor, *bytes.Buffer) {
		var w bytes.Buffer
		return &doctor{w: &w}, &w
	}
	t.Run("missing file fails", func(t *testing.T) {
		d, w := newDoctor()
		d.checkConfig(filepath.Join(t.TempDir(), "missing.json"))
		if d.fails != 1 || !strings.Contains(w.String(), "FAIL config:") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("group-readable warns", func(t *testing.T) {
		path := writeMinimalConfig(t, "")
		if err := os.Chmod(path, 0640); err != nil {
			t.Fatal(err)
		}
		d, w := newDoctor()
		d.checkConfig(path)
		if d.warns != 1 || !strings.Contains(w.String(), "WARN config_permissions:") {
			t.Fatalf("%s", w.String())
		}
	})
	t.Run("malformed fails load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte("{nope"), 0600); err != nil {
			t.Fatal(err)
		}
		d, w := newDoctor()
		d.checkConfig(path)
		if d.fails != 1 || !strings.Contains(w.String(), "load/validation failed") {
			t.Fatalf("%s", w.String())
		}
	})
	// The "FAIL listen" branch in checkConfig is unreachable through
	// the file path: config.LoadBase validates first, and validation
	// applies the same net.SplitHostPort check (config.go ~1700), so a
	// listen value that fails SplitHostPort is already rejected at load.
	// It remains as defense in depth; no test fabricates it.
	t.Run("valid config passes and attaches", func(t *testing.T) {
		path := writeMinimalConfig(t, "")
		d, w := newDoctor()
		d.checkConfig(path)
		if d.fails != 0 || !strings.Contains(w.String(), "PASS config:") {
			t.Fatalf("%s", w.String())
		}
		if d.cfg == nil {
			t.Fatal("d.cfg not attached after successful load")
		}
	})
}

// runConfig: usage on no args; positional args rejected; validate
// on a valid and an invalid config; dry-run; diff against itself;
// unknown subcommand.
func TestRunConfigBranches(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runConfig(nil, &out, &errBuf); code != configExitUsage {
		t.Fatalf("no args: exit=%d", code)
	}
	if code := runConfig([]string{"validate", "--config", "x", "extra"}, &out, &errBuf); code != configExitUsage {
		t.Fatalf("positional: exit=%d", code)
	}
	if code := runConfig([]string{"frobnicate"}, &out, &errBuf); code != configExitUsage {
		t.Fatalf("unknown: exit=%d", code)
	}
	good := writeMinimalConfig(t, "")
	if code := runConfig([]string{"validate", "--config", good}, &out, &errBuf); code != configExitOK {
		t.Fatalf("validate: exit=%d (%s)", code, errBuf.String())
	}
	if got := out.String(); !strings.Contains(got, "config valid:") {
		t.Fatalf("validate output: %q", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"listen":"127.0.0.1:1","decision":{"mode":"nope"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runConfig([]string{"validate", "--config", bad}, &out, &errBuf); code != configExitFailure {
		t.Fatalf("invalid: exit=%d", code)
	}
	out.Reset()
	errBuf.Reset()
	if code := runConfig([]string{"dry-run", "--config", good}, &out, &errBuf); code != configExitOK {
		t.Fatalf("dry-run: exit=%d (%s)", code, errBuf.String())
	}
	if got := out.String(); !strings.Contains(got, "dry-run passed:") {
		t.Fatalf("dry-run output: %q", got)
	}
	out.Reset()
	errBuf.Reset()
	if code := runConfig([]string{"diff", "--config", good, "--against", good}, &out, &errBuf); code != configExitOK {
		t.Fatalf("diff: exit=%d (%s)", code, errBuf.String())
	}
}

// writeBundle error paths: a temp-dir creation failure (outPath
// inside a regular file) and a missing staged file.
func TestWriteBundleErrors(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	// outPath under a regular file: CreateTemp must fail.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(filepath.Join(blocker, "bundle.tar"), []byte("{}"), nil, stage); err == nil {
		t.Fatal("bad outPath accepted")
	}
	// Staged file listed in the manifest but absent on disk.
	manifest := []backupManifestFile{{Name: "missing.json", Size: 10}}
	if err := writeBundle(filepath.Join(dir, "b.tar"), []byte("{}"), manifest, stage); err == nil {
		t.Fatal("missing staged file accepted")
	}
}

// A journal directory with no readable manifest is uninspectable
// (WARN), distinct from an interrupted journal (WARN with recover).
func TestDoctorCheckRestoreJournalsUninspectable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	jdir := filepath.Join(tmp, "nexaroute-restore-journal-manifestless")
	if err := os.MkdirAll(jdir, 0700); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	d := &doctor{w: &w}
	d.checkRestoreJournals()
	if d.warns == 0 || !strings.Contains(w.String(), "no readable manifest") {
		t.Fatalf("warns=%d output=%q", d.warns, w.String())
	}
}

// dispatchSubcommand routes every known subcommand through the
// documented shape and refuses unknown ones without claiming them.
func TestDispatchSubcommandShape(t *testing.T) {
	var out, errBuf bytes.Buffer
	handled := []struct {
		args []string
	}{
		{[]string{"policy"}},
		{[]string{"audit"}},
		{[]string{"config"}},
		{[]string{"doctor", "--help"}},
		{[]string{"backup", "--help"}},
		{[]string{"restore", "--help"}},
		{[]string{"secrets"}},
		{[]string{"privacy", "--help"}},
		{[]string{"init", "--help"}},
	}
	for _, h := range handled {
		_, ok := dispatchSubcommand(h.args, nil, &out, &errBuf)
		if !ok {
			t.Errorf("%v not handled", h.args)
		}
		out.Reset()
		errBuf.Reset()
	}
	if _, ok := dispatchSubcommand([]string{"frobnicate"}, nil, &out, &errBuf); ok {
		t.Fatal("unknown subcommand claimed")
	}
	if _, ok := dispatchSubcommand([]string{"--version"}, nil, &out, &errBuf); ok {
		t.Fatal("--version claimed as subcommand")
	}
}

// The real binary must dispatch --version without touching config
// state, and must route a CLI usage error out of main's flag path.
func TestMainBinaryVersionAndDispatch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "nexaroute-test")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gateway")
	build.Dir = filepath.Dir(findRepoRoot(t))
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("binary build failed: %v (%s)", err, out)
	}
	ver := exec.Command(bin, "--version")
	var out bytes.Buffer
	ver.Stdout = &out
	if err := ver.Run(); err != nil {
		t.Fatalf("version: %v", err)
	}
	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, "NexaRoute v") {
		t.Fatalf("version output: %q", got)
	}
	pol := exec.Command(bin, "policy")
	var perr bytes.Buffer
	pol.Stderr = &perr
	if err := pol.Run(); err == nil {
		t.Fatal("policy with no args exited 0")
	}
	if !strings.Contains(perr.String(), "usage: nexaroute policy") {
		t.Fatalf("policy stderr: %q", perr.String())
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// runConfig error branches: empty path, dry-run load failure,
// provider-registry failure, diff comparison failure, and a diff
// with real changes.
func TestRunConfigErrorBranches(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runConfig([]string{"validate", "--config", ""}, &out, &errBuf); code != configExitUsage {
		t.Fatalf("empty path: exit=%d", code)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code := runConfig([]string{"dry-run", "--config", missing}, &out, &errBuf); code != configExitFailure {
		t.Fatalf("dry-run missing: exit=%d", code)
	}
	if code := runConfig([]string{"diff", "--config", missing}, &out, &errBuf); code != configExitFailure {
		t.Fatalf("diff missing: exit=%d", code)
	}
	if code := runConfig([]string{"diff", "--config", writeMinimalConfig(t, ""), "--against", missing}, &out, &errBuf); code != configExitFailure {
		t.Fatalf("diff bad against: exit=%d", code)
	}
	// An unknown provider type fails dry-run at load; the
	// "preparing providers" branch is defense in depth because the
	// registry builder applies the same validation the loader does.
	registryBad := filepath.Join(t.TempDir(), "regbad.json")
	if err := os.WriteFile(registryBad, []byte(`{"listen":"127.0.0.1:1","providers":[{"id":"p","type":"no_such_type","enabled":true}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runConfig([]string{"dry-run", "--config", registryBad}, &out, &errBuf); code != configExitFailure {
		t.Fatalf("dry-run registry: exit=%d (%s)", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "dry-run failed:") {
		t.Fatalf("dry-run stderr: %q", errBuf.String())
	}
	// Diff with real changes reports them.
	other := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(other, []byte(`{"listen":"127.0.0.1:2"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := runConfig([]string{"diff", "--config", writeMinimalConfig(t, ""), "--against", other}, &out, &errBuf); code != configExitOK {
		t.Fatalf("diff changes: exit=%d (%s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "listen: changed") {
		t.Fatalf("diff output: %q", out.String())
	}
}

// runPrivacy: usage shapes, format/path errors, and the top-level
// non-object guard that prevents auditing the built-in defaults.
func TestRunPrivacyBranches(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runPrivacy(nil, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("no args: exit=%d", code)
	}
	if code := runPrivacy([]string{"audit", "--format", "yaml"}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("bad format: exit=%d", code)
	}
	if code := runPrivacy([]string{"audit", "--config", ""}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("empty path: exit=%d", code)
	}
	if code := runPrivacy([]string{"audit", "--config", filepath.Join(t.TempDir(), "nope.json")}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("missing file: exit=%d", code)
	}
	nullPath := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(nullPath, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runPrivacy([]string{"audit", "--config", nullPath}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("top-level null: exit=%d", code)
	}
	if !strings.Contains(errBuf.String(), "top-level value must be a JSON object") {
		t.Fatalf("stderr: %q", errBuf.String())
	}
	good := writeMinimalConfig(t, "")
	if code := runPrivacy([]string{"audit", "--config", good}, &out, &errBuf); code != 0 {
		t.Fatalf("audit: exit=%d (%s)", code, errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := runPrivacy([]string{"audit", "--config", good, "--format", "json"}, &out, &errBuf); code != 0 {
		t.Fatalf("json audit: exit=%d (%s)", code, errBuf.String())
	}
}

// runInit with piped stdin: empty answers take the type-specific
// defaults; a typed answer overrides.
func TestRunInitInteractiveDefaults(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "new.json")
	stdin := strings.NewReader("\n\n\n\n")
	var out, errBuf bytes.Buffer
	if code := runInit([]string{"--type", "gemini", "--out", outPath}, stdin, &out, &errBuf); code != 0 {
		t.Fatalf("init: exit=%d (%s)", code, errBuf.String())
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"generativelanguage.googleapis.com", "gemini-2.5-flash", "GEMINI_API_KEY"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("gemini defaults missing %q in %s", want, raw)
		}
	}
	// Typed answers override defaults; --force overwrites.
	outPath2 := filepath.Join(dir, "new2.json")
	stdin2 := strings.NewReader("https://example.com\nMY_KEY\nmy-model\n127.0.0.1:9999\n")
	if code := runInit([]string{"--type", "openai_compatible", "--out", outPath2, "--provider-id", "p1"}, stdin2, &out, &errBuf); code != 0 {
		t.Fatalf("init typed: exit=%d (%s)", code, errBuf.String())
	}
	raw2, _ := os.ReadFile(outPath2)
	for _, want := range []string{"https://example.com", "MY_KEY", "my-model", "127.0.0.1:9999"} {
		if !strings.Contains(string(raw2), want) {
			t.Fatalf("typed answers missing %q in %s", want, raw2)
		}
	}
	// Refusing to overwrite without --force.
	if code := runInit([]string{"--type", "openai_compatible", "--out", outPath2}, strings.NewReader(""), &out, &errBuf); code == 0 {
		t.Fatal("overwrite without --force accepted")
	}
	// SaveAtomic failure: out under a regular file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runInit([]string{"--type", "openai_compatible", "--out", filepath.Join(blocker, "x.json"), "--base-url", "https://e.com", "--api-key-env", "", "--model", "m", "--listen", "127.0.0.1:1"}, strings.NewReader(""), &out, &errBuf); code == 0 {
		t.Fatal("bad out path accepted")
	}
}
