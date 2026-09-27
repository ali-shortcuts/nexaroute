package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

func TestControlPlaneV2AssetsAreEmbedded(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, want := range []string{"/control-plane-v2.css", "/control-plane-v2.js"} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	for _, asset := range []string{"web/control-plane-v2.css", "web/control-plane-v2.js"} {
		if _, err := webFS.ReadFile(asset); err != nil {
			t.Fatalf("embedded asset %s missing: %v", asset, err)
		}
	}
}

func TestControlPlaneV2HasNoNativeBrowserDialogs(t *testing.T) {
	files := []string{"web/app.js", "web/control-plane-v2.js"}
	native := regexp.MustCompile(`(^|[^.A-Za-z0-9_])(prompt|confirm|alert)\s*\(`)
	for _, name := range files {
		b, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if loc := native.FindIndex(b); loc != nil {
			start := loc[0] - 60
			if start < 0 {
				start = 0
			}
			end := loc[1] + 120
			if end > len(b) {
				end = len(b)
			}
			t.Fatalf("%s still contains a native browser dialog call near %q", name, string(b[start:end]))
		}
	}
}

func TestControlPlaneV2KeepsSimpleAndAdvancedRouting(t *testing.T) {
	b, err := webFS.ReadFile("web/control-plane-v2.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		"saveSimpleRoute",
		"/admin/api/candidate-pools",
		"/admin/api/route-profiles",
		"/admin/api/virtual-endpoints",
		"advancedPool",
		"advancedProfile",
		"advancedVirtual",
		"nextProviderIdentity",
		"Select all",
		"Saved credential",
		"fa:",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("control-plane-v2.js missing expected UX contract %q", want)
		}
	}
}

func TestControlPlaneV2AccessibilityContracts(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, want := range []string{
		`id="mainContent"`,
		`tabindex="-1"`,
		`viewport-fit=cover`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing accessibility/responsive contract %q", want)
		}
	}

	jsBytes, err := webFS.ReadFile("web/control-plane-v2.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	for _, want := range []string{
		"installExperiencePolish",
		"const skip=q('.cp-skip-link')",
		"Skip to main content",
		"رفتن به محتوای اصلی",
		"aria-current",
		"aria-controls",
		"aria-live",
		"MutationObserver",
		"ArrowDown",
		"ArrowUp",
		"prefers-reduced-motion",
	} {
		if want == "prefers-reduced-motion" {
			cssBytes, cssErr := webFS.ReadFile("web/control-plane-v2.css")
			if cssErr != nil {
				t.Fatal(cssErr)
			}
			if !strings.Contains(string(cssBytes), want) {
				t.Fatalf("control-plane-v2.css missing accessibility contract %q", want)
			}
			continue
		}
		if !strings.Contains(js, want) {
			t.Fatalf("control-plane-v2.js missing accessibility contract %q", want)
		}
	}
}

func TestControlPlaneV2DoesNotOwnRoutingDecisions(t *testing.T) {
	b, err := webFS.ReadFile("web/control-plane-v2.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)

	// The UX layer may compose backend primitives, but must not grow an
	// independent client-side health/latency/capability routing algorithm.
	for _, forbidden := range []string{
		"chooseBestDeployment(",
		"selectHealthyDeployment(",
		"clientSideFailover(",
		"rankDeploymentsByLatency(",
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("control-plane-v2.js contains forbidden client-side routing authority %q", forbidden)
		}
	}

	for _, required := range []string{
		"/admin/api/candidate-pools",
		"/admin/api/route-profiles",
		"/admin/api/virtual-endpoints",
		"await refresh()",
	} {
		if !strings.Contains(js, required) {
			t.Fatalf("control-plane-v2.js missing backend-authority contract %q", required)
		}
	}
}
