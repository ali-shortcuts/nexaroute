package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Task 8 NexaFlow HTTP API (canonical service, admin-authenticated).
//
// Routes exist only while a Service is attached (see AttachNexaflow);
// otherwise the paths fall through to the static file server and
// answer 404, so disabling NexaFlow preserves existing gateway
// behaviour exactly (scenario 15).
//
// Safety rules enforced here, not just in the service:
//   - every admin route requires admin authentication (401 otherwise);
//   - run detail exposes node outputs (already redacted at persist
//     time), timing, attempts, states, and error CODES — never raw
//     node error text and never node inputs, because error strings
//     and inputs are not redacted at persist time and could carry
//     secret-shaped values;
//   - the public webhook route authenticates by signature only; an
//     unknown definition and a definition without a configured secret
//     both answer 404, and verification failures answer a generic 401
//     (replays answer 409 with the replay code, which reveals nothing
//     secret).

func (s *Server) nexaflowAdmin(r *http.Request) (authz.Identity, bool) {
	if s.nexaflow == nil {
		return authz.Identity{}, false
	}
	identity, _, ok, err := s.authenticateAdmin(r)
	if err != nil || !ok {
		return authz.Identity{}, false
	}
	return identity, true
}

func (s *Server) nexaflowRequireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

func nexaflowDecodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "unreadable request body")
		return false
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return false
		}
	}
	return true
}

