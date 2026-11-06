package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only lock behaviour")
	}
}

// TestAcquireMkdirAllError proves a lock path under a regular file fails
// closed with the MkdirAll error instead of silently using another path.
func TestAcquireMkdirAllError(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Acquire(filepath.Join(blocker, "nexaroute.lock")); err == nil || ok {
		t.Fatalf("Acquire under a file: ok=%v err=%v, want an error", ok, err)
	}
}

// TestAcquireOpenFileError uses /sys, where the directory exists (so
// MkdirAll succeeds) but creating a file fails: the filesystem is read-only.
func TestAcquireOpenFileError(t *testing.T) {
	requireLinux(t)
	if _, err := os.Stat("/sys"); err != nil {
		t.Skip("/sys not present")
	}
	if _, ok, err := Acquire("/sys/nexaroute.lock"); err == nil || ok {
		t.Fatalf("Acquire on /sys: ok=%v err=%v, want an error", ok, err)
	}
}

// TestAcquireChmodError drives the fchmod failure branch through the
// chmodFunc seam. The procfs route (symlink to /proc/self/status) is not
// deterministic: as root the open succeeds and chmod fails EPERM, but as
// an unprivileged user the O_RDWR open itself fails EACCES before chmod
// is reached, so the test failed on GitHub runners. The seam proves the
// branch — a chmod failure fails closed — on every user.
func TestAcquireChmodError(t *testing.T) {
	requireLinux(t)
	old := chmodFunc
	chmodFunc = func(*os.File, os.FileMode) error { return errors.New("chmod boom") }
	t.Cleanup(func() { chmodFunc = old })
	_, ok, err := Acquire(filepath.Join(t.TempDir(), "nexaroute.lock"))
	if err == nil || ok || !strings.Contains(err.Error(), "chmod boom") {
		t.Fatalf("Acquire with failing chmod: ok=%v err=%v, want a chmod failure", ok, err)
	}
}

// TestAcquireFlockError proves a non-EWOULDBLOCK flock failure is reported
// as an error, not misread as "another instance holds the lock". No
// filesystem in the test environment produces such an error naturally, so
// the test drives it through the flockFunc seam.
func TestAcquireFlockError(t *testing.T) {
	requireLinux(t)
	old := flockFunc
	flockFunc = func(fd int, how int) error { return syscall.EINVAL }
	t.Cleanup(func() { flockFunc = old })
	if _, ok, err := Acquire(filepath.Join(t.TempDir(), "nexaroute.lock")); !errors.Is(err, syscall.EINVAL) || ok {
		t.Fatalf("Acquire with failing flock: ok=%v err=%v, want EINVAL", ok, err)
	}
}

// TestAcquireSecuresPreexistingLooseFile documents why Acquire chmods: a
// lock file left behind with 0644 permissions is tightened to 0600.
func TestAcquireSecuresPreexistingLooseFile(t *testing.T) {
	requireLinux(t)
	path := filepath.Join(t.TempDir(), "nexaroute.lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lock, ok, err := Acquire(path)
	if err != nil || !ok {
		t.Fatalf("Acquire ok=%v err=%v", ok, err)
	}
	defer lock.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode=%v err=%v, want 0600", info.Mode(), err)
	}
}

// TestExecRunnerStartSuccess covers the execRunner success path: with a
// graphical session and a real executable on PATH, OpenBrowser launches it
// and reports which browser was used.
func TestExecRunnerStartSuccess(t *testing.T) {
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	bin := t.TempDir()
	script := filepath.Join(bin, "google-chrome")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := OpenBrowser("http://127.0.0.1:8080/", nil, nil)
	if err != nil || got != "google-chrome" {
		t.Fatalf("OpenBrowser=%q err=%v, want google-chrome", got, err)
	}
}
