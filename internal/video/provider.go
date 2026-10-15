package video

import (
	"context"
	"net/http"
)

type ProviderJob struct {
	ProviderJobID string `json:"provider_job_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Accepted      bool   `json:"accepted"`
}

type ProviderJobStatus struct {
	ProviderJobID string            `json:"provider_job_id"`
	State         JobState          `json:"state"`
	Progress      float64           `json:"progress"`
	Error         string            `json:"error,omitempty"`
	Usage         map[string]string `json:"usage,omitempty"`
	Outputs       []Asset           `json:"outputs,omitempty"`
	// ActualCostUSD is the provider-reported billed amount in USD, when the
	// provider reports one. It is additive: providers that do not report a
	// cost leave it nil, and the orchestrator falls back to the legacy
	// Usage["actual_cost_usd"] entry before treating the job as unreconciled.
	ActualCostUSD *float64 `json:"actual_cost_usd,omitempty"`
}

type WebhookRegistration struct {
	CallbackURL     string
	SecretReference string
}
type AssetSink interface {
	Put(ctx context.Context, asset Asset, content []byte) (Asset, error)
}

type VideoProvider interface {
	ID() string
	Capabilities(context.Context) (Capabilities, error)
	ValidateRequest(VideoRequest) error
	EstimateCost(VideoRequest) (CostEstimate, error)
	CreateJob(context.Context, VideoRequest) (ProviderJob, error)
	GetJob(context.Context, string) (ProviderJobStatus, error)
	CancelJob(context.Context, string) error
	DownloadOutputs(context.Context, ProviderJobStatus, AssetSink) ([]Asset, error)
	RegisterWebhook(context.Context, WebhookRegistration) error
	VerifyWebhook(*http.Request) error
	ParseWebhook(*http.Request) (ProviderJobStatus, error)
}
