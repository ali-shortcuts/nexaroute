package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var errInjectedFileStore = errors.New("injected file store failure")

type faultTemp struct {
	fileStoreTemp
	stage string
}

func (f faultTemp) Write(p []byte) (int, error) {
	if f.stage == "write" {
		return 0, errInjectedFileStore
	}
	if f.stage == "short-write" {
		return 0, nil
	}
	return f.fileStoreTemp.Write(p)
}
func (f faultTemp) Sync() error {
	if f.stage == "temp-sync" {
		return errInjectedFileStore
	}
	return f.fileStoreTemp.Sync()
}
func (f faultTemp) Close() error {
	err := f.fileStoreTemp.Close()
	if f.stage == "temp-close" && err == nil {
		return errInjectedFileStore
	}
	return err
}

type faultDir struct{ fileStoreDir }

func (f faultDir) Sync() error { return errInjectedFileStore }

func injectFileStoreFailure(s *FileStore, stage string) {
	ops := defaultFileStoreFS()
	create := ops.createTemp
	ops.createTemp = func(dir, pattern string) (fileStoreTemp, error) {
		f, err := create(dir, pattern)
		if err != nil {
			return nil, err
		}
		return faultTemp{fileStoreTemp: f, stage: stage}, nil
	}
	if stage == "rename" {
		ops.rename = func(string, string) error { return errInjectedFileStore }
	}
	if stage == "dir-open" {
		ops.openDir = func(string) (fileStoreDir, error) { return nil, errInjectedFileStore }
	}
	if stage == "dir-sync" {
		open := ops.openDir
		ops.openDir = func(path string) (fileStoreDir, error) {
			d, err := open(path)
			if err != nil {
				return nil, err
			}
			return faultDir{d}, nil
		}
	}
	s.fs = ops // Set before concurrent access; operations themselves hold s.mu.
}

func diskDrafts(t *testing.T, path string) map[string]Definition {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc fileStateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Drafts
}

func TestFileStorePersistFailuresAndRecovery(t *testing.T) {
	for _, stage := range []string{"write", "short-write", "temp-sync", "temp-close", "rename", "dir-open", "dir-sync"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "store.json")
			s, err := OpenFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if err := s.SaveDraft(ctx, Definition{ID: "baseline"}); err != nil {
				t.Fatal(err)
			}
			injectFileStoreFailure(s, stage)
			err = s.SaveDraft(ctx, Definition{ID: "attempt"})
			cause := errInjectedFileStore
			if stage == "short-write" {
				cause = io.ErrShortWrite
			}
			if !errors.Is(err, cause) {
				t.Fatalf("SaveDraft: got %v, want cause %v", err, cause)
			}
			postRename := stage == "dir-open" || stage == "dir-sync"
			if errors.Is(err, ErrFileStoreCommitUncertain) != postRename {
				t.Fatalf("uncertainty classification: %v", err)
			}
			if _, ok := s.state.drafts["attempt"]; ok {
				t.Fatal("failed operation modified in-memory state")
			}
			disk := diskDrafts(t, path)
			if _, ok := disk["baseline"]; !ok {
				t.Fatal("baseline missing on disk")
			}
			if _, ok := disk["attempt"]; ok != postRename {
				t.Fatalf("attempt on disk=%v, postRename=%v", ok, postRename)
			}
			if postRename {
				if _, err := s.GetDraft(ctx, "baseline"); !errors.Is(err, ErrFileStoreCommitUncertain) {
					t.Fatalf("read on uncertain store: %v", err)
				}
				if err := s.SaveDraft(ctx, Definition{ID: "stale"}); !errors.Is(err, ErrFileStoreCommitUncertain) {
					t.Fatalf("write on uncertain store: %v", err)
				}
				if _, ok := diskDrafts(t, path)["attempt"]; !ok {
					t.Fatal("rejected stale write lost the renamed state")
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = OpenFileStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				if _, err := s.GetDraft(ctx, "attempt"); err != nil {
					t.Fatalf("reopen did not load renamed state: %v", err)
				}
			} else {
				s.fs = defaultFileStoreFS()
				if _, err := s.GetDraft(ctx, "baseline"); err != nil {
					t.Fatalf("pre-rename error poisoned store: %v", err)
				}
			}
			if err := s.SaveDraft(ctx, Definition{ID: "after"}); err != nil {
				t.Fatalf("mutation after recovery: %v", err)
			}
			disk = diskDrafts(t, path)
			if _, ok := disk["after"]; !ok {
				t.Fatal("subsequent mutation missing on disk")
			}
			if _, ok := disk["attempt"]; ok != postRename {
				t.Fatalf("attempt retained=%v, expected=%v", ok, postRename)
			}
		})
	}
}

func TestFileStoreCloseAndMutationRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		path := filepath.Join(t.TempDir(), "store.json")
		s, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mutationErr, closeErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			mutationErr = s.SaveDraft(context.Background(), Definition{ID: "raced"})
		}()
		go func() { defer wg.Done(); <-start; closeErr = s.Close() }()
		close(start)
		wg.Wait()
		if closeErr != nil || (mutationErr != nil && !errors.Is(mutationErr, os.ErrClosed)) {
			t.Fatalf("close=%v mutation=%v", closeErr, mutationErr)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if _, err := s.GetDraft(context.Background(), "raced"); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("read after Close: %v", err)
		}
		reopened, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		_, getErr := reopened.GetDraft(context.Background(), "raced")
		if mutationErr == nil && getErr != nil || mutationErr != nil && !errors.Is(getErr, ErrNotFound) {
			t.Errorf("mutation=%v, reopened read=%v", mutationErr, getErr)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileStoreExclusiveLock(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("file locking is not implemented on this platform")
	}
	path := filepath.Join(t.TempDir(), "store.json")
	first, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenFileStore(path); err == nil {
		_ = other.Close()
		t.Fatal("second store acquired an active lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(fmt.Errorf("reacquire after Close: %w", err))
	}
	defer second.Close()
}

func TestFileStoreEmptyOrNullExistingFileFailsClosed(t *testing.T) {
	for _, contents := range []string{"", "null", "  null\n"} {
		t.Run(fmt.Sprintf("%q", contents), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				store, err := OpenFileStore(path)
				if err == nil {
					_ = store.Close()
					t.Fatal("corrupt existing file was treated as a new empty store")
				}
				if !strings.Contains(err.Error(), "corrupt file store") {
					t.Fatalf("unexpected error; lock may not have been released: %v", err)
				}
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != contents {
				t.Fatalf("corrupt file was modified: %q, %v", raw, err)
			}
		})
	}
}
