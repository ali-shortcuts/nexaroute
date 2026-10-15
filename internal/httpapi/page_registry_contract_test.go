package httpapi

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The SPA page registry lives in three places that must agree: the
// nav buttons in index.html, the views() map, and the meta table
// that render() destructures for the page title/description. A page
// present in nav/views but missing from meta makes render() throw
// (pageMeta returns undefined and the destructure fails), which is
// how the Policies page shipped dead. This contract keeps the three
// sets identical, and also guards against duplicate top-level
// function declarations, which JavaScript resolves silently
// (the later declaration wins) after union merges.
func TestPageRegistryContract(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)

	navSet := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-page="([a-z]+)"`).FindAllStringSubmatch(string(index), -1) {
		navSet[m[1]] = true
	}

	viewsBlock := regexp.MustCompile(`(?s)function views\(\) \{ return \{(.*?)\} \}`).FindStringSubmatch(js)
	if viewsBlock == nil {
		t.Fatal("views() literal not found in app.js")
	}
	viewsSet := map[string]bool{}
	for _, m := range regexp.MustCompile(`(\w+): \w+View`).FindAllStringSubmatch(viewsBlock[1], -1) {
		viewsSet[m[1]] = true
	}

	metaBlock := regexp.MustCompile(`(?s)const meta = \{(.*?)\};`).FindStringSubmatch(js)
	if metaBlock == nil {
		t.Fatal("meta literal not found in app.js")
	}
	metaSet := map[string]bool{}
	for _, m := range regexp.MustCompile(`(\w+): \[`).FindAllStringSubmatch(metaBlock[1], -1) {
		metaSet[m[1]] = true
	}

	for _, pair := range []struct {
		name string
		a, b map[string]bool
	}{
		{"nav vs views", navSet, viewsSet},
		{"nav vs meta", navSet, metaSet},
		{"views vs meta", viewsSet, metaSet},
	} {
		if d := setDiff(pair.a, pair.b); d != "" {
			t.Errorf("%s disagree: %s", pair.name, d)
		}
	}

	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`function (\w+)\(`).FindAllStringSubmatch(js, -1) {
		seen[m[1]]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("app.js declares function %s %d times (later declaration silently wins)", name, n)
		}
	}
}

func setDiff(a, b map[string]bool) string {
	var only []string
	for k := range a {
		if !b[k] {
			only = append(only, k+" only in first")
		}
	}
	for k := range b {
		if !a[k] {
			only = append(only, k+" only in second")
		}
	}
	sort.Strings(only)
	return strings.Join(only, "; ")
}
