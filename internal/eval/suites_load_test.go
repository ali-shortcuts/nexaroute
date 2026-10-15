package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorPack_ValidLoadsAndRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pack.json")
	if err := os.WriteFile(path, []byte(validOperatorPack()), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadSuitesFromPath(path)
	if err != nil {
		t.Fatalf("valid pack rejected: %v", err)
	}
	if len(list) != 1 || list[0].ID != "operator-smoke" {
		t.Fatalf("unexpected suites: %+v", list)
	}
	// A valid pack runs through the deterministic runner.
	r := NewRunner()
	exec, err := NewReplayExecutor([]Outcome{
		{CaseID: "op-case-one", Status: OutcomeOK, Output: "ok"},
		{CaseID: "op-case-two", Status: OutcomeOK, Output: "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.RunWithSuite(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "operator-smoke"}, list[0], exec)
	if err != nil {
		t.Fatalf("operator suite run failed: %v", err)
	}
	if !res.Scoreable || res.Score != 1 {
		t.Fatalf("operator suite run not scoreable pass: %+v", res)
	}
}

func validOperatorPack() string {
	return `{"id":"operator-smoke","version":"1","dimension":"coding","MinSamples":1,"MaxCases":128,` +
		`"cases":[{"id":"op-case-one","weight":1,"expectation":{"kind":"exact_match","expected":"ok"}},` +
		`{"id":"op-case-two","weight":1,"expectation":{"kind":"exact_match","expected":"ok"}}]}`
}

func TestOperatorPack_MalformedRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `{"id":`},
		{"unknown field", `{"id":"x","version":"1","dimension":"coding","MinSamples":1,"cases":[{"id":"c","weight":1,"expectation":{"kind":"exact_match"}}],"typo_field":true}`},
		{"unknown expectation", `{"id":"x","version":"1","dimension":"coding","MinSamples":1,"cases":[{"id":"c","weight":1,"expectation":{"kind":"vibes"}}]}`},
		{"duplicate case", `{"id":"x","version":"1","dimension":"coding","MinSamples":1,"cases":[{"id":"c","weight":1,"expectation":{"kind":"exact_match"}},{"id":"c","weight":1,"expectation":{"kind":"exact_match"}}]}`},
		{"no cases", `{"id":"x","version":"1","dimension":"coding","MinSamples":1,"cases":[]}`},
		{"empty", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSuites([]byte(tc.body)); err == nil {
				t.Fatalf("malformed pack accepted: %s", tc.body)
			}
		})
	}
}

func TestOperatorPack_DirectoryLoadIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a-good.json"), []byte(validOperatorPack()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b-bad.json"), []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("directory with a malformed pack must be rejected as a whole")
	} else if !strings.Contains(err.Error(), "b-bad.json") {
		t.Fatalf("error must name the malformed pack, got: %v", err)
	}
}

func TestOperatorPack_BuiltinCollisionRejected(t *testing.T) {
	body := `{"id":"coding","version":"1","dimension":"coding","MinSamples":1,` +
		`"cases":[{"id":"c","weight":1,"expectation":{"kind":"exact_match"}}]}`
	dir := t.TempDir()
	path := filepath.Join(dir, "pack.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuitesFromPath(path); err == nil {
		t.Fatal("pack colliding with a built-in suite id must be rejected")
	} else if !strings.Contains(err.Error(), "collides") {
		t.Fatalf("error must report the collision, got: %v", err)
	}
}

func TestOperatorPack_BuiltinCatalogUnchanged(t *testing.T) {
	before := Catalog()
	if len(before) != 8 {
		t.Fatalf("built-in catalog size changed: %d", len(before))
	}
	for _, e := range before {
		if e.Source != "" && e.Source != "builtin" {
			t.Fatalf("built-in entry source changed: %+v", e)
		}
		if _, ok := LookupSuite(e.ID); !ok {
			t.Fatalf("built-in suite %q not discoverable", e.ID)
		}
	}
}
