package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// NexaFlow Studio HTTP surface.
//
// Authorization is enforced twice, never by the UI alone: the admin
// middleware maps every /admin/api/nexaflow/* request to
// routing.read (GET) or routing.write (mutations) in
// adminPermissionForRequest, and Registry.Execute re-checks each
// node's own permission against the run starter's identity. A viewer
// can read workflows; an operator can edit and run them; a node that
// needs providers.read still refuses an operator who lacks it.
//
// What this surface deliberately does NOT do:
//   - Video nodes: the bundled server backend reports backend_unavailable
//     for video ports. Video jobs are operated through the Video Studio
//     bearer token, which the workflow service does not hold, and
//     inventing a token path would be a credential bypass. Video nodes
//     still validate and appear in the palette; they fail safely at run
//     time with that message until a video execution port is wired.
//   - LLM/chat nodes: executed through the gateway's own data plane via
//     GatewayClient (same-origin self-call). In a keyless gateway this
//     works; in a key-gated gateway the call fails closed with the
//     gateway's 401, because the workflow service never mints or
//     borrows a client credential. Operators who need LLM runs behind
//     client-key gating must call the data plane with their own key
//     outside workflows until a credential path is explicitly designed.
//   - Secrets: workflow JSON, run input, and node config never carry
//     secret values. secretRef fields carry references only, resolved
//     from process environment ("env:NAME") by the backend, fail-closed
//     otherwise. Node outputs are secret-redacted by Registry.Execute
//     and the API renders bounded previews, never raw stored outputs.

// nexaflowService returns the lazily constructed workflow service.
// Construction failure (e.g. a corrupt store file) fails closed: every
// NexaFlow request then returns 503 instead of serving empty state.
func (s *Server) nexaflowService() (*nexaflow.Service, error) {
	s.nexaflowMu.Lock()
	defer s.nexaflowMu.Unlock()
	if s.nexaflowSvc != nil || s.nexaflowSvcErr != nil {
		return s.nexaflowSvc, s.nexaflowSvcErr
	}
	storePath := ""
	if strings.TrimSpace(s.configPath) != "" {
		storePath = filepath.Join(filepath.Dir(s.configPath), "nexaflow-workflows.json")
	}
	svc, err := nexaflow.NewService(nexaflow.DefaultRegistry(), s.nexaflowBackend(), nexaflow.NewStore(storePath))
	if err != nil {
		s.nexaflowSvcErr = err
		return nil, err
	}
	s.nexaflowSvc = svc
	return svc, nil
}

// SetNexaflowSelfBaseURL sets the base URL the workflow backend uses
// for gateway self-calls (chat, MCP, HTTP egress is policy-guarded
// separately). Call it at startup, before the first NexaFlow request;
// after the service is constructed the value is fixed.
func (s *Server) SetNexaflowSelfBaseURL(url string) {
	s.nexaflowMu.Lock()
	defer s.nexaflowMu.Unlock()
	s.nexaflowSelfBaseURL = url
}

// nexaflowBackend composes the server's in-process ports. It must be
// called with nexaflowMu held (from nexaflowService) or before serving.
func (s *Server) nexaflowBackend() nexaflow.Backend {
	gw := &nexaflow.GatewayClient{BaseURL: s.nexaflowSelfBaseURL}
	return &nexaflowServerBackend{
		health: &nexaflow.DirectHealthCompatBackend{Health: s.hm, CompatStore: s.capStore},
		cost:   &nexaflow.DirectCostBackend{Tracker: s.usage, Ledger: s.budgets, Prices: map[string]usage.Price{}},
		eval:   &nexaflow.ReplayEvalBackend{Runner: eval.NewRunner()},
		gw:     gw,
		server: s,
	}
}

type nexaflowServerBackend struct {
	health *nexaflow.DirectHealthCompatBackend
	cost   *nexaflow.DirectCostBackend
	eval   *nexaflow.ReplayEvalBackend
	gw     *nexaflow.GatewayClient
	server *Server
}

