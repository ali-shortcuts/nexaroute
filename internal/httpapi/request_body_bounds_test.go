package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAdminRequestBodyReadsAreBounded is a static contract on the handler
// package: every read of an inbound request body must be bounded. An unbounded
// read lets a client decide how much memory a handler allocates, and this
// package already bounds ~15 call sites at 4 KiB–1 MiB — a single unbounded
// outlier is invisible in review, which is exactly how one regressed (the
// autopilot action handler decoded r.Body with no limit).
//
// Two bounded forms are accepted:
//
//  1. an inline bound on the same line — http.MaxBytesReader(w, r.Body, n) or
//     io.LimitReader(r.Body, n);
//  2. a bound assigned to r.Body shortly before the read, as
//     admin_nexaflow_task8.go does (r.Body = http.MaxBytesReader(...)).
//
// Any other read of r.Body that decodes or copies it fails the test.
func TestAdminRequestBodyReadsAreBounded(t *testing.T) {
	readRe := regexp.MustCompile(`(Decode|ReadAll)\([^)]*r\.Body`)
	inlineRe := regexp.MustCompile(`MaxBytesReader|LimitReader`)
	assignRe := regexp.MustCompile(`r\.Body\s*=\s*.+(MaxBytesReader|LimitReader)`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked, bounded := 0, 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if !readRe.MatchString(line) {
				continue
			}
			checked++
			if inlineRe.MatchString(line) {
				bounded++
				continue
			}
			// Look back for a bound assigned to r.Body directly before this
			// read (same function, at most 8 lines up).
			ok := false
			for j := i - 1; j >= 0 && j >= i-8; j-- {
				if assignRe.MatchString(lines[j]) {
					ok = true
					break
				}
				if strings.Contains(lines[j], "func ") {
					break
				}
			}
			if ok {
				bounded++
				continue
			}
			t.Errorf("%s:%d reads r.Body without a bound: %s", name, i+1, strings.TrimSpace(line))
		}
	}
	// Sanity floor: the package has nine bounded read sites today. A much
	// lower number means the pattern drifted and the scan is checking nothing.
	if checked < 5 {
		t.Fatalf("only %d r.Body reads found — the scan is broken, not the code", checked)
	}
	if bounded != checked {
		t.Fatalf("bounded reads %d of %d — every body read must be bounded", bounded, checked)
	}
	t.Logf("bounded request-body reads: %d", bounded)
}
