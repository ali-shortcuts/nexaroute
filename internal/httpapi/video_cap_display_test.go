package httpapi

import (
	"strings"
	"testing"
)

// #435 item 7 remainder: the video max-cost cap exists end to
// end (request field, ledger admission, create form) and the
// job record carries it back — the Studio cost label must
// SHOW the cap the job runs under, next to the estimate and
// the reconciled actual, so an operator can see the bound
// without opening the raw job JSON.
func TestVideoCapDisplayWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"const cap = Number(j.request && j.request.max_cost_usd)",
		"const capLabel = Number.isFinite(cap) && cap > 0 ? ' · cap ' + fmtUSD(cap) : ''",
		"deltaLabel + capLabel",
		"' · unreconciled' + capLabel",
		// The create form keeps feeding the same field.
		"req.max_cost_usd = Number(maxCost)",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing video-cap display wiring %q", want)
		}
	}
}