func nexaflowErrStatus(err error) int {
	switch {
	case errors.Is(err, nexaflow.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, nexaflow.ErrAlreadyExists),
		errors.Is(err, nexaflow.ErrIdempotencyConflict),
		errors.Is(err, nexaflow.ErrApprovalConflict),
		errors.Is(err, nexaflow.ErrRunLimit),
		errors.Is(err, nexaflow.ErrDefinitionBusy),
		errors.Is(err, nexaflow.ErrRunParked):
		return http.StatusConflict
	case errors.Is(err, nexaflow.ErrTerminalState),
		errors.Is(err, nexaflow.ErrStateConflict),
		errors.Is(err, nexaflow.ErrNotWaiting):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func nexaflowRunSummary(run nexaflow.Run) map[string]any {
	return map[string]any{
		"id": run.ID, "definition_id": run.DefinitionID, "revision": run.Revision,
		"state": run.State, "principal": run.Principal,
		"error_code": run.ErrorCode,
		"created_at": run.CreatedAt, "updated_at": run.UpdatedAt,
		"started_at": run.StartedAt, "finished_at": run.FinishedAt,
	}
}

func nexaflowNodeView(e nexaflow.NodeExecution) map[string]any {
	return map[string]any{
		"node_id": e.NodeID, "attempt": e.Attempt, "state": e.State,
		"output": e.Output, "output_truncated": e.OutputTruncated,
		"error_code": e.ErrorCode,
		"started_at": e.StartedAt, "finished_at": e.FinishedAt,
		"created_at": e.CreatedAt, "updated_at": e.UpdatedAt,
		"ambiguous": e.Ambiguous, "dispatched": e.Dispatched,
	}
}

func (s *Server) adminNexaflowNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.nexaflowAdmin(r); !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
		return
	}
	var out []map[string]any
	for _, spec := range s.nexaflow.Registry.All() {
		// The Visual Studio reads the canonical registry schema from
		// this endpoint and keeps no duplicate frontend schema.
		out = append(out, map[string]any{
			"id": spec.ID, "version": spec.Version, "title": spec.Title,
			"category": spec.Category, "description": spec.Description,
			"input_schema":       nexaflowFieldViews(spec.InputSchema),
			"output_schema":      nexaflowFieldViews(spec.OutputSchema),
			"config_schema":      nexaflowFieldViews(spec.ConfigSchema),
			"default_timeout_ms": spec.DefaultTimeout.Milliseconds(),
			"max_timeout_ms":     spec.MaxTimeout.Milliseconds(),
			"max_input_bytes":    spec.MaxInputBytes,
			"max_output_bytes":   spec.MaxOutputBytes,
			"permission":         string(spec.Permission),
			"side_effect":        string(spec.SideEffect),
			"requires_policy":    spec.RequiresOperatorPolicy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// nexaflowFieldViews renders registry schema fields in the snake_case
// shape the studio editor consumes (name/type/required/description);
// empty schemas render as [] rather than null.
func nexaflowFieldViews(fields []nexaflow.Field) []map[string]any {
	out := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		out = append(out, map[string]any{"name": f.Name, "type": f.Type, "required": f.Required, "description": f.Description})
	}
	return out
}

func (s *Server) adminNexaflowTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.nexaflowAdmin(r); !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
		return
	}
	var out []map[string]any
	for _, tpl := range nexaflow.DefaultTemplates() {
		out = append(out, map[string]any{
			"id": tpl.ID, "title": tpl.Title, "description": tpl.Description,
			"published": tpl.Published, "requires_policy": tpl.RequiresPolicy,
			"node_count": len(tpl.Nodes), "placeholders": tpl.Placeholders,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// adminNexaflowTemplateByID serves the /admin/api/nexaflow/templates/
// subtree:
//   - GET /templates/{id}: the full inert template, including its nodes,
//     so a studio/gallery client can build a workflow draft from it
//     (PR #732 detail). The list endpoint deliberately returns metadata
//     only.
//   - POST /templates/{id}/instantiate: server-side template
//     instantiation into a saved draft (studio port).
//
// Dispatch is shape-first for unknown shapes (404) and method-checked for
// known shapes (405 for a wrong method on a known shape), so the OpenAPI
// method-set probe and the studio error matrix both hold.
// adminNexaflowTemplateInstantiate serves POST
// /admin/api/nexaflow/templates/{id}/instantiate as its own exact route
// (see server.go): the templates/ subtree handler answers only GET on the
// detail path, so documenting instantiate as a POST on the detail path
// would contradict the handler (405) and fail the OpenAPI contract test.
func (s *Server) adminNexaflowTemplateInstantiate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.nexaflowAdmin(r); !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/nexaflow/templates/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "instantiate" || parts[0] == "" {
		errorJSON(w, http.StatusNotFound, "template not found")
		return
	}
	var body struct {
		Placeholders   map[string]string `json:"placeholders"`
		PolicyApproved bool              `json:"policy_approved"`
	}
	if !nexaflowDecodeBody(w, r, &body) {
		return
	}
	def, err := s.nexaflow.InstantiateTemplate(r.Context(), parts[0], body.Placeholders, body.PolicyApproved)
	if err != nil {
		errorJSON(w, nexaflowErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": def.ID, "definition": def})
}

func (s *Server) adminNexaflowTemplateByID(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.nexaflowAdmin(r); !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/nexaflow/templates/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 1 {
		errorJSON(w, http.StatusNotFound, "template not found")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
		return
	}
	id := parts[0]
	if id == "" {
		errorJSON(w, http.StatusNotFound, "template not found")
		return
	}
	for _, tpl := range nexaflow.DefaultTemplates() {
		if tpl.ID != id {
			continue
		}
		var nodes []map[string]any
		for _, n := range tpl.Nodes {
			nodes = append(nodes, map[string]any{"node_id": n.NodeID, "version": n.Version, "config": n.Config})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": tpl.ID, "version": tpl.Version, "title": tpl.Title,
			"description": tpl.Description, "nodes": nodes,
			"edges": tpl.Edges, "placeholders": tpl.Placeholders,
			"requires_policy": tpl.RequiresPolicy, "published": tpl.Published,
		})
		return
	}
	errorJSON(w, http.StatusNotFound, "template not found")
}

func (s *Server) adminNexaflowDefinitions(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.nexaflowAdmin(r)
	if !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	_ = identity
	switch r.Method {
	case http.MethodGet:
		drafts, err := s.nexaflow.ListDrafts(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definitions": drafts})
	case http.MethodPost:
		var def nexaflow.Definition
		if !nexaflowDecodeBody(w, r, &def) {
			return
		}
		if err := s.nexaflow.SaveDraft(r.Context(), def); err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": def.ID})
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// adminNexaflowDefinitionByID serves /admin/api/nexaflow/definitions/{id}
// and its sub-resources: publish, validate, versions, versions/{rev},
// runs (GET lists, POST starts).
func (s *Server) adminNexaflowDefinitionByID(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.nexaflowAdmin(r)
	if !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/nexaflow/definitions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		errorJSON(w, http.StatusNotFound, "definition not found")
		return
	}
	id := parts[0]
	ctx := r.Context()
	// Known sub-resource shapes answer 405 for a wrong method (so the
	// OpenAPI method-set contract can distinguish "route exists, wrong
	// method" from "no such route"); only unknown shapes answer 404.
	shape := ""
	switch {
	case len(parts) == 1:
		shape = "draft"
	case len(parts) == 2 && parts[1] == "publish":
		shape = "publish"
	case len(parts) == 2 && parts[1] == "validate":
		shape = "validate"
	case len(parts) == 2 && parts[1] == "versions":
		shape = "versions"
	case len(parts) == 3 && parts[1] == "versions":
		shape = "version"
	case len(parts) == 2 && parts[1] == "runs":
		shape = "runs"
	}
	switch shape {
	case "":
		errorJSON(w, http.StatusNotFound, "not found")
		return
	case "draft":
		if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
			return
		}
		def, err := s.nexaflow.GetDraft(ctx, id)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, def)
	case "validate":
		// Ported from #691's validate endpoint: validate the stored
		// draft without saving or publishing it. A validation
		// outcome is 200 with valid=false (mirroring #691); only a
		// missing draft or store error is an error status.
		if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
			return
		}
		draft, err := s.nexaflow.GetDraft(ctx, id)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		if err := s.nexaflow.Validate(&draft); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": true})
	case "publish":
		if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
			return
		}
		pub, err := s.nexaflow.Publish(ctx, id, identity)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definition_id": id, "revision": pub.Version.Revision, "status": pub.Version.Status})
	case "versions":
		if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
			return
		}
		revs, err := s.nexaflow.ListRevisions(ctx, id)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": revs})
	case "version":
		if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
			return
		}
		rev, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil {
			errorJSON(w, http.StatusBadRequest, "invalid revision")
			return
		}
		pub, err := s.nexaflow.GetRevision(ctx, id, rev)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, pub)
	case "runs":
		if r.Method == http.MethodGet {
			runs, err := s.nexaflow.ListRuns(ctx, nexaflow.RunFilter{DefinitionID: id}, identity)
			if err != nil {
				errorJSON(w, nexaflowErrStatus(err), err.Error())
				return
			}
			var out []map[string]any
			for _, run := range runs {
				out = append(out, nexaflowRunSummary(run))
			}
			writeJSON(w, http.StatusOK, map[string]any{"runs": out})
			return
		}
		if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Revision       uint64         `json:"revision"`
			Input          map[string]any `json:"input"`
			IdempotencyKey string         `json:"idempotency_key"`
		}
		if !nexaflowDecodeBody(w, r, &body) {
			return
		}
		var pub nexaflow.PublishedDefinition
		var err error
		if body.Revision == 0 {
			pub, err = s.nexaflow.LatestPublished(ctx, id)
		} else {
			pub, err = s.nexaflow.GetRevision(ctx, id, body.Revision)
		}
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		run, err := s.nexaflow.StartRun(ctx, pub, identity, body.Input, body.IdempotencyKey)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		run, err = s.nexaflow.ExecuteRun(ctx, run.ID)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, nexaflowRunSummary(run))
	}
}

