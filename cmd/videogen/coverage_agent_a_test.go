package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type exitPanic struct{ code int }

func installExitStub(t *testing.T) {
	t.Helper()
	old := osExit
	osExit = func(code int) { panic(exitPanic{code}) }
	t.Cleanup(func() { osExit = old })
}

// callMain runs main with swapped os.Args/stdout/stderr and converts an
// osExit stub panic into a returned exit code, mirroring real exit
// semantics (the process would stop at that point).
func callMain(t *testing.T, args []string) (out, errOut string, exitCode *int) {
	t.Helper()
	oldArgs, oldStdout, oldStderr := os.Args, os.Stdout, os.Stderr
	outF, err := os.CreateTemp(t.TempDir(), "main-stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "main-stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	os.Args, os.Stdout, os.Stderr = args, outF, errF
	defer func() {
		os.Args, os.Stdout, os.Stderr = oldArgs, oldStdout, oldStderr
		_ = outF.Close()
		_ = errF.Close()
		outBytes, _ := os.ReadFile(outF.Name())
		errBytes, _ := os.ReadFile(errF.Name())
		out, errOut = string(outBytes), string(errBytes)
	}()
	defer func() {
		if r := recover(); r != nil {
			if ep, ok := r.(exitPanic); ok {
				code := ep.code
				exitCode = &code
				return
			}
			panic(r)
		}
	}()
	main()
	return out, errOut, exitCode
}

func TestMainVersionFlag(t *testing.T) {
	installExitStub(t)
	for _, flag := range []string{"-version", "--version"} {
		out, _, exitCode := callMain(t, []string{"videogen", flag})
		if exitCode != nil {
			t.Fatalf("main %s exited with %d, want a normal return", flag, *exitCode)
		}
		if !strings.Contains(out, "NexaRoute videogen v") {
			t.Fatalf("main %s stdout=%q, want version line", flag, out)
		}
	}
}

func TestMainExitCodes(t *testing.T) {
	installExitStub(t)
	t.Setenv("NEXAROUTE_VIDEO_TOKEN", "")

	_, errOut, exitCode := callMain(t, []string{"videogen"})
	if exitCode == nil || *exitCode != 2 || !strings.Contains(errOut, "usage: videogen") {
		t.Fatalf("no-args: exit=%v stderr=%q, want exit 2 with usage", exitCode, errOut)
	}

	_, errOut, exitCode = callMain(t, []string{"videogen", "bogus-command"})
	if exitCode == nil || *exitCode != 2 || !strings.Contains(errOut, `unknown command "bogus-command"`) {
		t.Fatalf("unknown command: exit=%v stderr=%q, want exit 2", exitCode, errOut)
	}

	_, errOut, exitCode = callMain(t, []string{"videogen", "providers"})
	if exitCode == nil || *exitCode != 1 || !strings.Contains(errOut, "Video Gateway token is missing") {
		t.Fatalf("providers without token: exit=%v stderr=%q, want exit 1", exitCode, errOut)
	}
}

// TestDownloadAssetResolveOutputPathError removes the process working
// directory so filepath.Abs fails: the CLI must report the path problem
// instead of attempting the download.
func TestDownloadAssetResolveOutputPathError(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	client, err := newAPIClient("http://127.0.0.1:8080", "secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", "out.mp4"); err == nil || !strings.Contains(err.Error(), "resolve output path") {
		t.Fatalf("deleted-cwd download err=%v, want resolve output path", err)
	}
}

// TestDownloadAssetCheckOutputPathError uses a self-referential symlink:
// Stat fails with ELOOP, which is neither success nor "not exist".
func TestDownloadAssetCheckOutputPathError(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	client, err := newAPIClient("http://127.0.0.1:8080", "secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", loop); err == nil || !strings.Contains(err.Error(), "check output path") {
		t.Fatalf("symlink-loop download err=%v, want check output path", err)
	}
}

// TestDownloadAssetCreateTempFailure has the gateway delete the output
// directory before responding: the client must fail creating its temp
// file instead of writing anywhere else. The mutation runs before the
// headers are sent, so the directory is already gone when the client
// tries to create the temp file.
func TestDownloadAssetCreateTempFailure(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	server := newDownloadServer(t, func() {
		_ = os.RemoveAll(dir)
	}, []byte("video bytes"), false)
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "create temporary download file") {
		t.Fatalf("removed-dir download err=%v, want create temporary download file", err)
	}
}

