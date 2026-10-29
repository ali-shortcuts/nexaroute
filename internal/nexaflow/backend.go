package nexaflow

import (
	"context"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// Backend is the port interface every node talks to. It is a set of ports,
// NOT implementations of the subsystems: routing, health, compat, eval,
// MCP, video, cost, and HTTP egress all keep their own stores, sessions,
// and credential handling. Adapters translate node calls into port calls
// and port results into node outputs; they never reimplement subsystem
// behaviour. A nil Backend is rejected by nodes that need one.
//
// Secret handling: ResolveSecretRef resolves a secretRef to a value only
// when a resolver is wired; otherwise it fails closed. Resolved values
// must never be returned in node outputs or error messages.
type Backend interface {
	ResolveSelector(ctx context.Context, selector string) (RouteInfo, error)
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	ProviderHealth(ctx context.Context, deploymentID string) (ProviderHealth, error)
	Compat(ctx context.Context, deploymentID string) (CompatInfo, error)
	RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error)
	MCPList(ctx context.Context, serverID string) ([]MCPTool, error)
	MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error)
	VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error)
	VideoStatus(ctx context.Context, jobID string) (VideoJobInfo, error)
	VideoAsset(ctx context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error)
	UsageSnapshot(ctx context.Context) (UsageInfo, error)
	BudgetSnapshot(ctx context.Context, key string) (BudgetInfo, error)
	HTTPDo(ctx context.Context, method, url string, headers map[string]string, body []byte) (HTTPResponse, error)
	ResolveSecretRef(ref string) (string, error)
}

// RouteInfo is the result of validating a node selector. Kind is one of
// "route" (an existing virtual-endpoint public model, resolved by
// route.Resolver), "alias" (a deployment model alias from
// config.ModelConfig.Aliases), or "deployment" (a deployment id).
// The gateway still performs the actual routing; this only proves the
// selector exists before a billable call is made.
type RouteInfo struct {
	Kind          string
	ResolvedModel string
	DeploymentIDs []string
}

type ChatMessage struct {
	Role    string
	Content string
}

type ChatRequest struct {
	Selector    string
	Messages    []ChatMessage
	MaxTokens   int
	Temperature *float64
}

type ChatResponse struct {
	Model            string
	Text             string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ResolvedRoute    string
}

type ProviderHealth struct {
	DeploymentID        string
	Status              string
	Successes           int64
	Failures            int64
	ConsecutiveFailures int
	LastError           string
	CooldownUntil       time.Time
}

type CompatInfo struct {
	DeploymentID    string
	Support         map[string]string // capability name -> "supported"|"unsupported"|"unknown"
	ScorecardStatus string
	Protocol        string
	ContextWindow   int
	MaxOutput       int
}

type MCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type VideoSubmitRequest struct {
	Prompt          string
	Mode            string
	DurationSeconds float64
	AspectRatio     string
}

// VideoJobInfo is the bounded public view of a video job. It deliberately
// carries no filesystem paths: asset access goes through VideoAsset.
type VideoJobInfo struct {
	JobID    string
	State    string
	Terminal bool
	Assets   []VideoAssetInfo
}

type VideoAssetInfo struct {
	ID          string
	Filename    string
	ContentType string
	SHA256      string
	SizeBytes   int64
}

type DeploymentUsage struct {
	Deployment       string
	PromptTokens     int64
	CompletionTokens int64
	Requests         int64
	EstimatedCostUSD float64
	CostBasis        string
	CostCompleteness string
	UnpricedTokens   int64
}

// UsageInfo mirrors usage.Snapshot without repricing anything: CostBasis,
// CostCompleteness, and UnpricedTokens are surfaced verbatim, never hidden
// or recomputed, because a cost figure without its basis is misleading.
type UsageInfo struct {
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	TotalRequests         int64
	TotalEstimatedCostUSD float64
	TotalCostBasis        string
	TotalUnpricedTokens   int64
	ByDeployment          []DeploymentUsage
}

// BudgetInfo is a read-only snapshot. It is NOT an enforcement decision
// and NOT a reservation: see the budget_check node documentation.
type BudgetInfo struct {
	Key       string
	Committed float64
	Reserved  float64
}

type HTTPResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// SecretResolver resolves a secretRef to its value. Implementations must
// fail closed (error) when no resolver is configured.
type SecretResolver func(ref string) (string, error)

// ReplayEvalBackend adapts the existing evaluation plane directly: it
// runs eval.Runner.Run, which builds a ReplayExecutor from the
// caller-supplied outcomes. No network, no model calls, no live scores.
// It implements only the eval port; compose it with other backends.
type ReplayEvalBackend struct {
	Runner *eval.Runner
}

func (b *ReplayEvalBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	runner := b.Runner
	if runner == nil {
		runner = eval.NewRunner()
	}
	res, err := runner.Run(ctx, eval.Request{
		SuiteID: suiteID, DeploymentID: deploymentID, Outcomes: outcomes,
	})
	if err != nil {
		return eval.Result{}, err
	}
	return res, nil
}

// TerminalVideoStates are the video.JobState values after which no further
// transition is expected. They are mirrored here (not imported) so this
// package does not depend on video internals beyond the HTTP surface.
func IsTerminalVideoState(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "expired":
		return true
	}
	return false
}
