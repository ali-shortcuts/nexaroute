package httpapi

import (
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// Studio port: the draft-body validate route the standalone Visual Studio
// page (/studio.html) needs beyond the Task 8 admin API. Template
// instantiation lives in the templates/ subtree handler in
// admin_nexaflow_task8.go, alongside the #732 detail route. Both ride the
// interim canonical permission mapping (everything under
// /admin/api/nexaflow/ maps to config.read for GET and config.write
// for POST in adminPermissionForRequest); no workflow.* permission is
// invented here and no tenant value is ever read from a request.

// adminNexaflowDefinitionValidate serves POST
// /admin/api/nexaflow/definitions/validate. It validates the definition
// submitted in the body (draft-body validation): the studio validates
// unsaved edits without saving first, so validation can never silently
// answer for a stale saved draft. The verdict is always HTTP 200; an
// invalid definition answers valid:false with the validation error,
// never a 4xx that the UI would conflate with a failed request.
//
// Prospective identity: a draft that has never been saved has no id
// or name yet (both are assigned by the save dialog), and a new draft
// that cannot validate before its first save is unusable. Identity is
// not part of the validation verdict — SaveDraft still enforces
// id/name via ValidateDefinition — so empty id/name are substituted
// for validation only. The submitted definition is never stored here.
func (s *Server) adminNexaflowDefinitionValidate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.nexaflowAdmin(r); !ok {
		errorJSON(w, http.StatusUnauthorized, "admin authentication required")
		return
	}
	if !s.nexaflowRequireMethod(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Definition nexaflow.Definition `json:"definition"`
	}
	if !nexaflowDecodeBody(w, r, &body) {
		return
	}
	def := body.Definition
	if def.ID == "" {
		def.ID = "validate-draft"
	}
	if def.Name == "" {
		def.Name = "validate-draft"
	}
	err := s.nexaflow.Validate(&def)
	resp := map[string]any{
		"valid": err == nil, "secret_refs": nexaflow.DefinitionSecretRefs(&body.Definition, s.nexaflow.Registry),
	}
	if err != nil {
		resp["errors"] = []string{err.Error()}
	}
	writeJSON(w, http.StatusOK, resp)
}
