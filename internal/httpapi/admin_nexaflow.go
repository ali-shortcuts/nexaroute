package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// NexaFlow admin API.
//
// Routes (registered as explicit patterns in Server.Handler, one
// handler per route, so the OpenAPI contract test matches each route
// to exactly one spec path):
//   GET    /admin/api/nexaflow/nodes
//   GET    /admin/api/nexaflow/templates
//   GET    /admin/api/nexaflow/definitions?tenant=
//   POST   /admin/api/nexaflow/definitions
//   GET    /admin/api/nexaflow/definitions/{id}
//   POST   /admin/api/nexaflow/definitions/{id}/validate
//   POST   /admin/api/nexaflow/definitions/{id}/publish
//   GET    /admin/api/nexaflow/definitions/{id}/versions
//   GET    /admin/api/nexaflow/definitions/{id}/versions/{rev}
//   POST   /admin/api/nexaflow/definitions/{id}/runs
//   GET    /admin/api/nexaflow/definitions/{id}/runs
//   GET    /admin/api/nexaflow/runs/{id}
//   POST   /admin/api/nexaflow/runs/{id}/approve
//   POST   /admin/api/nexaflow/runs/{id}/resume
//   POST   /admin/api/nexaflow/runs/{id}/cancel
//   GET    /admin/api/nexaflow/audit
//   POST   /admin/api/nexaflow/scheduler/tick
//   POST   /nexaflow/webhooks/{definitionID}/{triggerNodeID} (HMAC, not admin auth)
//
// Permissions come from the central middleware matrix (workflow.read for
// GET, workflow.write for state changes). Handlers additionally fail with
// 503 when NexaFlow is not attached. Tenant scoping is by definition scope
// plus list filters: Identity carries no tenant, so this API does not
// block cross-tenant execution by itself.

func (s *Server) nexaflowReady(w http.ResponseWriter) bool {
	if s.nexaflowEngine == nil || s.nexaflowStore == nil {
		errorJSON(w, http.StatusServiceUnavailable, "nexaflow is not configured on this gateway")
		return false
	}
	return true
}

func nexaflowErrorStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if nexaflow.NotFound(err) {
		return http.StatusNotFound
	}
	var ne *nexaflow.NodeError
	if errors.As(err, &ne) {
		switch ne.Code {
		case "permission_denied":
			return http.StatusForbidden
		case "invalid_state", "run_conflict":
			return http.StatusConflict
		case "validation_error", "input_resolution", "input_too_large", "output_too_large":
			return http.StatusBadRequest
		case "replay_rejected":
			return http.StatusConflict
		}
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func nexaflowError(w http.ResponseWriter, err error) {
	errorJSON(w, nexaflowErrorStatus(err), err.Error())
}

func nexaflowLimit(r *http.Request, def int) int {
	limit := def
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	return limit
}

type nexaflowFieldView struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

type nexaflowNodeView struct {
	ID                     string              `json:"id"`
	Version                string              `json:"version"`
	Category               string              `json:"category"`
	Title                  string              `json:"title"`
	Description            string              `json:"description"`
	InputSchema            []nexaflowFieldView `json:"input_schema"`
	OutputSchema           []nexaflowFieldView `json:"output_schema"`
	ConfigSchema           []nexaflowFieldView `json:"config_schema"`
	Permission             string              `json:"permission"`
	SideEffect             string              `json:"side_effect"`
	MaxAttempts            int                 `json:"max_attempts"`
	RetryableCodes         []string            `json:"retryable_codes,omitempty"`
	DefaultTimeoutMs       int64               `json:"default_timeout_ms"`
	MaxTimeoutMs           int64               `json:"max_timeout_ms"`
	MaxInputBytes          int                 `json:"max_input_bytes"`
	MaxOutputBytes         int                 `json:"max_output_bytes"`
	RequiresOperatorPolicy bool                `json:"requires_operator_policy"`
}

func nexaflowFields(in []nexaflow.Field) []nexaflowFieldView {
	out := make([]nexaflowFieldView, 0, len(in))
	for _, f := range in {
		out = append(out, nexaflowFieldView{Name: f.Name, Type: f.Type, Required: f.Required, Description: f.Description})
	}
	return out
}

func (s *Server) adminNexaflowNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	specs := nexaflow.DefaultRegistry().All()
	views := make([]nexaflowNodeView, 0, len(specs))
	for _, spec := range specs {
		views = append(views, nexaflowNodeView{
			ID: spec.ID, Version: spec.Version, Category: spec.Category,
			Title: spec.Title, Description: spec.Description,
			InputSchema:  nexaflowFields(spec.InputSchema),
			OutputSchema: nexaflowFields(spec.OutputSchema),
			ConfigSchema: nexaflowFields(spec.ConfigSchema),
			Permission:   string(spec.Permission), SideEffect: string(spec.SideEffect),
			MaxAttempts: spec.Retry.MaxAttempts, RetryableCodes: spec.Retry.RetryableCodes,
			DefaultTimeoutMs: spec.DefaultTimeout.Milliseconds(), MaxTimeoutMs: spec.MaxTimeout.Milliseconds(),
			MaxInputBytes: spec.MaxInputBytes, MaxOutputBytes: spec.MaxOutputBytes,
			RequiresOperatorPolicy: spec.RequiresOperatorPolicy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}

type nexaflowTemplateNodeView struct {
	NodeID  string         `json:"node_id"`
	Version string         `json:"version"`
	Config  map[string]any `json:"config,omitempty"`
}

type nexaflowTemplateView struct {
	ID             string                     `json:"id"`
	Version        string                     `json:"version"`
	Title          string                     `json:"title"`
	Description    string                     `json:"description"`
	Nodes          []nexaflowTemplateNodeView `json:"nodes"`
	Edges          [][2]string                `json:"edges"`
	Placeholders   []string                   `json:"placeholders"`
	RequiresPolicy bool                       `json:"requires_policy"`
	Published      bool                       `json:"published"`
}

func (s *Server) adminNexaflowTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	// Templates are inert reference shapes: they are not runnable
	// definitions and this endpoint never instantiates or executes one.
	templates := nexaflow.DefaultTemplates()
	views := make([]nexaflowTemplateView, 0, len(templates))
	for _, t := range templates {
		nodes := make([]nexaflowTemplateNodeView, 0, len(t.Nodes))
		for _, n := range t.Nodes {
			nodes = append(nodes, nexaflowTemplateNodeView{NodeID: n.NodeID, Version: n.Version, Config: n.Config})
		}
		views = append(views, nexaflowTemplateView{
			ID: t.ID, Version: t.Version, Title: t.Title, Description: t.Description,
			Nodes: nodes, Edges: t.Edges, Placeholders: t.Placeholders,
			RequiresPolicy: t.RequiresPolicy, Published: t.Published,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"templates": views,
		"inert":     true,
		"note":      "templates are reference shapes, not runnable definitions",
	})
}

func (s *Server) adminNexaflowDefinitions(w http.ResponseWriter, r *http.Request) {
	// Method first, readiness second: an undocumented method must
	// answer 405 even when NexaFlow is unattached (whose 503 would
	// otherwise read as "implemented" to contract probes).
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		drafts, err := s.nexaflowStore.ListDrafts(r.Context(), r.URL.Query().Get("tenant"))
		if err != nil {
			nexaflowError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definitions": drafts})
	case http.MethodPost:
		var def nexaflow.Definition
		if _, err := readJSON(r, &def); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := nexaflow.ValidateDefinitionExtended(&def, nexaflow.DefaultRegistry()); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.nexaflowStore.SaveDraft(r.Context(), &def); err != nil {
			nexaflowError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definition": &def})
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func nexaflowPathID(r *http.Request, name string) string {
	return r.PathValue(name)
}

func (s *Server) adminNexaflowDefinition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	def, err := s.nexaflowStore.GetDraft(r.Context(), nexaflowPathID(r, "id"))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"definition": def})
}

func (s *Server) adminNexaflowDefinitionValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	def, err := s.nexaflowStore.GetDraft(r.Context(), nexaflowPathID(r, "id"))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	if err := nexaflow.ValidateDefinitionExtended(def, nexaflow.DefaultRegistry()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func (s *Server) adminNexaflowDefinitionPublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	def, err := s.nexaflowStore.GetDraft(r.Context(), nexaflowPathID(r, "id"))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	ver, published, err := s.nexaflowStore.Publish(r.Context(), def, adminIdentityFromRequest(r).Subject)
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"definition": published, "version": ver})
}

func (s *Server) adminNexaflowDefinitionVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	versions, err := s.nexaflowStore.ListVersions(r.Context(), nexaflowPathID(r, "id"))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (s *Server) adminNexaflowDefinitionVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	rev, err := strconv.ParseUint(nexaflowPathID(r, "rev"), 10, 64)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "revision must be a number")
		return
	}
	def, ver, err := s.nexaflowStore.GetVersion(r.Context(), nexaflowPathID(r, "id"), rev)
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"definition": def, "version": ver})
}