func backendUnavailable(port string) error {
	return nexaflow.NewNodeError("backend_unavailable", true, "nexaflow port %s is not wired in this gateway", port)
}

func (b *nexaflowServerBackend) ResolveSelector(ctx context.Context, selector string) (nexaflow.RouteInfo, error) {
	if b.server.routeResolver != nil {
		if rr, ok := b.server.routeResolver.Resolve(selector); ok {
			_ = rr
			return nexaflow.RouteInfo{Kind: "route", ResolvedModel: selector}, nil
		}
	}
	// Deployment id or model alias from the effective provider config.
	cfg := b.server.currentConfig()
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			if m.ID == selector || m.Model == selector {
				return nexaflow.RouteInfo{Kind: "deployment", ResolvedModel: m.ID, DeploymentIDs: []string{m.ID}}, nil
			}
			for _, a := range m.Aliases {
				if a == selector {
					return nexaflow.RouteInfo{Kind: "alias", ResolvedModel: m.ID, DeploymentIDs: []string{m.ID}}, nil
				}
			}
		}
	}
	return nexaflow.RouteInfo{}, nexaflow.NewNodeError("selector_not_found", false, "selector %q did not resolve", selector)
}

func (b *nexaflowServerBackend) Chat(ctx context.Context, req nexaflow.ChatRequest) (nexaflow.ChatResponse, error) {
	if b.gw == nil || strings.TrimSpace(b.gw.BaseURL) == "" {
		return nexaflow.ChatResponse{}, backendUnavailable("chat (no gateway base URL configured)")
	}
	return b.gw.Chat(ctx, req)
}

func (b *nexaflowServerBackend) ProviderHealth(ctx context.Context, deploymentID string) (nexaflow.ProviderHealth, error) {
	return b.health.ProviderHealth(ctx, deploymentID)
}

func (b *nexaflowServerBackend) Compat(ctx context.Context, deploymentID string) (nexaflow.CompatInfo, error) {
	return b.health.Compat(ctx, deploymentID)
}

func (b *nexaflowServerBackend) RunEvalReplay(ctx context.Context, suiteID, deploymentID string, outcomes []eval.Outcome) (eval.Result, error) {
	return b.eval.RunEvalReplay(ctx, suiteID, deploymentID, outcomes)
}

func (b *nexaflowServerBackend) MCPList(ctx context.Context, serverID string) ([]nexaflow.MCPTool, error) {
	if b.gw == nil || strings.TrimSpace(b.gw.BaseURL) == "" {
		return nil, backendUnavailable("mcp (no gateway base URL configured)")
	}
	return b.gw.MCPList(ctx, serverID)
}

func (b *nexaflowServerBackend) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	if b.gw == nil || strings.TrimSpace(b.gw.BaseURL) == "" {
		return nil, backendUnavailable("mcp (no gateway base URL configured)")
	}
	return b.gw.MCPCall(ctx, serverID, tool, args)
}

func (b *nexaflowServerBackend) VideoSubmit(ctx context.Context, req nexaflow.VideoSubmitRequest) (nexaflow.VideoJobInfo, error) {
	return nexaflow.VideoJobInfo{}, backendUnavailable("video (video jobs are operated through the Video Studio token)")
}

func (b *nexaflowServerBackend) VideoStatus(ctx context.Context, jobID string) (nexaflow.VideoJobInfo, error) {
	return nexaflow.VideoJobInfo{}, backendUnavailable("video (video jobs are operated through the Video Studio token)")
}

func (b *nexaflowServerBackend) VideoAsset(ctx context.Context, jobID, assetID string) (nexaflow.VideoAssetInfo, []byte, error) {
	return nexaflow.VideoAssetInfo{}, nil, backendUnavailable("video (video jobs are operated through the Video Studio token)")
}

func (b *nexaflowServerBackend) UsageSnapshot(ctx context.Context) (nexaflow.UsageInfo, error) {
	return b.cost.UsageSnapshot(ctx)
}

