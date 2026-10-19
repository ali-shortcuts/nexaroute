package httpapi

import (
	"strings"
	"testing"
)

// #417 item 6: the WebChat composer offers an image attachment
// only when the selected route can serve vision (a non-excluded
// deployment whose provider model declares the vision
// capability), and the attachment rides the current turn as a
// native content part per dialect — never as a silent rewrite
// of history.
func TestWebChatMultimodalWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"function pgVisionCapable()",
		"m.capabilities?.vision === true", // declared capability, never name-inferred
		"pgExclusions()",                  // only non-excluded deployments count
		`id="pg-image"`,
		`accept="image/*"`,
		"state.pgImage",
		"bt('pg_attach')",
		"'pg_attach': 'Attach image'",
		"'pg_attach': 'پیوست تصویر'",
		// Anthropic native image block
		"{ type: 'image', source: { type: 'base64', media_type: img.mediaType, data: img.data } }",
		// OpenAI native image_url data URL
		"{ type: 'image_url', image_url: { url: 'data:' + img.mediaType + ';base64,' + img.data } }",
		"bt('pg_image_attached')", // transcript marker
		"[image attached]",        // export marker
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing multimodal wiring %q", want)
		}
	}
}
