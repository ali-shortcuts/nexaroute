package nexaflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/egress"
)

// GatewayClient talks to a running NexaRoute gateway over HTTP. It is the
// only HTTP client code in this package for the chat, MCP, and video
// subsystems: it does not reimplement routing, MCP sessions, or video
// persistence — it calls the gateway endpoints that own them. Tests drive
// it against httptest servers; production wiring (base URL, auth) is the
// runtime's job and is not claimed here.
//
// AuthTokenRef, when set, is resolved through SecretResolver on every
// request; the resolved token is sent as a Bearer token and never logged
// or returned. When no resolver is wired, requests are sent without auth
// and the gateway decides (typically 401).
type GatewayClient struct {
	BaseURL        string
	HTTPClient     *http.Client
	AuthTokenRef   string
	SecretResolver SecretResolver
	// Egress policy for HTTPDo. Defaults (when zero) allow loopback HTTP
	// and require HTTPS elsewhere, matching egress.Policy semantics.
	Egress egress.Policy
	// MaxResponseBytes bounds every response body read. Zero = 16 MiB.
	MaxResponseBytes int64
}

func (c *GatewayClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func (c *GatewayClient) maxBytes() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return 16 << 20
}

func (c *GatewayClient) authToken() (string, error) {
	if c.AuthTokenRef == "" || c.SecretResolver == nil {
		return "", nil
	}
	return c.SecretResolver(c.AuthTokenRef)
}

func (c *GatewayClient) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) (int, error) {
	var body io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return 0, NewNodeError("validation_error", false, "request encoding failed")
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return 0, NewNodeError("backend_error", false, "gateway request failed: %s", RedactError(err))
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token, err := c.authToken(); err != nil {
		return 0, NewNodeError("secret_error", false, "auth token resolution failed")
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, NewNodeError("backend_error", true, "gateway request failed: %s", RedactError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()+1))
	if err != nil {
		return resp.StatusCode, NewNodeError("backend_error", true, "gateway response read failed")
	}
	if int64(len(raw)) > c.maxBytes() {
		return resp.StatusCode, NewNodeError("output_too_large", false, "gateway response exceeds bound")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, NewNodeError("backend_error", resp.StatusCode == 429 || resp.StatusCode >= 500, "gateway returned status %d", resp.StatusCode)
	}
	if respBody != nil {
		if err := json.Unmarshal(raw, respBody); err != nil {
			return resp.StatusCode, NewNodeError("backend_error", false, "gateway response decode failed")
		}
	}
	return resp.StatusCode, nil
}

// Chat calls POST /v1/chat/completions with stream forced false. The
// model field carries the validated selector; the gateway performs the
// actual routing. The prompt is never logged or stored by this package.
func (c *GatewayClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	msgs := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	payload := map[string]any{
		"model": req.Selector, "messages": msgs, "stream": false,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/chat/completions", payload, &decoded); err != nil {
		return ChatResponse{}, err
	}
	text := ""
	if len(decoded.Choices) > 0 {
		text = decoded.Choices[0].Message.Content
	}
	return ChatResponse{
		Model: decoded.Model, Text: text,
		PromptTokens:     decoded.Usage.PromptTokens,
		CompletionTokens: decoded.Usage.CompletionTokens,
		TotalTokens:      decoded.Usage.TotalTokens,
		ResolvedRoute:    req.Selector,
	}, nil
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func (c *GatewayClient) rpc(ctx context.Context, method string, params any) (map[string]any, error) {
	var decoded struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/mcp", jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params}, &decoded); err != nil {
		return nil, err
	}
	if decoded.Error != nil {
		return nil, NewNodeError("backend_error", false, "mcp gateway error: %s", RedactError(fmt.Errorf("%s", decoded.Error.Message)))
	}
	if decoded.Result == nil {
		return map[string]any{}, nil
	}
	return decoded.Result, nil
}

// MCPList calls tools/list on the MCP gateway. It does not open an MCP
// session itself; session and allowlist enforcement belong to the gateway,
// and the node additionally pre-checks its own allowlist on call.
func (c *GatewayClient) MCPList(ctx context.Context, serverID string) ([]MCPTool, error) {
	_ = serverID // server selection is a gateway concern; see docs.
	result, err := c.rpc(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	rawTools, _ := result["tools"].([]any)
	tools := make([]MCPTool, 0, len(rawTools))
	for _, raw := range rawTools {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := item["name"].(string)
		desc, _ := item["description"].(string)
		schema, _ := item["inputSchema"].(map[string]any)
		tools = append(tools, MCPTool{Name: name, Description: desc, InputSchema: schema})
	}
	return tools, nil
}

// MCPCall calls tools/call on the MCP gateway.
func (c *GatewayClient) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	_ = serverID
	return c.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
}