func (s *Server) adminNexaflowRuns(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.nexaflowAdmin(r)
	if !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
		return
	}
	runs, err := s.nexaflow.ListRuns(r.Context(), nexaflow.RunFilter{DefinitionID: r.URL.Query().Get("definition_id")}, identity)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []map[string]any
	for _, run := range runs {
		out = append(out, nexaflowRunSummary(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// adminNexaflowRunByID serves /admin/api/nexaflow/runs/{id},
// /approve, /cancel, and /resume (re-drive after restart/recovery).
func (s *Server) adminNexaflowRunByID(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.nexaflowAdmin(r)
	if !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/nexaflow/runs/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		errorJSON(w, http.StatusNotFound, "run not found")
		return
	}
	id := parts[0]
	ctx := r.Context()
	// Same 405-vs-404 discipline as the definitions handler: known
	// shapes with a wrong method answer 405, unknown shapes 404.
	shape := ""
	switch {
	case len(parts) == 1:
		shape = "run"
	case len(parts) == 2 && parts[1] == "approve":
		shape = "approve"
	case len(parts) == 2 && parts[1] == "cancel":
		shape = "cancel"
	case len(parts) == 2 && parts[1] == "resume":
		shape = "resume"
	}
	switch shape {
	case "":
		errorJSON(w, http.StatusNotFound, "not found")
		return
	case "run":
		if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
			return
		}
		run, execs, err := s.nexaflow.GetRun(ctx, id, identity)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		var nodes []map[string]any
		for _, e := range execs {
			nodes = append(nodes, nexaflowNodeView(e))
		}
		writeJSON(w, http.StatusOK, map[string]any{"run": nexaflowRunSummary(run), "nodes": nodes, "definition": run.Definition})
	case "approve":
		if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			NodeID   string `json:"node_id"`
			Decision string `json:"decision"`
		}
		if !nexaflowDecodeBody(w, r, &body) {
			return
		}
		if body.NodeID == "" || (body.Decision != "approved" && body.Decision != "rejected") {
			errorJSON(w, http.StatusBadRequest, "node_id and decision (approved|rejected) are required")
			return
		}
		if _, err := s.nexaflow.ApproveRun(ctx, id, body.NodeID, identity, body.Decision); err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		run, err := s.nexaflow.ExecuteRun(ctx, id)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, nexaflowRunSummary(run))
	case "cancel":
		if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
			return
		}
		run, err := s.nexaflow.CancelRun(ctx, id, identity)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, nexaflowRunSummary(run))
	case "resume":
		if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
			return
		}
		run, err := s.nexaflow.ResumeRun(ctx, id, identity)
		if err != nil {
			errorJSON(w, nexaflowErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, nexaflowRunSummary(run))
	}
}

