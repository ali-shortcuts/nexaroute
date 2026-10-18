package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	matrixViewsRe = regexp.MustCompile(`function views\(\) \{ return \{([^}]*)\}`)
	matrixPathRe  = regexp.MustCompile(`(?m)^  (/\S+):`)
	matrixIDRe    = regexp.MustCompile(`id="([^"]+)"`)
)

// TestContractMatrixDrift (#502 item 3): docs/CONTRACT_MATRIX.md
// must enumerate every registered page, every Admin OpenAPI path,
// and every shell element id — the matrix cannot silently drift
// from the sources it maps.
func TestContractMatrixDrift(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "CONTRACT_MATRIX.md"))
	if err != nil {
		t.Fatalf("read docs/CONTRACT_MATRIX.md: %v", err)
	}
	matrix := string(doc)

	js := readAppJS(t)
	m := matrixViewsRe.FindStringSubmatch(js)
	if m == nil {
		t.Fatal("views() registry not found in app.js")
	}
	for _, pair := range strings.Split(m[1], ",") {
		kv := strings.SplitN(pair, ":", 2)
		if len(kv) != 2 {
			continue
		}
		page, fn := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		if !strings.Contains(matrix, "`"+page+"`") || !strings.Contains(matrix, "`"+fn+"`") {
			t.Errorf("CONTRACT_MATRIX.md missing page %s / view %s", page, fn)
		}
	}

	spec, err := os.ReadFile(filepath.Join("..", "..", "docs", "api", "admin-openapi.yaml"))
	if err != nil {
		t.Fatalf("read admin-openapi.yaml: %v", err)
	}
	paths := matrixPathRe.FindAllStringSubmatch(string(spec), -1)
	if len(paths) == 0 {
		t.Fatal("no OpenAPI paths parsed")
	}
	for _, p := range paths {
		if !strings.Contains(matrix, "`"+p[1]+"`") {
			t.Errorf("CONTRACT_MATRIX.md missing OpenAPI path %s", p[1])
		}
	}

	htmlBytes, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, idm := range matrixIDRe.FindAllStringSubmatch(string(htmlBytes), -1) {
		if !strings.Contains(matrix, "`"+idm[1]+"`") {
			t.Errorf("CONTRACT_MATRIX.md missing shell id %s", idm[1])
		}
	}
}
