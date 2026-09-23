package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotatingWriterBoundsDiskAndDeletesOldBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexaroute.log")
	if err := os.WriteFile(path+".9", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotatingWriter(path, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := []byte(strings.Repeat("x", 300) + "\n")
	for i := 0; i < 30; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".9"); !os.IsNotExist(err) {
		t.Fatalf("stale backup was not deleted: %v", err)
	}
	var total int64
	for _, name := range []string{path, path + ".1", path + ".2"} {
		if st, err := os.Stat(name); err == nil {
			if st.Size() > 1024 {
				t.Fatalf("%s exceeds rotation size: %d", name, st.Size())
			}
			total += st.Size()
		}
	}
	if total > 3*1024 {
		t.Fatalf("rotated logs exceed total bound: %d", total)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("too many backups retained: %v", err)
	}
}

func TestRotatingWriterWithZeroBackupsKeepsOnlyCurrentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexaroute.log")
	w, err := NewRotatingWriter(path, 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte(strings.Repeat("x", 200) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("backup should not exist when retention=0: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() > 1024 {
		t.Fatalf("current log not bounded: size=%v err=%v", func() int64 { if st != nil { return st.Size() }; return -1 }(), err)
	}
}

func TestRateLimitedWriterSuppressesStormsAndReportsSummary(t *testing.T) {
	var dst bytes.Buffer
	w := NewRateLimitedWriter(&dst, 2, 20*time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(dst.String(), "line"); got != 2 {
		t.Fatalf("wrote %d lines, want 2: %q", got, dst.String())
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := w.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dst.String(), "suppressed=3") {
		t.Fatalf("missing suppression summary: %q", dst.String())
	}
	if !strings.Contains(dst.String(), "next") {
		t.Fatalf("new window did not resume writes: %q", dst.String())
	}
}