func (s *Server) adminNexaflowAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.nexaflowAdmin(r); !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": s.nexaflow.Audit()})
}

// nexaflowWebhookHandler serves the public signed webhook:
// POST /nexaflow/webhooks/{definition}/{trigger} with
// X-Nexaflow-Timestamp and X-Nexaflow-Signature headers. The signature
// is the authentication; no admin session is required or accepted.
func (s *Server) nexaflowWebhookHandler(w http.ResponseWriter, r *http.Request) {
	if s.nexaflowWebhook == nil {
		errorJSON(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/nexaflow/webhooks/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		errorJSON(w, http.StatusNotFound, "not found")
		return
	}
	defID, trigger := parts[0], parts[1]
	secret, ok := s.nexaflowSecrets[defID]
	if !ok || secret == "" {
		errorJSON(w, http.StatusNotFound, "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "unreadable body")
		return
	}
	runID, err := s.nexaflowWebhook.Handle(r.Context(), defID, trigger,
		r.Header.Get("X-Nexaflow-Timestamp"), r.Header.Get("X-Nexaflow-Signature"),
		string(body), secret)
	if err != nil {
		switch {
		case errors.Is(err, nexaflow.ErrWebhookReplay):
			errorJSON(w, http.StatusConflict, "webhook_replay")
		case errors.Is(err, nexaflow.ErrNotFound):
			errorJSON(w, http.StatusNotFound, "not found")
		default:
			// Generic: bad signature, stale timestamp, and malformed
			// deliveries are indistinguishable from outside.
			errorJSON(w, http.StatusUnauthorized, "webhook rejected")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID})
}