func (s *Server) adminNexaflowDefinitionRuns(w http.ResponseWriter, r *http.Request) {
	// Method first, readiness second: see adminNexaflowDefinitions.
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	id := nexaflowPathID(r, "id")
	switch r.Method {
	case http.MethodGet:
		runs, err := s.nexaflowStore.ListRuns(r.Context(), nexaflow.RunFilter{DefinitionID: id, Limit: nexaflowLimit(r, 50)})
		if err != nil {
			nexaflowError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": nexaflowRunSummaries(runs)})
	case http.MethodPost:
		var in struct {
			Input          map[string]any `json:"input"`
			Revision       uint64         `json:"revision"`
			IdempotencyKey string         `json:"idempotency_key"`
		}
		if _, err := readJSON(r, &in); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		run, err := s.nexaflowEngine.StartRunAsync(r.Context(), adminIdentityFromRequest(r), id, in.Revision, nexaflow.TriggerManual, "", in.IdempotencyKey, in.Input)
		if err != nil {
			nexaflowError(w, err)
			return
		}
		// Only the immutable id is read back: the dispatched run's
		// status mutates concurrently in its execution goroutine,
		// so polling the run record is the only sound way to observe
		// it. "accepted" is the dispatch outcome, not a run status.
		writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.ID, "status": "accepted"})
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func nexaflowRunSummaries(runs []*nexaflow.Run) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, nexaflowRunSummary(run))
	}
	return out
}

func nexaflowRunSummary(run *nexaflow.Run) map[string]any {
	var errCode string
	if run.Error != nil {
		errCode = run.Error.Code
	}
	return map[string]any{
		"id": run.ID, "definition_id": run.DefinitionID, "revision": run.Revision,
		"tenant_scope": run.TenantScope, "trigger": run.Trigger, "actor": run.Actor,
		"status": run.Status, "error_code": errCode,
		"created_at": run.CreatedAt, "updated_at": run.UpdatedAt, "finished_at": run.FinishedAt,
	}
}

func (s *Server) adminNexaflowRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	run, err := s.nexaflowStore.GetRun(r.Context(), nexaflowPathID(r, "id"))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) adminNexaflowRunApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	var in struct {
		Decision string `json:"decision"`
	}
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	run, err := s.nexaflowEngine.ApproveRun(r.Context(), adminIdentityFromRequest(r), nexaflowPathID(r, "id"), in.Decision)
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) adminNexaflowRunResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if _, err := readJSON(r, &in); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	run, err := s.nexaflowEngine.ResumeRun(r.Context(), adminIdentityFromRequest(r), nexaflowPathID(r, "id"), nexaflow.ResumeMode(in.Mode))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) adminNexaflowRunCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	run, err := s.nexaflowEngine.CancelRun(r.Context(), adminIdentityFromRequest(r), nexaflowPathID(r, "id"))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) adminNexaflowAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	events, err := s.nexaflowStore.ListAudit(r.Context(), nexaflowLimit(r, 100))
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) adminNexaflowSchedulerTick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) || s.nexaflowScheduler == nil {
		errorJSON(w, http.StatusServiceUnavailable, "nexaflow scheduler is not configured on this gateway")
		return
	}
	started, err := s.nexaflowScheduler.Tick(r.Context())
	if err != nil {
		nexaflowError(w, err)
		return
	}
	ids := make([]string, 0, len(started))
	for _, run := range started {
		ids = append(ids, run.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"started": ids})
}

// nexaflowWebhook is the signed webhook entry point. It deliberately lives
// outside /admin/api: authentication is the HMAC signature plus the
// durable replay ledger inside HandleWebhook, not the admin session.
// Headers: X-NexaFlow-Timestamp (unix seconds), X-NexaFlow-Signature
// (hex HMAC-SHA256 over "<timestamp>.<body>").
func (s *Server) nexaflowWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.nexaflowReady(w) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/nexaflow/webhooks/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		errorJSON(w, http.StatusNotFound, "path must be /nexaflow/webhooks/{definition}/{trigger}")
		return
	}
	timestamp := r.Header.Get("X-NexaFlow-Timestamp")
	signature := r.Header.Get("X-NexaFlow-Signature")
	if timestamp == "" || signature == "" {
		errorJSON(w, http.StatusBadRequest, "X-NexaFlow-Timestamp and X-NexaFlow-Signature are required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, nexaflow.MaxWorkflowInputBytes+1))
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "cannot read webhook body: "+err.Error())
		return
	}
	run, err := s.nexaflowEngine.HandleWebhook(r.Context(), parts[0], parts[1], timestamp, signature, body)
	if err != nil {
		nexaflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": nexaflowRunSummary(run)})
}
