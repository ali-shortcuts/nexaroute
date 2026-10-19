package video

import "fmt"

// ConsistencyMechanism is one consistency mechanism that is
// ACTUALLY in play for a job — a seed, reference imagery, a
// frame anchor, a continuity input — derived only from the
// job's recorded request (#420 item 3).
type ConsistencyMechanism struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// ConsistencyMechanisms projects the consistency mechanisms
// in play for a job from its recorded request, in a stable
// order. The list is empty when the brief carries no
// mechanism at all — callers must say so rather than imply
// consistency the pipeline never pursued (the issue's rule:
// no consistency claims without a mechanism).
func ConsistencyMechanisms(j VideoJob) []ConsistencyMechanism {
	r := j.Request
	out := []ConsistencyMechanism{}
	if r.Seed != nil {
		out = append(out, ConsistencyMechanism{Kind: "seed", Detail: fmt.Sprintf("seed %d", *r.Seed)})
	}
	if len(r.ReferenceAssets) > 0 {
		out = append(out, ConsistencyMechanism{Kind: "reference_images", Detail: plural(len(r.ReferenceAssets), "reference image")})
	}
	if len(r.CharacterReferences) > 0 {
		out = append(out, ConsistencyMechanism{Kind: "character_references", Detail: plural(len(r.CharacterReferences), "character reference")})
	}
	if len(r.LocationReferences) > 0 {
		out = append(out, ConsistencyMechanism{Kind: "location_references", Detail: plural(len(r.LocationReferences), "location reference")})
	}
	if len(r.StyleReferences) > 0 {
		out = append(out, ConsistencyMechanism{Kind: "style_references", Detail: plural(len(r.StyleReferences), "style reference")})
	}
	if r.FirstFrameAsset != nil {
		out = append(out, ConsistencyMechanism{Kind: "first_frame", Detail: "first-frame anchor"})
	}
	if r.LastFrameAsset != nil {
		out = append(out, ConsistencyMechanism{Kind: "last_frame", Detail: "last-frame anchor"})
	}
	shotRefs, shotConstraints := 0, 0
	for _, sh := range r.ShotPlan {
		shotRefs += len(sh.RequiredReferences)
		shotConstraints += len(sh.ContinuityConstraints)
	}
	if shotRefs > 0 {
		out = append(out, ConsistencyMechanism{Kind: "shot_references", Detail: plural(shotRefs, "per-shot reference")})
	}
	if shotConstraints > 0 {
		out = append(out, ConsistencyMechanism{Kind: "continuity_constraints", Detail: plural(shotConstraints, "continuity constraint")})
	}
	if r.ContinuityContext != nil {
		out = append(out, ConsistencyMechanism{Kind: "continuity", Detail: "continuity from a previous job"})
	}
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
