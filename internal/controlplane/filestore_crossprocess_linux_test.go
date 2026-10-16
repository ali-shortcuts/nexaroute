//go:build linux

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Cross-process regression tests for issue #406.
//
// The test binary re-executes itself as a child "store process": the parent
// test spawns real OS processes that share one FileStore data directory, the
// closest faithful reproduction of two gateway processes behind a supervisor.
//
// TestMain dispatches the child modes before the test runner starts.
func TestMain(m *testing.M) {
	switch os.Getenv("NEXAROUTE_CP_CHILD") {
	case "contended-writer":
		os.Exit(cpContendedWriterMain())
	case "burst-writer":
		os.Exit(cpBurstWriterMain())
	}
	os.Exit(m.Run())
}

// cpContendedWriterMain: NEXAROUTE_CP_COUNT optimistic-revision puts against
// NEXAROUTE_CP_NS, retrying ErrConflict. Prints
// "CP_CHILD_RESULT puts=%d conflicts=%d".
func cpContendedWriterMain() int {
	path := os.Getenv("NEXAROUTE_CP_PATH")
	ns := os.Getenv("NEXAROUTE_CP_NS")
	count, _ := strconv.Atoi(os.Getenv("NEXAROUTE_CP_COUNT"))
	tag := os.Getenv("NEXAROUTE_CP_TAG")
	store, err := NewFileStore(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: open store: %v\n", err)
		return 1
	}
	ctx := context.Background()
	puts, conflicts := 0, 0
	for i := 0; i < count; i++ {
		retries := 0
		for {
			snap, gerr := store.Get(ctx, ns)
			var rev uint64
			if gerr == nil {
				rev = snap.Revision
			} else if !errors.Is(gerr, ErrNotFound) {
				fmt.Fprintf(os.Stderr, "child: get: %v\n", gerr)
				return 1
			}
			_, perr := store.Put(ctx, ns, rev, []byte(fmt.Sprintf("%s-%d", tag, i)))
			if perr == nil {
				puts++
				break
			}
			if errors.Is(perr, ErrConflict) {
				conflicts++
				retries++
				if retries > 50000 {
					fmt.Fprintf(os.Stderr, "child: conflict retry budget exhausted\n")
					return 1
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "child: put: %v\n", perr)
			return 1
		}
	}
	fmt.Printf("CP_CHILD_RESULT puts=%d conflicts=%d\n", puts, conflicts)
	return 0
}

// cpBurstWriterMain: tight put loop used as a SIGKILL target. Tracks the
// revision locally; any conflict is unexpected (single writer).
func cpBurstWriterMain() int {
	path := os.Getenv("NEXAROUTE_CP_PATH")
	ns := os.Getenv("NEXAROUTE_CP_NS")
	count, _ := strconv.Atoi(os.Getenv("NEXAROUTE_CP_COUNT"))
	store, err := NewFileStore(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: open store: %v\n", err)
		return 1
	}
	ctx := context.Background()
	var rev uint64
	snap, gerr := store.Get(ctx, ns)
	if gerr == nil {
		rev = snap.Revision
	} else if !errors.Is(gerr, ErrNotFound) {
		fmt.Fprintf(os.Stderr, "child: get: %v\n", gerr)
		return 1
	}
	for i := 0; i < count; i++ {
		snap, err := store.Put(ctx, ns, rev, []byte(fmt.Sprintf("burst-%d", i)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "child: put: %v\n", err)
			return 1
		}
		rev = snap.Revision
	}
	return 0
}

func cpSpawnChild(t *testing.T, mode, path, ns, tag string, count int) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"NEXAROUTE_CP_CHILD="+mode,
		"NEXAROUTE_CP_PATH="+path,
		"NEXAROUTE_CP_NS="+ns,
		"NEXAROUTE_CP_TAG="+tag,
		"NEXAROUTE_CP_COUNT="+strconv.Itoa(count),
	)
	return cmd
}

