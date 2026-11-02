package nexaflow

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A process kill exercises lock release and visible rename state, not loss of
// kernel caches or a power-loss durability guarantee.
func TestFileStoreProcessKillRecovery(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires supported flock")
	}
	if stage := os.Getenv("NEXAFLOW_TEST_CRASH_STAGE"); stage != "" {
		s, err := OpenFileStore(os.Getenv("NEXAFLOW_TEST_CRASH_PATH"))
		if err != nil {
			t.Fatal(err)
		}
		pause := func() { fmt.Println("READY-TO-KILL"); time.Sleep(time.Hour) }
		if stage == "before-rename" {
			s.fs.rename = func(string, string) error { pause(); return nil }
		} else if stage == "after-rename" {
			s.fs.openDir = func(string) (fileStoreDir, error) { pause(); return nil, nil }
		} else {
			t.Fatal("unknown crash stage")
		}
		_ = s.SaveDraft(context.Background(), Definition{ID: "attempt"})
		t.Fatal("child unexpectedly returned")
	}
	for _, stage := range []string{"before-rename", "after-rename"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.json")
			s, err := OpenFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDraft(context.Background(), Definition{ID: "baseline"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestFileStoreProcessKillRecovery$")
			cmd.Env = append(os.Environ(), "NEXAFLOW_TEST_CRASH_STAGE="+stage, "NEXAFLOW_TEST_CRASH_PATH="+path)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				ready <- scanner.Scan() && scanner.Text() == "READY-TO-KILL"
			}()
			select {
			case ok := <-ready:
				if !ok {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatal("child did not reach fault point")
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal("child did not reach fault point in time")
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			reopened, err := OpenFileStore(path)
			if err != nil {
				t.Fatalf("reopen after kill: %v", err)
			}
			defer reopened.Close()
			if _, err := reopened.GetDraft(context.Background(), "baseline"); err != nil {
				t.Fatal(err)
			}
			_, err = reopened.GetDraft(context.Background(), "attempt")
			if stage == "after-rename" && err != nil {
				t.Fatalf("renamed state missing: %v", err)
			}
			if stage == "before-rename" && err == nil {
				t.Fatal("unrenamed draft appeared")
			}
			if err := reopened.SaveDraft(context.Background(), Definition{ID: "after-recovery"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFileStorePermissions(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires Unix file modes")
	}
	path := filepath.Join(t.TempDir(), "private", "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveDraft(context.Background(), Definition{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("store mode: %o", info.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm()&0007 != 0 {
		t.Fatalf("directory accessible to others: %o", dir.Mode().Perm())
	}
}
