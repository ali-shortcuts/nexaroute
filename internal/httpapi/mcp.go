package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// MCP gateway (M6-1): brokers registered upstream Model Context
// Protocol servers behind POST /mcp. See specs/037 for the design:
// namespaced tools (<server>__<tool>), per-tenant allowlists,
// lazy per-server sessions, response bounds, and audit events that
// never carry argument content.

type mcpServerState struct {
	mu        sync.Mutex
	url       string
	sessionID string
	ready     bool
	listTools []json.RawMessage
	listAt    time.Time
}

type mcpBroker struct {
	mu      sync.Mutex
	servers map[string]*mcpServerState
	client  *http.Client
}

func newMCPBroker() *mcpBroker {
	return &mcpBroker{servers: map[string]*mcpServerState{}, client: &http.Client{}}
}

func (b *mcpBroker) stateFor(srv config.MCPServerConfig) *mcpServerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.servers[srv.ID]
	if st == nil {
		st = &mcpServerState{}
		b.servers[srv.ID] = st
	}
	if st.url != srv.URL {
		// Server re-pointed by a config swap: drop the old session.
		st.url = srv.URL
		st.sessionID = ""
		st.ready = false
		st.listTools = nil
	}
	return st
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// broker returns the server's MCP broker, lazily created so
// partially-constructed servers (unit tests) cannot nil-panic.
func (s *Server) broker() *mcpBroker {
	if s.mcp == nil {
		s.mcp = newMCPBroker()
	}
	return s.mcp
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result json.RawMessage, rerr *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rerr}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = writeJSONBody(w, resp)
}

func writeJSONBody(w http.ResponseWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func rpcErr(code int, msg string) *rpcError { return &rpcError{Code: code, Message: msg} }

// mcpGateway serves POST /mcp.
func (s *Server) mcpGateway(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if !cfg.MCP.Enabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, presented, ok := s.mcpClientAllowed(w, r, cfg)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeRPC(w, nil, nil, rpcErr(-32700, "request body unreadable or too large"))
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		writeRPC(w, nil, nil, rpcErr(-32600, "batch requests are not supported"))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil || req.Method == "" {
		writeRPC(w, nil, nil, rpcErr(-32700, "invalid JSON-RPC request"))
		return
	}
	// Notifications (no id) are accepted without a response body.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-03-26"
		}
		result, _ := json.Marshal(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "nexaroute", "version": "1"},
		})
		writeRPC(w, req.ID, result, nil)
	case "ping":
		writeRPC(w, req.ID, json.RawMessage(`{}`), nil)
	case "tools/list":
		result, rerr := s.mcpToolsList(r, cfg, identity)
		writeRPC(w, req.ID, result, rerr)
	case "tools/call":
		result, rerr := s.mcpToolsCall(r, cfg, identity, presented, req)
		writeRPC(w, req.ID, result, rerr)
	default:
		writeRPC(w, req.ID, nil, rpcErr(-32601, "method not found"))
	}
}

// mcpClientAllowed resolves the calling client identity for the
// MCP endpoint. When client auth is disabled the endpoint follows
// the gateway's local-trust posture (anonymous identity).
func (s *Server) mcpClientAllowed(w http.ResponseWriter, r *http.Request, cfg config.Config) (clientIdentity, string, bool) {
	if !cfg.ClientAuth.Enabled {
		return clientIdentity{ID: "anonymous"}, "", true
	}
	keyText := extractClientKey(r)
	if keyText == "" {
		errorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		return clientIdentity{}, "", false
	}
	identity, _, ok := s.findClientIdentity(keyText)
	if !ok {
		if clientKeyMatches(cfg.ClientAuth.Keys, keyText) {
			return clientIdentity{ID: "legacy"}, keyText, true
		}
		errorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		return clientIdentity{}, "", false
	}
	return identity, keyText, true
}

func mcpTenantAllowed(srv config.MCPServerConfig, tenant string) bool {
	if len(srv.AllowedTenants) == 0 {
		return true
	}
	for _, t := range srv.AllowedTenants {
		if t == tenant {
			return true
		}
	}
	return false
}

func (s *Server) mcpToolsList(r *http.Request, cfg config.Config, identity clientIdentity) (json.RawMessage, *rpcError) {
	tools := []json.RawMessage{}
	for _, srv := range cfg.MCP.Servers {
		if !srv.Enabled || !mcpTenantAllowed(srv, identity.TenantID) {
			continue
		}
		serverTools, err := s.broker().listTools(r.Context(), cfg, srv)
		if err != nil {
			s.bus.Add(events.Event{RequestID: r.Header.Get("x-request-id"), Kind: "mcp_server_unreachable", Deployment: srv.ID, Message: "tools/list failed; server skipped"})
			continue
		}
		tools = append(tools, serverTools...)
	}
	result, _ := json.Marshal(map[string]any{"tools": tools})
	return result, nil
}

