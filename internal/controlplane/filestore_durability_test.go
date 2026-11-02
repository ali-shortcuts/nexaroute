package controlplane

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var errAtomicFault = errors.New("injected atomic write fault")

type atomicShortFile struct{ atomicFile }

func (f atomicShortFile) Write(b []byte) (int, error) { return f.atomicFile.Write(b[:len(b)-1]) }

type atomicBrokenDir struct{ atomicDir }

func (d atomicBrokenDir) Sync() error { return errAtomicFault }

func TestControlPlaneAtomicWriteFailures(t *testing.T) {
	for _, stage := range []string{"short", "rename", "dir-open", "dir-sync"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "data")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			fs := defaultAtomicFileFS()
			create, rename, open := fs.createTemp, fs.rename, fs.openDir
			if stage == "short" {
				fs.createTemp = func(dir, pattern string) (atomicFile, error) {
					f, e := create(dir, pattern)
					if e != nil {
						return nil, e
					}
					return atomicShortFile{f}, nil
				}
			}
			if stage == "rename" {
				fs.rename = func(a, b string) error { return errAtomicFault }
			} else {
				fs.rename = rename
			}
			if stage == "dir-open" {
				fs.openDir = func(string) (atomicDir, error) { return nil, errAtomicFault }
			}
			if stage == "dir-sync" {
				fs.openDir = func(path string) (atomicDir, error) {
					d, e := open(path)
					if e != nil {
						return nil, e
					}
					return atomicBrokenDir{d}, nil
				}
			}
			err := writeFileAtomicWithFS(fs, dir, "tmp-*", path, []byte("new"))
			uncertain := stage == "dir-open" || stage == "dir-sync"
			if err == nil || errors.Is(err, ErrFileStoreCommitUncertain) != uncertain {
				t.Fatalf("error: %v", err)
			}
			if stage == "short" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short: %v", err)
			}
			data, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			want := "old"
			if uncertain {
				want = "new"
			}
			if string(data) != want {
				t.Fatalf("disk=%q want=%q", data, want)
			}
		})
	}
}

func TestControlPlaneRejectsIncompleteDocuments(t *testing.T) {
	for _, data := range []string{"null", "{}", `{"schema":1}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(context.Background(), "x"); err == nil {
				t.Fatal("invalid state accepted")
			}
			if _, err := s.Put(context.Background(), "x", 0, []byte("value")); err == nil {
				t.Fatal("invalid state overwritten")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("file changed: %q %v", got, err)
			}
		})
	}
}
