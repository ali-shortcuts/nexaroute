package nexaflow

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// RuntimeBackend composes the partial production backends into one Backend
// for the workflow engine. It owns only translation, never subsystem
// behaviour:
//
//   - GatewayClient for chat, video, HTTP egress, and MCP over HTTP.
//   - DirectHealthCompatBackend / DirectCostBackend / ReplayEvalBackend
//     for the in-process ports.
//   - ResolveSelectorFunc, supplied by httpapi wiring from route.Resolver,
//     because selector validation needs the gateway's route table.
//   - Secrets, supplied by the operator, because no general secretRef
//     resolver exists elsewhere in the gateway.
//
// Any unwired port fails closed with backend_unavailable instead of
// inventing a result.
type RuntimeBackend struct {
	Gateway *GatewayClient
	Health  *DirectHealthCompatBackend
	Cost    *DirectCostBackend
	Eval    *ReplayEvalBackend

	// ResolveSelectorFunc validates a node selector. Nil fails closed.
	ResolveSelectorFunc func(ctx context.Context, selector string) (RouteInfo, error)
	// Secrets resolves a secretRef. Nil fails closed.
	Secrets SecretResolver
}

var _ Backend = (*RuntimeBackend)(nil)

func backendUnavailable(what string) error {
	return NewNodeError("backend_unavailable", false, "%s is not wired", what)
}

func (b *RuntimeBackend) ResolveSelector(ctx context.Context, selector string) (RouteInfo, error) {
	if b.ResolveSelectorFunc == nil {
		return RouteInfo{}, backendUnavailable("selector resolution")
	}
	return b.ResolveSelectorFunc(ctx, selector)
}

func (b *RuntimeBackend) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if b.Gateway == nil {
		return ChatResponse{}, backendUnavailable("gateway client")
	}
	return b.Gateway.Chat(ctx, req)
}

func (b *RuntimeBackend) ProviderHealth(ctx context.Context, deploymentID string) (ProviderHealth, error) {
	if b.Health == nil {
		return ProviderHealth{}, backendUnavailable("health backend")
	}
	return b.Health.ProviderHealth(ctx, deploymentID)
}

func (b *RuntimeBackend) Compat(ctx context.Context, deploymentID string) (CompatInfo, error) {
	if b.Health == nil {
		return CompatInfo{}, backendUnavailable("health backend")
	}
	return b.Health.Compat(ctx, deploymentID)
}

func (b *RuntimeBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	if b.Eval == nil {
		return eval.Result{}, backendUnavailable("eval backend")
	}
	return b.Eval.RunEvalReplay(ctx, suiteID, deploymentID, outcomes)
}

// MCPList translates the gateway's namespaced tool names back to the
// bare names the node contract uses: the gateway lists every server's
// tools as "<server>__<tool>", while the mcp.call node checks its
// allowlist and the gateway listing against the bare tool name for the
// requested server. Tools of other servers are dropped, never exposed.
func (b *RuntimeBackend) MCPList(ctx context.Context, serverID string) ([]MCPTool, error) {
	if b.Gateway == nil {
		return nil, backendUnavailable("gateway client")
	}
	tools, err := b.Gateway.MCPList(ctx, serverID)
	if err != nil {
		return nil, err
	}
	prefix := serverID + "__"
	out := make([]MCPTool, 0, len(tools))
	for _, t := range tools {
		if strings.HasPrefix(t.Name, prefix) {
			t.Name = strings.TrimPrefix(t.Name, prefix)
			out = append(out, t)
		}
	}
	return out, nil
}

// MCPCall namespaces the tool name the way the gateway broker requires:
// tools/call rejects bare names with "tool name must be namespaced".
func (b *RuntimeBackend) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	if b.Gateway == nil {
		return nil, backendUnavailable("gateway client")
	}
	return b.Gateway.MCPCall(ctx, serverID, serverID+"__"+tool, args)
}

func (b *RuntimeBackend) VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	if b.Gateway == nil {
		return VideoJobInfo{}, backendUnavailable("gateway client")
	}
	return b.Gateway.VideoSubmit(ctx, req)
}

func (b *RuntimeBackend) VideoStatus(ctx context.Context, jobID string) (VideoJobInfo, error) {
	if b.Gateway == nil {
		return VideoJobInfo{}, backendUnavailable("gateway client")
	}
	return b.Gateway.VideoStatus(ctx, jobID)
}

func (b *RuntimeBackend) VideoAsset(ctx context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error) {
	if b.Gateway == nil {
		return VideoAssetInfo{}, nil, backendUnavailable("gateway client")
	}
	return b.Gateway.VideoAsset(ctx, jobID, assetID)
}

func (b *RuntimeBackend) UsageSnapshot(ctx context.Context) (UsageInfo, error) {
	if b.Cost == nil {
		return UsageInfo{}, backendUnavailable("cost backend")
	}
	return b.Cost.UsageSnapshot(ctx)
}

func (b *RuntimeBackend) BudgetSnapshot(ctx context.Context, key string) (BudgetInfo, error) {
	if b.Cost == nil {
		return BudgetInfo{}, backendUnavailable("cost backend")
	}
	return b.Cost.BudgetSnapshot(ctx, key)
}

func (b *RuntimeBackend) HTTPDo(ctx context.Context, method, url string, headers map[string]string, body []byte) (HTTPResponse, error) {
	if b.Gateway == nil {
		return HTTPResponse{}, backendUnavailable("gateway client")
	}
	return b.Gateway.HTTPDo(ctx, method, url, headers, body)
}

func (b *RuntimeBackend) ResolveSecretRef(ref string) (string, error) {
	if b.Secrets == nil {
		return "", backendUnavailable("secret resolver")
	}
	return b.Secrets(ref)
}

// EnvSecretResolver resolves a secretRef as an environment variable name,
// mirroring how provider credentials use APIKeyEnv. It fails closed when
// the variable is unset or empty, and it never logs the value. This is
// the operator-supplied resolver for webhook secret_ref and HTTP
// auth_secret_ref values; it is not a vault or keyring integration.
func EnvSecretResolver(ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("nexaflow: empty secret reference")
	}
	if v := os.Getenv(ref); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("nexaflow: secret reference %q is not set", ref)
}