func (s *Server) mcpToolsCall(r *http.Request, cfg config.Config, identity clientIdentity, presented string, req rpcRequest) (json.RawMessage, *rpcError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		return nil, rpcErr(-32602, "tools/call requires a tool name")
	}
	serverID, tool, found := strings.Cut(params.Name, "__")
	if !found || serverID == "" || tool == "" {
		return nil, rpcErr(-32602, "tool name must be namespaced as <server>__<tool>")
	}
	var srv *config.MCPServerConfig
	for i := range cfg.MCP.Servers {
		if cfg.MCP.Servers[i].ID == serverID {
			srv = &cfg.MCP.Servers[i]
			break
		}
	}
	fail := func(code int, msg, result string) (json.RawMessage, *rpcError) {
		s.mcpAudit(r, serverID, tool, identity, result, len(params.Arguments))
		return nil, rpcErr(code, msg)
	}
	if srv == nil || !srv.Enabled {
		return fail(-32602, "unknown tool", "unknown_server")
	}
	if !mcpTenantAllowed(*srv, identity.TenantID) {
		return fail(-32003, "tool not available to this tenant", "tenant_forbidden")
	}
	if len(srv.Tools) > 0 {
		allowed := false
		for _, t := range srv.Tools {
			if t == tool {
				allowed = true
				break
			}
		}
		if !allowed {
			return fail(-32003, "tool not in the server allowlist", "tool_forbidden")
		}
	}
	// Per-key rate limit: mcp.rpm wins; otherwise the client-auth
	// global applies; 0 means unlimited.
	rpm := cfg.MCP.RPM
	if rpm == 0 {
		rpm = cfg.ClientAuth.RPM
	}
	if presented != "" && !s.clientRPMAllow(presented, rpm) {
		return fail(-32004, "MCP rate limit exceeded", "rate_limited")
	}
	upstreamParams, _ := json.Marshal(map[string]any{"name": tool, "arguments": json.RawMessage(orEmptyObject(params.Arguments))})
	result, rerr := s.broker().call(r.Context(), cfg, *srv, "tools/call", upstreamParams)
	if rerr != nil {
		s.mcpAudit(r, serverID, tool, identity, "error", len(params.Arguments))
		return nil, rerr
	}
	s.mcpAudit(r, serverID, tool, identity, "ok", len(params.Arguments))
	return result, nil
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// mcpAudit records one tool call. Argument CONTENT is never
// recorded — only its size.
func (s *Server) mcpAudit(r *http.Request, server, tool string, identity clientIdentity, result string, argsBytes int) {
	s.bus.Add(events.Event{
		RequestID:  r.Header.Get("x-request-id"),
		Kind:       "mcp_tool_call",
		Deployment: server,
		Message:    fmt.Sprintf("tool=%s tenant=%s identity=%s result=%s args_bytes=%d", tool, identity.TenantID, identity.ID, result, argsBytes),
	})
	s.mcpStatsMu.Lock()
	if s.mcpStats == nil {
		s.mcpStats = map[string]int64{}
	}
	s.mcpStats[server+"|"+result]++
	s.mcpStatsMu.Unlock()
}

// listTools returns the server's tools with namespaced names,
// using a 30 s per-server cache.
func (b *mcpBroker) listTools(ctx context.Context, cfg config.Config, srv config.MCPServerConfig) ([]json.RawMessage, error) {
	st := b.stateFor(srv)
	st.mu.Lock()
	if st.listTools != nil && time.Since(st.listAt) < 30*time.Second {
		cached := st.listTools
		st.mu.Unlock()
		return cached, nil
	}
	st.mu.Unlock()

	result, rerr := b.call(ctx, cfg, srv, "tools/list", json.RawMessage(`{}`))
	if rerr != nil {
		return nil, fmt.Errorf("tools/list: %s", rerr.Message)
	}
	var parsed struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("tools/list: invalid upstream result")
	}
	allow := map[string]bool{}
	for _, t := range srv.Tools {
		allow[t] = true
	}
	out := make([]json.RawMessage, 0, len(parsed.Tools))
	for _, rawTool := range parsed.Tools {
		var toolObj map[string]any
		if err := json.Unmarshal(rawTool, &toolObj); err != nil {
			continue
		}
		name, _ := toolObj["name"].(string)
		if name == "" || (len(allow) > 0 && !allow[name]) {
			continue
		}
		toolObj["name"] = srv.ID + "__" + name
		rewritten, err := json.Marshal(toolObj)
		if err != nil {
			continue
		}
		out = append(out, rewritten)
	}
	st.mu.Lock()
	st.listTools = out
	st.listAt = time.Now()
	st.mu.Unlock()
	return out, nil
}