// TestFileStoreTwoProcessesNoLostUpdates is the #406 regression test: two real
// processes perform contended optimistic-revision puts against one namespace.
// Every acknowledged write must be durable and visible, so the final revision
// must equal the total number of successful puts; conflicts must surface as
// ErrConflict (retried by the children), never as silent overwrites.
//
// RED (pre-fix): final revision < total puts — updates silently lost.
// GREEN (post-fix): final revision == total puts.
func TestFileStoreTwoProcessesNoLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const writers = 2
	const putsPerWriter = 100

	type childResult struct {
		puts      int
		conflicts int
		err       error
	}
	results := make(chan childResult, writers)
	for w := 0; w < writers; w++ {
		go func(tag string) {
			cmd := cpSpawnChild(t, "contended-writer", path, "shared", tag, putsPerWriter)
			out, err := cmd.CombinedOutput()
			r := childResult{err: err}
			if err == nil {
				for _, line := range strings.Split(string(out), "\n") {
					if rest, ok := strings.CutPrefix(line, "CP_CHILD_RESULT "); ok {
						_, _ = fmt.Sscanf(rest, "puts=%d conflicts=%d", &r.puts, &r.conflicts)
					}
				}
				if r.puts == 0 {
					r.err = fmt.Errorf("child %s printed no result (output: %q)", tag, out)
				}
			} else {
				r.err = fmt.Errorf("child %s: %v (output: %q)", tag, err, out)
			}
			results <- r
		}(string(rune('A' + w)))
	}

	totalPuts, totalConflicts := 0, 0
	for w := 0; w < writers; w++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("writer failed: %v", r.err)
			}
			totalPuts += r.puts
			totalConflicts += r.conflicts
		case <-time.After(120 * time.Second):
			t.Fatal("timed out waiting for writer children")
		}
	}
	if totalPuts != writers*putsPerWriter {
		t.Fatalf("children acknowledged %d puts, want %d", totalPuts, writers*putsPerWriter)
	}

	// Fresh-process view of the store: every acknowledged write durable.
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	final, err := store.Get(context.Background(), "shared")
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	t.Logf("two-process: %d puts acknowledged, %d conflicts observed, final revision %d",
		totalPuts, totalConflicts, final.Revision)
	if final.Revision != uint64(totalPuts) {
		t.Fatalf("LOST UPDATES: %d acknowledged puts but final revision is %d (%d updates silently lost)",
			totalPuts, final.Revision, uint64(totalPuts)-final.Revision)
	}
}

// TestFileStoreCrashKillChaos kills a writer process with SIGKILL at
// randomized points and verifies, from a fresh process each time, that the
// store always lands at the old or the new revision (never torn), that
// revisions are monotonic, and that the advisory lock is not wedged by the
// kill (a subsequent write must succeed promptly).
//
// This is an invariant proof: it passes on the pre-fix code too (persist was
// already atomic); it guards the new locking against crash-wedging.
func TestFileStoreCrashKillChaos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const iterations = 12
	rng := rand.New(rand.NewSource(406))
	var lastRev uint64
	for i := 0; i < iterations; i++ {
		cmd := cpSpawnChild(t, "burst-writer", path, "chaos", "", 400)
		if err := cmd.Start(); err != nil {
			t.Fatalf("iter %d: start: %v", i, err)
		}
		time.Sleep(time.Duration(rng.Intn(6000)) * time.Microsecond)
		_ = cmd.Process.Kill() // SIGKILL; may already have exited: not an error for us
		_ = cmd.Wait()

		// Fresh-process verification: store must be intact and monotonic.
		store, err := NewFileStore(path)
		if err != nil {
			t.Fatalf("iter %d: reopen: %v", i, err)
		}
		snap, err := store.Get(context.Background(), "chaos")
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("iter %d: store unreadable after SIGKILL: %v", i, err)
		}
		if snap.Revision < lastRev {
			t.Fatalf("iter %d: revision went backwards: %d -> %d", i, lastRev, snap.Revision)
		}
		// The kill must not wedge the advisory lock: a new write succeeds.
		putCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		after, err := store.Put(putCtx, "chaos", snap.Revision, []byte(fmt.Sprintf("postkill-%d", i)))
		cancel()
		if err != nil {
			t.Fatalf("iter %d: put after SIGKILL failed (lock wedged?): %v", i, err)
		}
		lastRev = after.Revision
	}
	t.Logf("crash chaos: %d SIGKILL iterations, final revision %d, monotonic, lock never wedged", iterations, lastRev)
}