func (b *nexaflowServerBackend) BudgetSnapshot(ctx context.Context, key string) (nexaflow.BudgetInfo, error) {
	return b.cost.BudgetSnapshot(ctx, key)
}

func (b *nexaflowServerBackend) HTTPDo(ctx context.Context, method, url string, headers map[string]string, body []byte) (nexaflow.HTTPResponse, error) {
	if b.gw == nil {
		return nexaflow.HTTPResponse{}, backendUnavailable("http")
	}
	return b.gw.HTTPDo(ctx, method, url, headers, body)
}

// ResolveSecretRef resolves only "env:NAME" references from the
// process environment. Any other reference fails closed: workflow
// JSON never becomes a credential carrier.
func (b *nexaflowServerBackend) ResolveSecretRef(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	if !ok || strings.TrimSpace(name) == "" {
		return "", nexaflow.NewNodeError("secret_error", false, "secret references must use env:NAME")
	}
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", nexaflow.NewNodeError("secret_error", false, "secret reference %q is not configured", ref)
}

// ---- DTOs: the API never renders raw stored outputs or configs with
// secret values. Configs are rendered as stored (they hold references,
// never values, by construction), outputs as bounded previews. ----

type nexaflowFieldDTO struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

type nexaflowNodeDTO struct {
	ID                   string             `json:"id"`
	Version              string             `json:"version"`
	Category             string             `json:"category"`
	Title                string             `json:"title"`
	Description          string             `json:"description"`
	InputSchema          []nexaflowFieldDTO `json:"input_schema"`
	OutputSchema         []nexaflowFieldDTO `json:"output_schema"`
	ConfigSchema         []nexaflowFieldDTO `json:"config_schema"`
	Permission           string             `json:"permission"`
	SideEffect           string             `json:"side_effect"`
	RequiresPolicy       bool               `json:"requires_policy"`
	DefaultTimeoutMillis int64              `json:"default_timeout_ms"`
	MaxTimeoutMillis     int64              `json:"max_timeout_ms"`
	MaxInputBytes        int                `json:"max_input_bytes"`
	MaxOutputBytes       int                `json:"max_output_bytes"`
	MaxAttempts          int                `json:"max_attempts"`
}

func nexaflowFields(fields []nexaflow.Field) []nexaflowFieldDTO {
	out := make([]nexaflowFieldDTO, 0, len(fields))
	for _, f := range fields {
		out = append(out, nexaflowFieldDTO{Name: f.Name, Type: f.Type, Required: f.Required, Description: f.Description})
	}
	return out
}

type nexaflowWorkflowDTO struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	Enabled         bool               `json:"enabled"`
	Draft           nexaflow.Graph     `json:"draft"`
	Versions        []nexaflow.Version `json:"versions"`
	PublishedNumber int                `json:"published_version"`
	CreatedBy       string             `json:"created_by"`
	UpdatedBy       string             `json:"updated_by"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	DraftUpdatedAt  time.Time          `json:"draft_updated_at"`
}

// nexaflowGraphView normalizes nil slices to [] so API consumers (notably
// the browser editor, which pushes onto draft.nodes) never see JSON null
// for an empty graph.
func nexaflowGraphView(g nexaflow.Graph) nexaflow.Graph {
	if g.Nodes == nil {
		g.Nodes = []nexaflow.GraphNode{}
	}
	if g.Edges == nil {
		g.Edges = []nexaflow.GraphEdge{}
	}
	if g.Inputs == nil {
		g.Inputs = []string{}
	}
	return g
}

func nexaflowWorkflowView(w *nexaflow.Workflow) nexaflowWorkflowDTO {
	published := 0
	if len(w.Versions) > 0 {
		published = w.Versions[len(w.Versions)-1].Number
	}
	versions := w.Versions
	if versions == nil {
		versions = []nexaflow.Version{}
	}
	return nexaflowWorkflowDTO{
		ID: w.ID, Name: w.Name, Enabled: w.Enabled, Draft: nexaflowGraphView(w.Draft),
		Versions: versions, PublishedNumber: published,
		CreatedBy: w.CreatedBy, UpdatedBy: w.UpdatedBy,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, DraftUpdatedAt: w.DraftUpdatedAt,
	}
}

type nexaflowNodeRunDTO struct {
	NodeID          string     `json:"node_id"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	Attempts        int        `json:"attempts"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationMs      int64      `json:"duration_ms"`
	OutputPreview   string     `json:"output_preview,omitempty"`
	OutputTruncated bool       `json:"output_truncated"`
	Error           string     `json:"error,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
}

