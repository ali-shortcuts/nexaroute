package video

import "testing"

// #420 item 3: the consistency projection names only
// mechanisms actually present in the recorded request — and
// is empty when there are none, so the UI can honestly say
// "no consistency mechanism in play".
func TestConsistencyMechanisms(t *testing.T) {
	if got := ConsistencyMechanisms(VideoJob{}); len(got) != 0 {
		t.Fatalf("bare job mechanisms = %v, want none", got)
	}
	seed := int64(42)
	asset := Asset{ID: "a", Kind: "image", URI: "https://example.test/a.png"}
	job := VideoJob{Request: VideoRequest{
		Seed:                &seed,
		ReferenceAssets:     []Asset{asset, asset},
		CharacterReferences: []Asset{asset},
		StyleReferences:     []Asset{asset},
		FirstFrameAsset:     &asset,
		LastFrameAsset:      &asset,
		ContinuityContext:   &ContinuityContext{PreviousFinalFrame: &asset},
		ShotPlan: []Shot{
			{ShotID: "s1", RequiredReferences: []Asset{asset}, ContinuityConstraints: []string{"same jacket"}},
			{ShotID: "s2", RequiredReferences: []Asset{asset}},
		},
	}}
	got := ConsistencyMechanisms(job)
	wantKinds := []string{"seed", "reference_images", "character_references", "style_references", "first_frame", "last_frame", "shot_references", "continuity_constraints", "continuity"}
	if len(got) != len(wantKinds) {
		t.Fatalf("mechanisms = %v", got)
	}
	for i, k := range wantKinds {
		if got[i].Kind != k {
			t.Fatalf("mechanism %d = %q, want %q (all: %v)", i, got[i].Kind, k, got)
		}
	}
	if got[0].Detail != "seed 42" {
		t.Fatalf("seed detail = %q", got[0].Detail)
	}
	if got[1].Detail != "2 reference images" {
		t.Fatalf("reference detail = %q", got[1].Detail)
	}
	if got[6].Detail != "2 per-shot references" {
		t.Fatalf("shot reference detail = %q", got[6].Detail)
	}
}