// call performs one upstream JSON-RPC call, ensuring the session
// first and retrying once after a session reset on HTTP 404.
func (b *mcpBroker) call(ctx context.Context, cfg config.Config, srv config.MCPServerConfig, method string, params json.RawMessage) (json.RawMessage, *rpcError) {
	result, rerr, sessionLost := b.callOnce(ctx, cfg, srv, method, params)
	if sessionLost {
		st := b.stateFor(srv)
		st.mu.Lock()
		st.ready = false
		st.sessionID = ""
		st.mu.Unlock()
		result, rerr, _ = b.callOnce(ctx, cfg, srv, method, params)
	}
	return result, rerr
}

func (b *mcpBroker) callOnce(ctx context.Context, cfg config.Config, srv config.MCPServerConfig, method string, params json.RawMessage) (json.RawMessage, *rpcError, bool) {
	st := b.stateFor(srv)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := b.ensureSessionLocked(ctx, cfg, srv, st); err != nil {
		return nil, rpcErr(-32002, "upstream MCP server unavailable"), false
	}
	resp, err := b.postLocked(ctx, cfg, srv, st, method, params, newRPCID())
	if err != nil {
		return nil, rpcErr(-32002, "upstream MCP server unavailable"), false
	}
	if resp.sessionLost {
		return nil, rpcErr(-32002, "upstream MCP session lost"), true
	}
	if resp.rpcErr != nil {
		return nil, resp.rpcErr, false
	}
	return resp.result, nil, false
}

type upstreamReply struct {
	result      json.RawMessage
	rpcErr      *rpcError
	sessionLost bool
}

// ensureSessionLocked performs the initialize handshake once per
// server (per URL). Called with st.mu held: session setup is
// serialized per server, calls included — MCP servers are
// stateful per session, and NexaRoute brokers one session per
// server by design.
func (b *mcpBroker) ensureSessionLocked(ctx context.Context, cfg config.Config, srv config.MCPServerConfig, st *mcpServerState) error {
	if st.ready {
		return nil
	}
	initParams, _ := json.Marshal(map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nexaroute", "version": "1"},
	})
	if _, err := b.postLocked(ctx, cfg, srv, st, "initialize", initParams, newRPCID()); err != nil {
		return err
	}
	// notifications/initialized completes the handshake; its
	// response (if any) is irrelevant.
	_, _, _, _ = b.postRawLocked(ctx, cfg, srv, st, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	st.ready = true
	return nil
}

func (b *mcpBroker) postLocked(ctx context.Context, cfg config.Config, srv config.MCPServerConfig, st *mcpServerState, method string, params json.RawMessage, id string) (*upstreamReply, error) {
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if len(params) > 0 {
		payload["params"] = params
	}
	body, status, contentType, err := b.postRawLocked(ctx, cfg, srv, st, payload)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound && st.sessionID != "" {
		return &upstreamReply{sessionLost: true}, nil
	}
	if status == http.StatusAccepted || status == http.StatusNoContent {
		return &upstreamReply{result: json.RawMessage(`{}`)}, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", status)
	}
	msg, err := parseRPCBody(body, contentType, id)
	if err != nil {
		return nil, err
	}
	if msg.Error != nil {
		return &upstreamReply{rpcErr: msg.Error}, nil
	}
	if len(msg.Result) == 0 {
		return &upstreamReply{result: json.RawMessage(`{}`)}, nil
	}
	return &upstreamReply{result: msg.Result}, nil
}

// postRawLocked POSTs one payload and returns the bounded body.
// The session header is captured from every response.
func (b *mcpBroker) postRawLocked(ctx context.Context, cfg config.Config, srv config.MCPServerConfig, st *mcpServerState, payload map[string]any) ([]byte, int, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.MCP.CallTimeoutSeconds)*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, srv.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if st.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", st.sessionID)
	}
	if srv.AuthEnv != "" {
		if v := os.Getenv(srv.AuthEnv); v != "" {
			httpReq.Header.Set("Authorization", v)
		}
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		st.sessionID = sid
	}
	limit := int64(cfg.MCP.MaxResponseBytes) + (64 << 10)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, "", err
	}
	if int64(len(body)) > int64(cfg.MCP.MaxResponseBytes) && resp.StatusCode == http.StatusOK {
		return nil, resp.StatusCode, "", fmt.Errorf("upstream response exceeds max_response_bytes")
	}
	return body, resp.StatusCode, resp.Header.Get("Content-Type"), nil
}

// parseRPCBody extracts the JSON-RPC message for `id` from a JSON
// body or an SSE stream of data: lines.
func parseRPCBody(body []byte, contentType, id string) (*rpcResponse, error) {
	if strings.Contains(contentType, "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 0, 64<<10), len(body)+1)
		var found *rpcResponse
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var msg rpcResponse
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				continue
			}
			if len(msg.Result) > 0 || msg.Error != nil {
				found = &msg
			}
		}
		if found == nil {
			return nil, fmt.Errorf("no JSON-RPC message in SSE stream")
		}
		return found, nil
	}
	var msg rpcResponse
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("invalid upstream JSON-RPC response")
	}
	return &msg, nil
}

func newRPCID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
