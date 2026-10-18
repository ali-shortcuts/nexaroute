package httpapi

import (
	"strings"
	"testing"
)

// Phase 10 (#410) Live Activity UI contract. These tests execute against the
// real embedded app.js so the live view cannot silently lose the wiring that
// connects it to real backend events. The behavioral assertions (reconnect,
// duplicate/out-of-order handling, empty and authorization states) are pinned
// here; browser acceptance is separate.

func TestActivityLiveViewIsWiredToBackend(t *testing.T) {
	js := readAppJS(t)
	view := extractFunctionLine(t, js, "activityView")
	for _, want := range []string{
		`data-activity-tab="live"`,
		"activityLiveList()",
		"activityConnectionBadge()",
		"activityDetail(",
		`id="activity-provider"`,
		`id="activity-state"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("activityView missing live-activity wiring %q", want)
		}
	}
	for _, want := range []string{
		"/admin/api/activity/requests?limit=100",
		"function ensureActivityPolling()",
		"function renderActivityOnly()",
		"function activityWaterfall(",
		"function activityDetail(",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing live-activity implementation %q", want)
		}
	}
}

func TestActivityReconnectDuplicateAndOrderContract(t *testing.T) {
	js := readAppJS(t)
	start := strings.Index(js, "async function startSSE()")
	if start < 0 {
		t.Fatal("startSSE not found")
	}
	// startSSE is a single line; take through the next newline.
	end := strings.Index(js[start:], "\n")
	sse := js[start : start+end]
	for _, want := range []string{
		// cursor-based resume so a reconnect does not lose events
		"&since=${encodeURIComponent(state.eventSeq)}",
		// duplicate events are ignored by sequence
		"if (seq && seq <= state.eventSeq) continue;",
		// an epoch change or a gap forces a bounded resync
		"epoch !== state.eventEpoch",
		"seq > state.eventSeq + 1",
		"await resyncSnapshot();",
		// automatic reconnection backoff
		"state.sseReconnect = setTimeout(() => startSSE(), 1000)",
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("startSSE missing reconnect/dedupe/order handling %q", want)
		}
	}
}

func TestActivityEmptyStaleAndAuthorizationStates(t *testing.T) {
	js := readAppJS(t)
	for _, want := range []string{
		"No requests observed",
		"Stale — not live",
		"Authentication required to view live activity.",
		"You do not have permission to view live activity.",
		"Live activity unavailable",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing explicit activity state text %q", want)
		}
	}
}

func TestActivityDeepLinkContract(t *testing.T) {
	js := readAppJS(t)
	for _, want := range []string{
		"function applyDeepLink()",
		`#activity`,
		"window.addEventListener('hashchange'",
		"location.hash = 'activity/' + encodeURIComponent(id)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing deep-link contract %q", want)
		}
	}
}

func TestActivityViewStaleNeverLive(t *testing.T) {
	js := readAppJS(t)
	live := extractFunctionLine(t, js, "activityLiveList")
	if !strings.Contains(live, "lc.state === 'stale' ? 'Stale — not live'") {
		t.Errorf("live list must label stale requests explicitly: %s", live)
	}
	if !strings.Contains(live, "activityBucket(lc.state) === 'stale' ? 'warn'") {
		t.Errorf("stale requests must use the non-live warning styling")
	}
}