// TestDownloadAssetSecureTempFileError drives the fchmod failure branch
// through the secureTempFile seam: no filesystem API can make chmod fail
// on an otherwise valid fresh temp file.
func TestDownloadAssetSecureTempFileError(t *testing.T) {
	old := secureTempFile
	secureTempFile = func(f *os.File) error { return errors.New("chmod boom") }
	t.Cleanup(func() { secureTempFile = old })

	server := gatewayTestServer(t, []byte("video bytes"))
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "secure temporary download file") {
		t.Fatalf("chmod-failure download err=%v, want secure temporary download file", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("failed download left output behind: %v", err)
	}
}

// TestDownloadAssetAppearedDuringDownload has the gateway create the
// output file mid-request: the client must refuse to overwrite it and
// leave the pre-existing file untouched.
func TestDownloadAssetAppearedDuringDownload(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	server := newDownloadServer(t, func() {
		if err := os.WriteFile(out, []byte("stale"), 0o600); err != nil {
			t.Error(err)
		}
	}, []byte("video bytes"), false)
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "appeared during download") {
		t.Fatalf("appeared-file download err=%v, want appeared during download", err)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "stale" {
		t.Fatalf("output=%q err=%v, want the stale file untouched", data, err)
	}
}

// TestDownloadAssetRecheckOutputPathError has the gateway replace the
// output path with a self-referential symlink mid-request: the recheck
// Stat fails with ELOOP, which is neither success nor "not exist".
func TestDownloadAssetRecheckOutputPathError(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	server := newDownloadServer(t, func() {
		if err := os.Symlink("out.mp4", out); err != nil {
			t.Error(err)
		}
	}, []byte("video bytes"), false)
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "recheck output path") {
		t.Fatalf("symlink-swap download err=%v, want recheck output path", err)
	}
}

// TestDownloadAssetPublishFailure deletes the output directory (with the
// client's temp file inside it) after the client has started copying the
// body: the final rename must fail instead of silently dropping the
// asset. The mutation runs after the headers are flushed, so the client
// has already created its temp file and is blocked waiting for the body.
func TestDownloadAssetPublishFailure(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	server := newDownloadServer(t, func() {
		time.Sleep(500 * time.Millisecond)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
		_ = os.Remove(dir)
	}, []byte("video bytes"), true)
	defer server.Close()
	client, err := newAPIClient(server.URL, "secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadAsset(context.Background(), "job-123", "asset-1", out); err == nil || !strings.Contains(err.Error(), "publish downloaded asset") {
		t.Fatalf("removed-dir publish err=%v, want publish downloaded asset", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom read") }
func (errReader) Close() error             { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRequestReadError proves a response body that fails mid-read is
// reported as a read error rather than a decode error or a success.
func TestRequestReadError(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: errReader{}, Request: req}, nil
	})
	client, err := newAPIClient("http://127.0.0.1:8080", "secret-token", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Providers(context.Background()); err == nil || !strings.Contains(err.Error(), "read Video Gateway response") {
		t.Fatalf("Providers with failing body err=%v, want read Video Gateway response", err)
	}
}

// newDownloadServer serves one asset download. mutate runs inside the
// handler; with mutateAfterFlush=false it runs before the headers are
// sent (the client has not created its temp file yet), with
// mutateAfterFlush=true it runs after the headers are flushed (the
// client has created its temp file and is waiting for the body).
func newDownloadServer(t *testing.T, mutate func(), body []byte, mutateAfterFlush bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mutateAfterFlush {
			mutate()
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if mutateAfterFlush {
			mutate()
		}
		_, _ = w.Write(body)
	}))
}
