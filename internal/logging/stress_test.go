package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStressConcurrentRotationStaysBounded(t *testing.T) {
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
	path := filepath.Join(t.TempDir(), "nexaroute.log")
	const maxBytes = 64 << 10
	const backups = 3
	w, err := NewRotatingWriter(path, maxBytes, backups)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				line := fmt.Sprintf("g=%d i=%d %s\n", g, i, strings.Repeat("x", 128))
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("write failed: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var total int64
	for i := 0; i <= backups; i++ {
		name := path
		if i > 0 {
			name = fmt.Sprintf("%s.%d", path, i)
		}
		if st, err := os.Stat(name); err == nil {
			if st.Size() > maxBytes {
				t.Fatalf("%s size=%d exceeds %d", name, st.Size(), maxBytes)
			}
			total += st.Size()
		}
	}
	if total > int64(backups+1)*maxBytes {
		t.Fatalf("total log storage=%d exceeds bound=%d", total, int64(backups+1)*maxBytes)
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, backups+1)); !os.IsNotExist(err) {
		t.Fatalf("unexpected backup beyond retention: %v", err)
	}
}