type nexaflowRunDTO struct {
	ID             string               `json:"id"`
	WorkflowID     string               `json:"workflow_id"`
	WorkflowName   string               `json:"workflow_name"`
	VersionNumber  int                  `json:"version_number"`
	Snapshot       nexaflow.Graph       `json:"snapshot"`
	Status         string               `json:"status"`
	StartedBy      string               `json:"started_by"`
	PolicyApproved bool                 `json:"policy_approved"`
	InputPreview   string               `json:"input_preview,omitempty"`
	InputTruncated bool                 `json:"input_truncated"`
	Nodes          []nexaflowNodeRunDTO `json:"nodes"`
	PendingNodeID  string               `json:"pending_node_id,omitempty"`
	Error          string               `json:"error,omitempty"`
	StartedAt      time.Time            `json:"started_at"`
	FinishedAt     *time.Time           `json:"finished_at,omitempty"`
}

func nexaflowRunView(r *nexaflow.Run) nexaflowRunDTO {
	dto := nexaflowRunDTO{
		ID: r.ID, WorkflowID: r.WorkflowID, WorkflowName: r.WorkflowName,
		VersionNumber: r.VersionNumber, Snapshot: nexaflowGraphView(r.Snapshot), Status: r.Status,
		StartedBy: r.StartedBy, PolicyApproved: r.PolicyApproved,
		PendingNodeID: r.PendingNodeID, Error: r.Error,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
	if r.Input != nil {
		raw := nexaflow.RedactJSON(r.Input)
		if len(raw) > 4096 {
			dto.InputPreview = raw[:4096]
			dto.InputTruncated = true
		} else {
			dto.InputPreview = raw
		}
	}
	// Nodes in snapshot order so the run view matches the canvas.
	for _, n := range r.Snapshot.Nodes {
		nr := r.Nodes[n.ID]
		if nr == nil {
			continue
		}
		preview, truncated := nexaflow.PreviewOutput(nr.Output)
		dto.Nodes = append(dto.Nodes, nexaflowNodeRunDTO{
			NodeID: nr.NodeID, Type: nr.Type, Status: nr.Status,
			Attempts: nr.Attempts, StartedAt: nr.StartedAt, FinishedAt: nr.FinishedAt,
			DurationMs: nr.DurationMs, OutputPreview: preview, OutputTruncated: truncated,
			Error: nr.Error, ErrorCode: nr.ErrorCode,
		})
	}
	return dto
}

func nexaflowErr(w http.ResponseWriter, err error) {
	var ve *nexaflow.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, 422, map[string]any{"error": "workflow validation failed", "issues": ve.Issues})
		return
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		errorJSON(w, 404, msg)
	case strings.Contains(msg, "disabled"), strings.Contains(msg, "not awaiting"), strings.Contains(msg, "cannot be cancelled"), strings.Contains(msg, "already has"):
		errorJSON(w, 409, msg)
	default:
		errorJSON(w, 400, msg)
	}
}

