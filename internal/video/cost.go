package video

type CostEstimate struct {
	Provider        string             `json:"provider"`
	Model           string             `json:"model"`
	Currency        string             `json:"currency"`
	EstimatedUSD    float64            `json:"estimated_usd"`
	UpperBoundUSD   float64            `json:"upper_bound_usd"`
	PriceKnown      bool               `json:"price_known"`
	BillableSeconds float64            `json:"billable_seconds"`
	Breakdown       map[string]float64 `json:"breakdown,omitempty"`
}
