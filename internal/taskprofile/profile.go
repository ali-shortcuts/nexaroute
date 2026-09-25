package taskprofile

// TaskProfile is the deterministic classification result.
// It is derived solely from RequestFeatures, never from raw prompts.
type TaskProfile struct {
	Type        TaskType     `json:"task_type"`
	Complexity  Complexity   `json:"complexity"`
	Confidence  float64      `json:"confidence"` // 0.0-1.0
	ReasonCodes []ReasonCode `json:"reason_codes"`

	EstimatedContextTokens int  `json:"estimated_context_tokens"`
	HasVision              bool `json:"has_vision"`
	HasReasoning           bool `json:"has_reasoning"`
	HasTools               bool `json:"has_tools"`
	ToolCount              int  `json:"tool_count"`
	ImageCount             int  `json:"image_count"`
	MessageCount           int  `json:"message_count"`
}

func (p TaskProfile) Valid() bool {
	if !p.Type.Valid() {
		return false
	}
	if !p.Complexity.Valid() {
		return false
	}
	if p.Confidence < 0 || p.Confidence > 1 {
		return false
	}
	return true
}
