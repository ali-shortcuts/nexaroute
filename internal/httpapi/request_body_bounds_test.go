package httpapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bodyReadFindings checks direct json.NewDecoder and io.ReadAll uses of
// r.Body. It accepts a bound wrapped around that body, or an unconditional
// top-level body assignment earlier in the same function with no intervening
// reassignment. This is deliberately conservative, not general taint analysis:
// aliases, other APIs and interprocedural flows still require review.
func bodyReadFindings(name string, src []byte) (int, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return 0, nil, err
	}
	isBody := func(e ast.Expr) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != "Body" {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		return ok && id.Name == "r"
	}
	isBound := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		if !ok {
			return false
		}
		return (id.Name == "http" && s.Sel.Name == "MaxBytesReader" && len(c.Args) == 3 && isBody(c.Args[1])) ||
			(id.Name == "io" && s.Sel.Name == "LimitReader" && len(c.Args) == 2 && isBody(c.Args[0]))
	}
	checked := 0
	var findings []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// Only an assignment that is a direct statement of the function body
		// is definitely reached on every path to a later top-level statement.
		var topBounds []token.Pos
		for _, stmt := range fn.Body.List {
			a, ok := stmt.(*ast.AssignStmt)
			if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
				continue
			}
			if isBody(a.Lhs[0]) && isBound(a.Rhs[0]) {
				topBounds = append(topBounds, a.End())
			}
		}
		var writes []token.Pos
		var loops []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n.(type) {
			case *ast.ForStmt, *ast.RangeStmt:
				loops = append(loops, n)
			}
			a, ok := n.(*ast.AssignStmt)
			if ok {
				for _, e := range a.Lhs {
					if isBody(e) {
						writes = append(writes, a.Pos())
					}
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || len(c.Args) == 0 {
				return true
			}
			s, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := s.X.(*ast.Ident)
			if !ok || ((id.Name != "json" || s.Sel.Name != "NewDecoder") && (id.Name != "io" || s.Sel.Name != "ReadAll")) {
				return true
			}
			arg := c.Args[0]
			if isBound(arg) {
				checked++
				return true
			}
			if !isBody(arg) {
				return true
			}
			checked++
			bound := token.NoPos
			for _, p := range topBounds {
				if p < c.Pos() && p > bound {
					bound = p
				}
			}
			for _, p := range writes {
				if p > bound && p < c.Pos() {
					bound = token.NoPos
				}
				// A write lexically after the read may execute before the read
				// on the next iteration of an enclosing loop. Conservatively
				// reject it even when a branch might not take that write.
				for _, loop := range loops {
					if loop.Pos() < c.Pos() && c.Pos() < p && p < loop.End() {
						bound = token.NoPos
					}
				}
			}
			if bound == token.NoPos {
				findings = append(findings, fmt.Sprintf("%s: unbounded r.Body read", fset.Position(c.Pos())))
			}
			return true
		})
	}
	return checked, findings, nil
}

func TestAdminRequestBodyReadsAreBounded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		n, findings, err := bodyReadFindings(name, src)
		if err != nil {
			t.Fatal(err)
		}
		checked += n
		for _, finding := range findings {
			t.Error(finding)
		}
	}
	// The package currently has at least 14 direct bounded reads; a smaller
	// count means the scanner or source inventory changed and needs review.
	if checked < 14 {
		t.Fatalf("only %d direct body reads found; expected at least 14", checked)
	}
	t.Logf("checked %d direct body reads", checked)
}

func TestBodyReadFindingsRedGreen(t *testing.T) {
	cases := []struct {
		name, code string
		want       int
	}{
		{"bare-chain", `json.NewDecoder(r.Body).Decode(&x)`, 1},
		{"bare-decoder", `dec := json.NewDecoder(r.Body); dec.Decode(&x)`, 1},
		{"bare-decoder-unused", `dec := json.NewDecoder(r.Body); _ = dec`, 1},
		{"bare-readall", `io.ReadAll(r.Body)`, 1},
		{"inline", `json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&x)`, 0},
		{"bounded-decoder-variable", `dec := json.NewDecoder(io.LimitReader(r.Body, 4096)); dec.Decode(&x)`, 0},
		{"bounded-assignment", `r.Body = http.MaxBytesReader(w, r.Body, 4096); io.ReadAll(r.Body)`, 0},
		{"conditional-assignment", `if ok { r.Body = http.MaxBytesReader(w, r.Body, 4096) }; io.ReadAll(r.Body)`, 1},
		{"reassignment", `r.Body = http.MaxBytesReader(w, r.Body, 4096); r.Body = other; io.ReadAll(r.Body)`, 1},
		{"loop-reassignment-after-read", `r.Body = http.MaxBytesReader(w, r.Body, 4096); for more { io.ReadAll(r.Body); r.Body = other }`, 1},
		{"range-reassignment-after-read", `r.Body = http.MaxBytesReader(w, r.Body, 4096); for range items { json.NewDecoder(r.Body); r.Body = other }`, 1},
		{"outer-loop-reassignment", `r.Body = http.MaxBytesReader(w, r.Body, 4096); for more { for more { io.ReadAll(r.Body) }; r.Body = other }`, 1},
		{"loop-without-reassignment", `r.Body = http.MaxBytesReader(w, r.Body, 4096); for more { io.ReadAll(r.Body) }`, 0},
		{"inline-loop-reassignment", `for more { io.ReadAll(http.MaxBytesReader(w, r.Body, 4096)); r.Body = other }`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte("package p\nfunc f() { " + tc.code + " }")
			n, found, err := bodyReadFindings(tc.name, src)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 || len(found) != tc.want {
				t.Fatalf("checked=%d findings=%v, want 1 and %d", n, found, tc.want)
			}
		})
	}
}
