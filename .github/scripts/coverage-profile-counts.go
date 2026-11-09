// coverage-profile-counts parses a Go coverage profile and counts only blocks
// that overlap a declared function, matching cmd/cover's -func behavior.
// Blocks outside functions are deliberately excluded from the denominator and
// numerator; scripts/coverage-gate.sh still cross-checks the resulting ratio
// against `go tool cover -func` before evaluating the exact threshold.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type position struct {
	line int
	col  int
}

type block struct {
	start      position
	end        position
	statements int64
	count      int64
}

type functionExtent struct {
	start position
	end   position
}

type functionCollector struct {
	fset    *token.FileSet
	extents []functionExtent
}

func (c *functionCollector) Visit(node ast.Node) ast.Visitor {
	fn, ok := node.(*ast.FuncDecl)
	if !ok || fn.Body == nil {
		return c
	}
	start := c.fset.Position(fn.Pos())
	end := c.fset.Position(fn.End())
	c.extents = append(c.extents, functionExtent{
		start: position{line: start.Line, col: start.Column},
		end:   position{line: end.Line, col: end.Column},
	})
	return c
}

func parsePosition(raw string) (position, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return position{}, fmt.Errorf("invalid source position %q", raw)
	}
	line, err := strconv.Atoi(parts[0])
	if err != nil || line < 1 {
		return position{}, fmt.Errorf("invalid source line %q", parts[0])
	}
	col, err := strconv.Atoi(parts[1])
	if err != nil || col < 1 {
		return position{}, fmt.Errorf("invalid source column %q", parts[1])
	}
	return position{line: line, col: col}, nil
}

func parseBlock(raw string, line int) (string, block, error) {
	fields := strings.Fields(raw)
	if len(fields) != 3 {
		return "", block{}, fmt.Errorf("invalid coverage record on line %d", line)
	}
	separator := strings.LastIndex(fields[0], ":")
	if separator <= 0 {
		return "", block{}, fmt.Errorf("missing file/position separator on line %d", line)
	}
	location := strings.Split(fields[0][separator+1:], ",")
	if len(location) != 2 {
		return "", block{}, fmt.Errorf("invalid coverage range on line %d", line)
	}
	start, err := parsePosition(location[0])
	if err != nil {
		return "", block{}, fmt.Errorf("line %d: %w", line, err)
	}
	end, err := parsePosition(location[1])
	if err != nil {
		return "", block{}, fmt.Errorf("line %d: %w", line, err)
	}
	statements, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || statements < 0 {
		return "", block{}, fmt.Errorf("invalid statement count on line %d", line)
	}
	count, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || count < 0 {
		return "", block{}, fmt.Errorf("invalid execution count on line %d", line)
	}
	return fields[0][:separator], block{start: start, end: end, statements: statements, count: count}, nil
}

func modulePath(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("open go.mod: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	return "", fmt.Errorf("module directive not found in go.mod")
}

func sourcePath(root, module, profilePath string) (string, error) {
	var source string
	if filepath.IsAbs(profilePath) {
		source = filepath.Clean(profilePath)
	} else {
		rel := filepath.FromSlash(profilePath)
		if strings.HasPrefix(profilePath, module+"/") {
			rel = filepath.FromSlash(strings.TrimPrefix(profilePath, module+"/"))
		}
		source = filepath.Join(root, rel)
	}
	rel, err := filepath.Rel(root, source)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("coverage source escapes module root: %q", profilePath)
	}
	return source, nil
}

func readProfile(path string) (map[string][]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open coverage profile: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read coverage profile: %w", err)
		}
		return nil, fmt.Errorf("empty coverage profile")
	}
	mode := strings.TrimSpace(scanner.Text())
	if mode != "mode: set" && mode != "mode: count" && mode != "mode: atomic" {
		return nil, fmt.Errorf("invalid coverage profile mode %q", mode)
	}
	files := make(map[string][]block)
	for line := 2; scanner.Scan(); line++ {
		name, item, err := parseBlock(scanner.Text(), line)
		if err != nil {
			return nil, err
		}
		files[name] = append(files[name], item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read coverage profile: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("coverage profile has no blocks")
	}
	return files, nil
}

func counts(profile string) (int64, int64, error) {
	root, err := os.Getwd()
	if err != nil {
		return 0, 0, fmt.Errorf("get working directory: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve module root: %w", err)
	}
	module, err := modulePath(root)
	if err != nil {
		return 0, 0, err
	}
	files, err := readProfile(profile)
	if err != nil {
		return 0, 0, err
	}
	var covered, total int64
	for profileName, blocks := range files {
		source, err := sourcePath(root, module, profileName)
		if err != nil {
			return 0, 0, err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, source, nil, parser.SkipObjectResolution)
		if err != nil {
			return 0, 0, fmt.Errorf("parse coverage source %q: %w", profileName, err)
		}
		collector := &functionCollector{fset: fset}
		ast.Walk(collector, parsed)
		sort.Slice(blocks, func(i, j int) bool {
			if blocks[i].start.line != blocks[j].start.line {
				return blocks[i].start.line < blocks[j].start.line
			}
			return blocks[i].start.col < blocks[j].start.col
		})
		for _, fn := range collector.extents {
			for _, item := range blocks {
				if item.start.line > fn.end.line || (item.start.line == fn.end.line && item.start.col >= fn.end.col) {
					break
				}
				if item.end.line < fn.start.line || (item.end.line == fn.start.line && item.end.col <= fn.start.col) {
					continue
				}
				total += item.statements
				if item.count > 0 {
					covered += item.statements
				}
			}
		}
	}
	if total <= 0 {
		return 0, 0, fmt.Errorf("coverage profile has no statements inside functions")
	}
	return covered, total, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: coverage-profile-counts <coverage.out>")
		os.Exit(2)
	}
	covered, total, err := counts(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverage profile counts:", err)
		os.Exit(1)
	}
	fmt.Printf("%d %d\n", covered, total)
}