// VideoSubmit posts to /v1/video/jobs. Submitting through the HTTP API
// (not the queue store) is deliberate: queue.JobStore.Create alone does
// not run a job; the API/orchestrator path does.
func (c *GatewayClient) VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	payload := map[string]any{
		"prompt": req.Prompt, "mode": req.Mode,
		"duration_seconds": req.DurationSeconds, "aspect_ratio": req.AspectRatio,
	}
	var decoded struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/v1/video/jobs", payload, &decoded); err != nil {
		return VideoJobInfo{}, err
	}
	if decoded.JobID == "" {
		return VideoJobInfo{}, NewNodeError("backend_error", false, "video gateway returned no job id")
	}
	return VideoJobInfo{JobID: decoded.JobID, State: decoded.State, Terminal: IsTerminalVideoState(decoded.State)}, nil
}

func parseVideoJob(raw map[string]any) VideoJobInfo {
	jobID, _ := raw["job_id"].(string)
	state, _ := raw["state"].(string)
	info := VideoJobInfo{JobID: jobID, State: state, Terminal: IsTerminalVideoState(state)}
	if assets, ok := raw["output_assets"].([]any); ok {
		for _, a := range assets {
			item, ok := a.(map[string]any)
			if !ok {
				continue
			}
			id, _ := item["id"].(string)
			name, _ := item["filename"].(string)
			ct, _ := item["content_type"].(string)
			sha, _ := item["sha256"].(string)
			size, _ := item["size_bytes"].(float64)
			info.Assets = append(info.Assets, VideoAssetInfo{ID: id, Filename: name, ContentType: ct, SHA256: sha, SizeBytes: int64(size)})
		}
	}
	return info
}

// VideoStatus polls GET /v1/video/jobs/{id}. Polling cadence and
// completion waiting are the runtime's job; this is one poll.
func (c *GatewayClient) VideoStatus(ctx context.Context, jobID string) (VideoJobInfo, error) {
	var decoded map[string]any
	if _, err := c.doJSON(ctx, http.MethodGet, "/v1/video/jobs/"+jobID, nil, &decoded); err != nil {
		return VideoJobInfo{}, err
	}
	return parseVideoJob(decoded), nil
}

// VideoAsset downloads GET /v1/video/jobs/{jobID}/assets/{assetID}.
// Bytes are returned only under the client's response bound; the caller
// (the output node) additionally enforces its own MaxOutputBytes. No
// filesystem path is ever returned.
func (c *GatewayClient) VideoAsset(ctx context.Context, jobID, assetID string) (VideoAssetInfo, []byte, error) {
	status, err := c.VideoStatus(ctx, jobID)
	if err != nil {
		return VideoAssetInfo{}, nil, err
	}
	var meta VideoAssetInfo
	for _, a := range status.Assets {
		if a.ID == assetID {
			meta = a
			break
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/video/jobs/"+jobID+"/assets/"+assetID, nil)
	if err != nil {
		return VideoAssetInfo{}, nil, NewNodeError("backend_error", false, "video asset request failed")
	}
	if token, err := c.authToken(); err != nil {
		return VideoAssetInfo{}, nil, NewNodeError("secret_error", false, "auth token resolution failed")
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return VideoAssetInfo{}, nil, NewNodeError("backend_error", true, "video asset download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return VideoAssetInfo{}, nil, NewNodeError("backend_error", resp.StatusCode >= 500, "video asset gateway returned status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()+1))
	if err != nil {
		return VideoAssetInfo{}, nil, NewNodeError("backend_error", true, "video asset read failed")
	}
	if int64(len(raw)) > c.maxBytes() {
		return VideoAssetInfo{}, nil, NewNodeError("output_too_large", false, "video asset exceeds bound")
	}
	if meta.ID == "" {
		meta = VideoAssetInfo{ID: assetID, SizeBytes: int64(len(raw))}
	}
	return meta, raw, nil
}

// HTTPDo performs one egress-guarded HTTP request: https except loopback,
// cross-origin redirects refused, response bounded. It reuses
// egress.Policy.NewTransport and egress.RefuseCrossOriginRedirects; it
// does not invent its own SSRF policy. The caller (the http node) forbids
// Authorization headers from workflow input; credentials, when needed,
// come only from a secretRef resolved by the backend.
func (c *GatewayClient) HTTPDo(ctx context.Context, method, url string, headers map[string]string, body []byte) (HTTPResponse, error) {
	policy := c.Egress
	policy.AllowLoopback = true
	transport, err := policy.NewTransport(url, "")
	if err != nil {
		return HTTPResponse{}, NewNodeError("validation_error", false, "egress refused request: %s", RedactError(err))
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: egress.RefuseCrossOriginRedirects(5, "nexaflow http node"),
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return HTTPResponse{}, NewNodeError("validation_error", false, "http request build failed")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return HTTPResponse{}, NewNodeError("backend_error", true, "http request failed: %s", RedactError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes()+1))
	if err != nil {
		return HTTPResponse{}, NewNodeError("backend_error", true, "http response read failed")
	}
	if int64(len(raw)) > c.maxBytes() {
		return HTTPResponse{}, NewNodeError("output_too_large", false, "http response exceeds bound")
	}
	outHeaders := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			outHeaders[k] = vs[0]
		}
	}
	return HTTPResponse{Status: resp.StatusCode, Headers: outHeaders, Body: raw}, nil
}
