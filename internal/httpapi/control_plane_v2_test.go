package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

func TestControlPlaneShellAssetsAreEmbedded(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, want := range []string{"/styles.css", "/app.js", `id="main"`, `data-page="overview"`, `data-page="providers"`, `data-page="routing"`, `data-page="activity"`, `data-page="settings"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	for _, asset := range []string{"web/styles.css", "web/app.js"} {
		if _, err := webFS.ReadFile(asset); err != nil {
			t.Fatalf("embedded asset %s missing: %v", asset, err)
		}
	}
}

func TestControlPlaneHasNoNativeBrowserDialogs(t *testing.T) {
	b, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	native := regexp.MustCompile(`(^|[^.A-Za-z0-9_])(prompt|confirm|alert)\s*\(`)
	if loc := native.FindIndex(b); loc != nil {
		t.Fatalf("app.js still contains a native browser dialog call near %q", string(b[loc[0]:loc[1]]))
	}
}

func TestControlPlaneKeepsSimpleAndAdvancedRoutingBackendOwned(t *testing.T) {
	b, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"/admin/api/simple-routes", "/admin/api/snapshot", "Candidate pools", "Advanced routing objects", "Backend compiled", "await refresh()"} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing expected UX contract %q", want)
		}
	}
	for _, forbidden := range []string{"chooseBestDeployment(", "selectHealthyDeployment(", "clientSideFailover(", "rankDeploymentsByLatency("} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js contains forbidden client-side routing authority %q", forbidden)
		}
	}
}

func TestControlPlaneAccessibilityContracts(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, want := range []string{`class="skip-link"`, `id="main"`, `tabindex="-1"`, `viewport-fit=cover`, `aria-live="polite"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing accessibility/responsive contract %q", want)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "prefers-reduced-motion") {
		t.Fatal("styles.css missing reduced-motion contract")
	}
}
