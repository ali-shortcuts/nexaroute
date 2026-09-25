package jev

import (
	"bytes"
	"encoding/json"
	"math"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

// Response envelope per the official Jev API docs: {code, message, data}
// with code==0 indicating success. For model-route, data.decision is the
// selected candidate id exactly as sent, with optional confidence,
// probabilities, and guidance.
type envelopeWire struct {
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type decisionDataWire struct {
	Decision      string             `json:"decision"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	// Guidance and any other free text are intentionally not decoded: free
	// text is never copied into reason codes, metrics, logs, or responses.
}

const (
	// maxDecisionIDBytes bounds the returned choice string. Ours are "cN"
	// (a few bytes); anything beyond this is malformed, not a choice.
	maxDecisionIDBytes = 64
	// maxProbEntries caps decoded probability maps as defense-in-depth (the
	// known-choice rule below is the real bound).
	maxProbEntries = 2048
)

// ParseResponse strictly validates a Jev model-route response and maps the
// opaque choice back to its physical deployment ID. allowed maps the
// request's opaque IDs to physical IDs. Confidence is 0 when absent
// (unknown). Any malformed input yields a typed remote error; callers must
// fail open and never partially trust the output.
func ParseResponse(body []byte, allowed map[string]string) (physicalID string, confidence float64, err error) {
	if len(body) == 0 {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision returned an empty body")
	}
	var env envelopeWire
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&env); err != nil {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision returned invalid JSON")
	}
	if env.Code == nil || *env.Code != 0 {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision reported failure")
	}
	if len(env.Data) == 0 {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision omitted data")
	}
	var data decisionDataWire
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision data malformed")
	}
	if data.Decision == "" || len(data.Decision) > maxDecisionIDBytes {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision omitted selection")
	}
	physical, ok := allowed[data.Decision]
	if !ok || physical == "" {
		// Unknown choice: never guess, never pick nearest, never fall back
		// to first. The router order stays authoritative.
		return "", 0, remote.NewError(remote.ClassUnknownCandidate, decision.ReasonExternalUnknownCandidate, "external decision selected an unknown candidate")
	}
	conf := 0.0
	if data.Confidence != nil {
		c := *data.Confidence
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 || c > 1 {
			return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision confidence out of range")
		}
		conf = c
	}
	if len(data.Probabilities) > maxProbEntries {
		return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision probabilities unbounded")
	}
	for choice, p := range data.Probabilities {
		if _, known := allowed[choice]; !known {
			return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision probabilities reference unknown candidate")
		}
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return "", 0, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision probability out of range")
		}
	}
	return physical, conf, nil
}
