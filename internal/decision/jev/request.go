package jev

import (
	"encoding/json"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

// Wire format for POST /api/v1/decisions/model-route, per the official Jev
// API docs (verified 2026-09-25): required task + candidates (at least two,
// each with id and description); optional priorities, constraints, stakes.
// Phase F omits stakes (no trustworthy risk signal from metadata alone).
type candidateWire struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Cost        string `json:"cost,omitempty"`
	Latency     string `json:"latency,omitempty"`
}

type requestWire struct {
	Task        string          `json:"task"`
	Candidates  []candidateWire `json:"candidates"`
	Priorities  []string        `json:"priorities,omitempty"`
	Constraints []string        `json:"constraints,omitempty"`
}

// marshalRequest serializes the Jev body and enforces the documented 32 KiB
// cap BEFORE anything may be sent. Oversize yields a typed error so the
// caller fails open without a network call.
func marshalRequest(w requestWire) ([]byte, error) {
	body, err := json.Marshal(w)
	if err != nil {
		return nil, remote.NewError(remote.ClassRequestTooLarge, decision.ReasonExternalRequestTooLarge, "external decision request could not be encoded")
	}
	if len(body) > remote.MaxRequestBytes {
		return nil, remote.NewError(remote.ClassRequestTooLarge, decision.ReasonExternalRequestTooLarge, "external decision request exceeds size limit")
	}
	return body, nil
}