// adminNexaflowNodes serves the canonical registry: the single source
// of node schemas the editor consumes. No Run functions, no secrets.
func (s *Server) adminNexaflowNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	svc, err := s.nexaflowService()
	if err != nil {
		errorJSON(w, 503, "nexaflow service unavailable: "+err.Error())
		return
	}
	nodes := make([]nexaflowNodeDTO, 0)
	for _, spec := range svc.Registry().All() {
		nodes = append(nodes, nexaflowNodeDTO{
			ID: spec.ID, Version: spec.Version, Category: spec.Category,
			Title: spec.Title, Description: spec.Description,
			InputSchema:  nexaflowFields(spec.InputSchema),
			OutputSchema: nexaflowFields(spec.OutputSchema),
			ConfigSchema: nexaflowFields(spec.ConfigSchema),
			Permission:   string(spec.Permission), SideEffect: string(spec.SideEffect),
			RequiresPolicy:       spec.RequiresOperatorPolicy,
			DefaultTimeoutMillis: spec.DefaultTimeout.Milliseconds(),
			MaxTimeoutMillis:     spec.MaxTimeout.Milliseconds(),
			MaxInputBytes:        spec.MaxInputBytes,
			MaxOutputBytes:       spec.MaxOutputBytes,
			MaxAttempts:          spec.Retry.MaxAttempts,
		})
	}
	writeJSON(w, 200, map[string]any{"version": nexaflow.SupportedVersion, "nodes": nodes})
}

func nexaflowPrincipal(r *http.Request) authz.Identity {
	return adminIdentityFromRequest(r)
}

// adminNexaflowWorkflows handles the collection: GET list, POST create.
func (s *Server) adminNexaflowWorkflows(w http.ResponseWriter, r *http.Request) {
	svc, err := s.nexaflowService()
	if err != nil {
		errorJSON(w, 503, "nexaflow service unavailable: "+err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		workflows, err := svc.Store().ListWorkflows()
		if err != nil {
			errorJSON(w, 503, err.Error())
			return
		}
		out := make([]nexaflowWorkflowDTO, 0, len(workflows))
		for _, wf := range workflows {
			out = append(out, nexaflowWorkflowView(wf))
		}
		writeJSON(w, 200, map[string]any{"workflows": out})
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		wf, err := svc.CreateWorkflow(in.Name, nexaflowPrincipal(r))
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 201, nexaflowWorkflowView(wf))
	default:
		errorJSON(w, 405, "method not allowed")
	}
}

