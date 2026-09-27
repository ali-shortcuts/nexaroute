package jev

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

// Jev envelope per docs: {code, message, data} code 0 success
type JevEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Data for model-route: decision is selected candidate id
type JevModelRouteData struct {
	Decision      string             `json:"decision"`                // selected candidate id (opaque c0,c1)
	Confidence    *float64           `json:"confidence,omitempty"`    // optional
	Probabilities map[string]float64 `json:"probabilities,omitempty"` // optional
	Guidance      string             `json:"guidance,omitempty"`      // free text, ignored
	// Other fields may exist but we ignore
}

// ParsedResponse is validated, bounded result
type ParsedResponse struct {
	SelectedID string
	Confidence float64 // 0 if absent, finite [0,1]
	RawData    JevModelRouteData
}

// ParseAndValidate validates envelope and extracts decision
func ParseAndValidate(body []byte, allowedIDs map[string]struct{}) (*ParsedResponse, error) {
	if len(body) == 0 {
		return nil, remote.NewError(remote.ErrInvalidResponse, "empty body")
	}
	// Must be valid JSON
	var env JevEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, remote.NewError(remote.ErrInvalidResponse, "invalid json")
	}
	// code 0 success
	if env.Code != 0 {
		return nil, remote.NewError(remote.ErrInvalidResponse, fmt.Sprintf("non-zero code %d", env.Code))
	}
	if len(env.Data) == 0 {
		return nil, remote.NewError(remote.ErrInvalidResponse, "missing data")
	}
	var data JevModelRouteData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, remote.NewError(remote.ErrInvalidResponse, "invalid data json")
	}
	if data.Decision == "" {
		return nil, remote.NewError(remote.ErrInvalidResponse, "missing decision")
	}
	// Validate selected ID exists in allowed opaque mapping
	if _, ok := allowedIDs[data.Decision]; !ok {
		return nil, &remote.Error{Kind: remote.ErrUnknownCandidate, Message: fmt.Sprintf("unknown choice %s", data.Decision)}
	}
	// Confidence validation
	conf := 0.0
	if data.Confidence != nil {
		c := *data.Confidence
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return nil, remote.NewError(remote.ErrInvalidResponse, "nan/inf confidence")
		}
		if c < 0 || c > 1 {
			return nil, remote.NewError(remote.ErrInvalidResponse, fmt.Sprintf("confidence out of bounds %f", c))
		}
		conf = c
	}
	// Probabilities validation if present
	if data.Probabilities != nil {
		if len(data.Probabilities) > remote.MaxCandidates {
			return nil, remote.NewError(remote.ErrInvalidResponse, "too many probabilities")
		}
		for k, v := range data.Probabilities {
			if _, ok := allowedIDs[k]; !ok {
				// Allow extra? Spec says never use unknown choices, so reject if unknown in probabilities?
				// For safety, we reject if probabilities contain unknown candidate
				return nil, remote.NewError(remote.ErrInvalidResponse, fmt.Sprintf("unknown candidate in probabilities %s", k))
			}
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
				return nil, remote.NewError(remote.ErrInvalidResponse, "invalid probability")
			}
		}
	}
	// Guidance is ignored, not copied into reason codes

	return &ParsedResponse{
		SelectedID: data.Decision,
		Confidence: conf,
		RawData:    data,
	}, nil
}
