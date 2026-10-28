package nexaflow

import (
	"context"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// GatewayClient selector resolution.
//
// The gateway exposes no dedicated selector-validation endpoint, so
// ResolveSelector validates against the gateway's own model list
// (GET /v1/models, served by the gateway). A selector that appears in
// that list is returned as kind "route"; anything else fails closed
// with selector_not_found and no chat call is ever made for it. This
// does not duplicate routing: the gateway still performs the actual
// routing when Chat is called, and a selector accepted here can still
// be rejected by the gateway at chat time. Virtual-endpoint public
// models, aliases, and deployment ids are all surfaced through the
// model list by the gateway; this client does not reinterpret them.
func (c *GatewayClient) ResolveSelector(ctx context.Context, selector string) (RouteInfo, error) {
	if strings.TrimSpace(selector) == "" {
		return RouteInfo{}, NewNodeError("validation_error", false, "selector must not be empty")
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/models", nil, &decoded); err != nil {
		return RouteInfo{}, err
	}
	for _, m := range decoded.Data {
		if m.ID == selector {
			return RouteInfo{Kind: "route", ResolvedModel: selector}, nil
		}
	}
	return RouteInfo{}, NewNodeError("selector_not_found", false, "selector %q is not listed by the gateway", selector)
}

// ResolveSecretRef resolves a secret reference through the client's
// configured resolver. With no resolver wired it fails closed; it never
// invents a value and never logs the resolved value.
func (c *GatewayClient) ResolveSecretRef(ref string) (string, error) {
	if c.SecretResolver == nil {
		return "", NewNodeError("secret_error", false, "no secret resolver is wired")
	}
	if strings.TrimSpace(ref) == "" {
		return "", NewNodeError("secret_error", false, "secret reference must not be empty")
	}
	value, err := c.SecretResolver(ref)
	if err != nil {
		return "", NewNodeError("secret_error", false, "secret resolution failed")
	}
	return value, nil
}

// CompositeBackend assembles a full Backend from the pieces that own
// each subsystem. Nothing here reimplements a subsystem:
//
//   - Gateway: HTTP chat, MCP, video, generic egress, selector
//     validation, and secret resolution (GatewayClient).
//   - HealthCompat: in-process health manager + compat store.
//   - Cost: in-process usage tracker + budget ledger (read-only).
//   - Eval: eval.Runner replay (ReplayEvalBackend).
//
// Any port whose component is nil fails closed with backend_unavailable
// instead of silently succeeding. A runtime that needs only a subset
// wires only that subset; the engine surfaces the missing port as an
// error on the node that needs it, never as a fake result.
type CompositeBackend struct {
	Gateway      *GatewayClient
	HealthCompat *DirectHealthCompatBackend
	Cost         *DirectCostBackend
	Eval         *ReplayEvalBackend
}

func (b *CompositeBackend) requireGateway() (*GatewayClient, error) {
	if b == nil || b.Gateway == nil {
		return nil, NewNodeError("backend_unavailable", true, "gateway client is not wired")
	}
	return b.Gateway, nil
}

func (b *CompositeBackend) ResolveSelector(ctx context.Context, selector string) (RouteInfo, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return RouteInfo{}, err
	}
	return gw.ResolveSelector(ctx, selector)
}

func (b *CompositeBackend) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return ChatResponse{}, err
	}
	return gw.Chat(ctx, req)
}

func (b *CompositeBackend) ProviderHealth(ctx context.Context, deploymentID string) (ProviderHealth, error) {
	if b == nil || b.HealthCompat == nil {
		return ProviderHealth{}, NewNodeError("backend_unavailable", true, "health backend is not wired")
	}
	return b.HealthCompat.ProviderHealth(ctx, deploymentID)
}

func (b *CompositeBackend) Compat(ctx context.Context, deploymentID string) (CompatInfo, error) {
	if b == nil || b.HealthCompat == nil {
		return CompatInfo{}, NewNodeError("backend_unavailable", true, "compat backend is not wired")
	}
	return b.HealthCompat.Compat(ctx, deploymentID)
}

func (b *CompositeBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	if b == nil || b.Eval == nil {
		return eval.Result{}, NewNodeError("backend_unavailable", true, "eval backend is not wired")
	}
	return b.Eval.RunEvalReplay(ctx, suiteID, deploymentID, outcomes)
}

func (b *CompositeBackend) MCPList(ctx context.Context, serverID string) ([]MCPTool, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return nil, err
	}
	return gw.MCPList(ctx, serverID)
}

func (b *CompositeBackend) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return nil, err
	}
	return gw.MCPCall(ctx, serverID, tool, args)
}

func (b *CompositeBackend) VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return VideoJobInfo{}, err
	}
	return gw.VideoSubmit(ctx, req)
}

func (b *CompositeBackend) VideoStatus(ctx context.Context, jobID string) (VideoJobInfo, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return VideoJobInfo{}, err
	}
	return gw.VideoStatus(ctx, jobID)
}

func (b *CompositeBackend) VideoAsset(ctx context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return VideoAssetInfo{}, nil, err
	}
	return gw.VideoAsset(ctx, jobID, assetID)
}

func (b *CompositeBackend) UsageSnapshot(ctx context.Context) (UsageInfo, error) {
	if b == nil || b.Cost == nil {
		return UsageInfo{}, NewNodeError("backend_unavailable", true, "cost backend is not wired")
	}
	return b.Cost.UsageSnapshot(ctx)
}

func (b *CompositeBackend) BudgetSnapshot(ctx context.Context, key string) (BudgetInfo, error) {
	if b == nil || b.Cost == nil {
		return BudgetInfo{}, NewNodeError("backend_unavailable", true, "budget backend is not wired")
	}
	return b.Cost.BudgetSnapshot(ctx, key)
}

func (b *CompositeBackend) HTTPDo(ctx context.Context, method, url string, headers map[string]string, body []byte) (HTTPResponse, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return HTTPResponse{}, err
	}
	return gw.HTTPDo(ctx, method, url, headers, body)
}

func (b *CompositeBackend) ResolveSecretRef(ref string) (string, error) {
	gw, err := b.requireGateway()
	if err != nil {
		return "", err
	}
	return gw.ResolveSecretRef(ref)
}

var _ Backend = (*CompositeBackend)(nil)