// adminNexaflowWorkflowByID handles one workflow and its sub-actions:
// GET, PUT (draft), DELETE, POST rename/duplicate/enabled/publish,
// GET validate, GET versions, GET/POST runs.
func (s *Server) adminNexaflowWorkflowByID(w http.ResponseWriter, r *http.Request) {
	svc, err := s.nexaflowService()
	if err != nil {
		errorJSON(w, 503, "nexaflow service unavailable: "+err.Error())
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/nexaflow/workflows/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		errorJSON(w, 404, "workflow not found")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	principal := nexaflowPrincipal(r)
	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			wf, err := svc.Store().GetWorkflow(id)
			if err != nil {
				errorJSON(w, 503, err.Error())
				return
			}
			if wf == nil {
				errorJSON(w, 404, "workflow not found")
				return
			}
			writeJSON(w, 200, nexaflowWorkflowView(wf))
		case http.MethodPut:
			var in struct {
				Draft nexaflow.Graph `json:"draft"`
			}
			if _, err := readJSON(r, &in); err != nil {
				errorJSON(w, 400, "invalid JSON: "+err.Error())
				return
			}
			wf, err := svc.UpdateDraft(id, in.Draft, principal)
			if err != nil {
				nexaflowErr(w, err)
				return
			}
			writeJSON(w, 200, nexaflowWorkflowView(wf))
		case http.MethodDelete:
			if err := svc.DeleteWorkflow(id); err != nil {
				nexaflowErr(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"deleted": true})
		default:
			errorJSON(w, 405, "method not allowed")
		}
	case "rename":
		if r.Method != http.MethodPost {
			errorJSON(w, 405, "method not allowed")
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		wf, err := svc.RenameWorkflow(id, in.Name, principal)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 200, nexaflowWorkflowView(wf))
	case "duplicate":
		if r.Method != http.MethodPost {
			errorJSON(w, 405, "method not allowed")
			return
		}
		wf, err := svc.DuplicateWorkflow(id, principal)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 201, nexaflowWorkflowView(wf))
	case "enabled":
		if r.Method != http.MethodPost {
			errorJSON(w, 405, "method not allowed")
			return
		}
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		wf, err := svc.SetEnabled(id, in.Enabled, principal)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 200, nexaflowWorkflowView(wf))
	case "publish":
		if r.Method != http.MethodPost {
			errorJSON(w, 405, "method not allowed")
			return
		}
		wf, err := svc.PublishWorkflow(id, principal)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 200, nexaflowWorkflowView(wf))
	case "validate":
		if r.Method != http.MethodGet {
			errorJSON(w, 405, "method not allowed")
			return
		}
		issues, err := svc.ValidateDraft(id)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"issues": issues, "valid": !nexaflow.HasErrors(issues)})
	case "versions":
		if r.Method != http.MethodGet {
			errorJSON(w, 405, "method not allowed")
			return
		}
		wf, err := svc.Store().GetWorkflow(id)
		if err != nil {
			errorJSON(w, 503, err.Error())
			return
		}
		if wf == nil {
			errorJSON(w, 404, "workflow not found")
			return
		}
		writeJSON(w, 200, map[string]any{"versions": wf.Versions})
	case "runs":
		switch r.Method {
		case http.MethodGet:
			limit := 50
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 {
					limit = n
				}
			}
			runs, err := svc.ListRuns(id, limit)
			if err != nil {
				errorJSON(w, 503, err.Error())
				return
			}
			out := make([]nexaflowRunDTO, 0, len(runs))
			for _, run := range runs {
				out = append(out, nexaflowRunView(run))
			}
			writeJSON(w, 200, map[string]any{"runs": out})
		case http.MethodPost:
			var in struct {
				VersionNumber  int            `json:"version_number"`
				Input          map[string]any `json:"input"`
				PolicyApproved bool           `json:"policy_approved"`
			}
			if _, err := readJSON(r, &in); err != nil {
				errorJSON(w, 400, "invalid JSON: "+err.Error())
				return
			}
			run, err := svc.StartRun(id, in.VersionNumber, in.Input, in.PolicyApproved, principal)
			if err != nil {
				nexaflowErr(w, err)
				return
			}
			writeJSON(w, 201, nexaflowRunView(run))
		default:
			errorJSON(w, 405, "method not allowed")
		}
	default:
		errorJSON(w, 404, "unknown nexaflow action")
	}
}

// adminNexaflowRunByID handles one run: GET, POST cancel, POST approve.
func (s *Server) adminNexaflowRunByID(w http.ResponseWriter, r *http.Request) {
	svc, err := s.nexaflowService()
	if err != nil {
		errorJSON(w, 503, "nexaflow service unavailable: "+err.Error())
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/nexaflow/runs/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		errorJSON(w, 404, "run not found")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "":
		if r.Method != http.MethodGet {
			errorJSON(w, 405, "method not allowed")
			return
		}
		run, err := svc.GetRun(id)
		if err != nil {
			errorJSON(w, 503, err.Error())
			return
		}
		if run == nil {
			errorJSON(w, 404, "run not found")
			return
		}
		writeJSON(w, 200, nexaflowRunView(run))
	case "cancel":
		if r.Method != http.MethodPost {
			errorJSON(w, 405, "method not allowed")
			return
		}
		run, err := svc.CancelRun(id)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 200, nexaflowRunView(run))
	case "approve":
		if r.Method != http.MethodPost {
			errorJSON(w, 405, "method not allowed")
			return
		}
		var in struct {
			Decision string `json:"decision"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		run, err := svc.ApproveRun(id, in.Decision, nexaflowPrincipal(r).Subject)
		if err != nil {
			nexaflowErr(w, err)
			return
		}
		writeJSON(w, 200, nexaflowRunView(run))
	default:
		errorJSON(w, 404, "unknown nexaflow action")
	}
}
