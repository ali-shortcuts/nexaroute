package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bounds for operator-defined suite packs. Files are small declarative JSON
// documents in the existing Suite format; the bounds keep a misconfigured
// path from turning evaluation into an unbounded loader.
const (
	MaxOperatorSuites      = 32
	MaxSuitePackBytes      = 1 << 20
	MaxSuitesPathBytes     = 4096
	operatorSuiteExtension = ".json"
)

// ParseSuite parses one operator suite pack in the existing suite format: the
// JSON encoding of a Suite. Unknown fields are rejected so typos fail closed
// instead of silently changing scoring.
func ParseSuite(data []byte) (Suite, error) {
	if len(data) == 0 {
		return Suite{}, fmt.Errorf("suite pack is empty")
	}
	if len(data) > MaxSuitePackBytes {
		return Suite{}, fmt.Errorf("suite pack exceeds %d bytes", MaxSuitePackBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var s Suite
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("invalid suite pack: %w", err)
	}
	if err := ValidateSuite(s); err != nil {
		return Suite{}, err
	}
	return s, nil
}

// ParseSuites parses a pack that holds either one Suite object, a JSON array
// of suites, or an object with a "suites" array. All three spell the same
// Suite struct from suite.go, so operators keep writing the existing format.
func ParseSuites(data []byte) ([]Suite, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("suite pack is empty")
	}
	if len(data) > MaxSuitePackBytes {
		return nil, fmt.Errorf("suite pack exceeds %d bytes", MaxSuitePackBytes)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("suite pack is empty")
	}
	if strings.HasPrefix(trimmed, "[") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.DisallowUnknownFields()
		var list []Suite
		if err := dec.Decode(&list); err != nil {
			return nil, fmt.Errorf("invalid suite pack: %w", err)
		}
		return checkSuites(list)
	}
	if strings.HasPrefix(trimmed, "{") {
		// Probe for the {"suites": [...]} envelope before falling back to one suite.
		var envelope struct {
			Suites []Suite `json:"suites"`
		}
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.DisallowUnknownFields()
		// Decode into a generic map first to decide the shape without
		// accepting unknown fields silently.
		var raw map[string]json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid suite pack: %w", err)
		}
		if payload, ok := raw["suites"]; ok {
			if len(raw) != 1 {
				return nil, fmt.Errorf("invalid suite pack: \"suites\" envelope must hold only \"suites\"")
			}
			dec2 := json.NewDecoder(strings.NewReader(string(payload)))
			dec2.DisallowUnknownFields()
			if err := dec2.Decode(&envelope.Suites); err != nil {
				return nil, fmt.Errorf("invalid suite pack: %w", err)
			}
			return checkSuites(envelope.Suites)
		}
		s, err := ParseSuite([]byte(trimmed))
		if err != nil {
			return nil, err
		}
		return checkSuites([]Suite{s})
	}
	return nil, fmt.Errorf("invalid suite pack: must be a JSON object or array")
}

func checkSuites(list []Suite) ([]Suite, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("suite pack holds no suites")
	}
	if len(list) > MaxOperatorSuites {
		return nil, fmt.Errorf("suite pack exceeds %d suites", MaxOperatorSuites)
	}
	seen := map[string]struct{}{}
	for _, s := range list {
		if err := ValidateSuite(s); err != nil {
			return nil, err
		}
		if _, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("suite pack has duplicate suite %q", s.ID)
		}
		seen[s.ID] = struct{}{}
	}
	out := append([]Suite(nil), list...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// LoadSuitesFromPath loads operator-defined suites from a file or a directory
// of case packs in the existing suite format. A directory loads every *.json
// file (deterministic filename order); a file loads one pack (single suite,
// array, or {"suites": [...]} envelope).
//
// The load is atomic: any malformed pack aborts the whole load with a clear
// error and no partial result, and suite ids that collide with built-ins or
// with each other are rejected.
func LoadSuitesFromPath(path string) ([]Suite, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, nil
	}
	if len(trimmed) > MaxSuitesPathBytes {
		return nil, fmt.Errorf("suites path exceeds %d bytes", MaxSuitesPathBytes)
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return nil, fmt.Errorf("evaluation suites path: %w", err)
	}
	builtin := map[string]struct{}{}
	for _, s := range builtinSuites() {
		builtin[s.ID] = struct{}{}
	}
	collect := func(data []byte, what string) ([]Suite, error) {
		list, err := ParseSuites(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		return list, nil
	}
	var all []Suite
	if !info.IsDir() {
		data, err := os.ReadFile(trimmed)
		if err != nil {
			return nil, fmt.Errorf("evaluation suites file: %w", err)
		}
		all, err = collect(data, fmt.Sprintf("suite pack %q", trimmed))
		if err != nil {
			return nil, err
		}
	} else {
		entries, err := os.ReadDir(trimmed)
		if err != nil {
			return nil, fmt.Errorf("evaluation suites directory: %w", err)
		}
		var files []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), operatorSuiteExtension) {
				continue
			}
			files = append(files, filepath.Join(trimmed, name))
		}
		sort.Strings(files)
		if len(files) == 0 {
			return nil, fmt.Errorf("evaluation suites directory %q holds no suite packs (*.json)", trimmed)
		}
		for _, f := range files {
			info, err := os.Stat(f)
			if err != nil {
				return nil, fmt.Errorf("suite pack %q: %w", f, err)
			}
			if info.Size() > MaxSuitePackBytes {
				return nil, fmt.Errorf("suite pack %q exceeds %d bytes", f, MaxSuitePackBytes)
			}
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("suite pack %q: %w", f, err)
			}
			list, err := collect(data, fmt.Sprintf("suite pack %q", f))
			if err != nil {
				return nil, err
			}
			all = append(all, list...)
		}
	}
	if len(all) > MaxOperatorSuites {
		return nil, fmt.Errorf("operator suites exceed %d suites", MaxOperatorSuites)
	}
	seen := map[string]struct{}{}
	for _, s := range all {
		if _, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("operator suites have duplicate suite %q", s.ID)
		}
		if _, collides := builtin[s.ID]; collides {
			return nil, fmt.Errorf("operator suite %q collides with a built-in suite id", s.ID)
		}
		seen[s.ID] = struct{}{}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, nil
}
