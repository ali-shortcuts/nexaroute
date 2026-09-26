package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// Phase H bounds for the admin surface.
const (
	maxEvaluationRunBodyBytes = 4 << 20
	maxAdminScorecardRows     = 500
	maxAdminEvaluationRuns    = 100
	maxEvaluationArtifactRows = 512
)

func parseBoundedLimit(r *http.Request, name string, def, max int) int {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// adminScorecards serves GET /admin/api/scorecards — a bounded, provenance-labelled
// view of the scorecard registry. An empty list means "no evidence", never "all
// unknown models scored 0".
func (s *Server) adminScorecards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	plane := s.evaluationSnapshot()
	if plane == nil {
		writeJSON(w, http.StatusOK, map[string]any{"scorecards": []any{}, "total": 0, "stats": nil})
		return
	}
	limit := parseBoundedLimit(r, "limit", 50, maxAdminScorecardRows)
	filter := strings.TrimSpace(r.URL.Query().Get("deployment"))
	if filter != "" {
		row, ok := plane.scorecardDetail(filter)
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"scorecards": []any{},
				"total":      plane.Registry().Len(),
				"stats":      plane.Stats(),
				"note":       "no scorecard for deployment (no evidence recorded)",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"scorecards": []any{row},
			"total":      plane.Registry().Len(),
			"stats":      plane.Stats(),
		})
		return
	}
	rows := plane.scorecardRows(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"scorecards": rows,
		"total":      plane.Registry().Len(),
		"stats":      plane.Stats(),
	})
}

// adminScorecardByDeployment serves GET /admin/api/scorecards/{deployment_id}.
func (s *Server) adminScorecardByDeployment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/admin/api/scorecards/")
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 512 || strings.ContainsAny(id, " \t\r\n") {
		errorJSON(w, http.StatusBadRequest, "invalid deployment id")
		return
	}
	plane := s.evaluationSnapshot()
	if plane == nil {
		errorJSON(w, http.StatusNotFound, "no scorecard for deployment")
		return
	}
	row, ok := plane.scorecardDetail(id)
	if !ok {
		errorJSON(w, http.StatusNotFound, "no scorecard for deployment")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scorecard": row})
}

// adminEvaluationSuites serves the deterministic suite/evaluator catalog.
func (s *Server) adminEvaluationSuites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	plane := s.evaluationSnapshot()
	stats := evaluationStats{}
	if plane != nil {
		stats = plane.Stats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    plane != nil && plane.Enabled(),
		"suites":     eval.Catalog(),
		"evaluators": stats.Evaluators,
		"bounds": map[string]int{
			"max_artifacts_per_run": eval.MaxOutcomesPerRun,
			"max_cases_per_suite":   eval.MaxCasesPerSuite,
			"max_stored_runs":       eval.MaxStoredRuns,
			"max_scorecards":        plane.maxScorecards(),
		},
		"judge_available": stats.JudgeRegistered,
		"judge_note":      "Phase H ships no judge implementation; deterministic evaluators always take precedence",
	})
}

func (p *evaluationPlane) maxScorecards() int {
	cfg := p.config()
	if cfg.MaxScorecards <= 0 {
		return 0
	}
	return cfg.MaxScorecards
}

// adminEvaluationRuns serves the bounded run history plus the evaluation-only
// health namespace.
func (s *Server) adminEvaluationRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	plane := s.evaluationSnapshot()
	if plane == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []any{}, "health": []any{}, "stats": nil})
		return
	}
	limit := parseBoundedLimit(r, "limit", 20, maxAdminEvaluationRuns)
	if id := strings.TrimSpace(r.URL.Query().Get("run")); id != "" {
		run, ok := plane.Store().Get(id)
		if !ok {
			errorJSON(w, http.StatusNotFound, "unknown evaluation run")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"run": run, "stats": plane.Stats()})
		return
	}
	runs := plane.Store().Recent(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":      evalRunRows(runs),
		"health":    plane.evaluationHealthRows(eval.MaxOutcomesPerRun),
		"stats":     plane.Stats(),
		"run_limit": limit,
		"note":      "evaluation health is a separate namespace and never affects routing health",
	})
}

func evalRunRows(runs []eval.Result) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		row := map[string]any{
			"run_id":         run.RunID,
			"suite_id":       run.SuiteID,
			"suite_version":  run.SuiteVersion,
			"deployment_id":  run.DeploymentID,
			"provider_id":    run.ProviderID,
			"model":          run.Model,
			"started_at":     run.StartedAt.Format(time.RFC3339),
			"finished_at":    run.FinishedAt.Format(time.RFC3339),
			"duration_ms":    run.DurationMS,
			"cases":          len(run.Cases),
			"samples":        run.Samples,
			"score":          run.Score,
			"scoreable":      run.Scoreable,
			"reason":         run.Reason,
			"verdicts":       run.Counts,
			"judge_used":     run.JudgeUsed,
			"upstream_calls": run.Upstream,
		}
		out = append(out, row)
	}
	return out
}

// evaluationRunRequest is the POST /admin/api/evaluation/run payload. It carries
// recorded artifacts (evidence), never prompts: the gateway judges what a model
// already produced.
type evaluationRunRequest struct {
	SuiteID         string         `json:"suite_id"`
	DeploymentID    string         `json:"deployment_id"`
	ProviderID      string         `json:"provider_id,omitempty"`
	Model           string         `json:"model,omitempty"`
	CaseTimeoutMS   int            `json:"case_timeout_ms,omitempty"`
	LatencyTargetMS float64        `json:"latency_target_ms,omitempty"`
	TTFTTargetMS    float64        `json:"ttft_target_ms,omitempty"`
	Artifacts       []eval.Outcome `json:"artifacts"`
}

// adminEvaluationRun executes a deterministic suite against recorded artifacts,
// stores the run and writes a scorecard when the run produced evidence.
//
// It never calls an upstream model: Phase H evaluation is offline replay, so it
// cannot consume quota, pollute provider health or affect data-plane routing.
func (s *Server) adminEvaluationRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	plane := s.evaluationSnapshot()
	if plane == nil || !plane.Enabled() {
		errorJSON(w, http.StatusConflict, "evaluation plane is disabled (set evaluation.enabled)")
		return
	}
	cfg := plane.config()
	if cfg.MaxArtifacts <= 0 {
		cfg.MaxArtifacts = 128
	}
	var in evaluationRunRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEvaluationRunBodyBytes+1))
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	if len(body) > maxEvaluationRunBodyBytes {
		errorJSON(w, http.StatusRequestEntityTooLarge, "evaluation payload exceeds safe size")
		return
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.SuiteID == "" {
		errorJSON(w, http.StatusBadRequest, "suite_id is required")
		return
	}
	deploymentID := strings.TrimSpace(in.DeploymentID)
	if deploymentID == "" {
		errorJSON(w, http.StatusBadRequest, "deployment_id is required")
		return
	}
	dep, ok := s.deploymentForEvaluation(deploymentID)
	if !ok {
		errorJSON(w, http.StatusNotFound, "unknown deployment: "+deploymentID)
		return
	}
	if in.ProviderID != "" && in.ProviderID != dep.ProviderID {
		errorJSON(w, http.StatusBadRequest, "provider_id does not match deployment")
		return
	}
	if in.Model != "" && in.Model != dep.Model {
		errorJSON(w, http.StatusBadRequest, "model does not match deployment")
		return
	}
	if len(in.Artifacts) == 0 {
		errorJSON(w, http.StatusBadRequest, "artifacts are required: evaluation replays recorded evidence")
		return
	}
	if len(in.Artifacts) > cfg.MaxArtifacts {
		errorJSON(w, http.StatusBadRequest, "too many artifacts for one run")
		return
	}
	if _, ok := eval.LookupSuite(in.SuiteID); !ok {
		errorJSON(w, http.StatusBadRequest, "unknown suite_id: "+in.SuiteID)
		return
	}

	req := eval.Request{
		DeploymentID:    dep.ID,
		ProviderID:      dep.ProviderID,
		Model:           dep.Model,
		SuiteID:         in.SuiteID,
		CaseTimeoutMS:   in.CaseTimeoutMS,
		LatencyTargetMS: in.LatencyTargetMS,
		TTFTTargetMS:    in.TTFTTargetMS,
		Outcomes:        in.Artifacts,
	}
	if req.LatencyTargetMS == 0 {
		req.LatencyTargetMS = cfg.LatencyTargetMS
	}
	if req.TTFTTargetMS == 0 {
		req.TTFTTargetMS = cfg.TTFTTargetMS
	}
	if req.CaseTimeoutMS <= 0 || req.CaseTimeoutMS > eval.MaxRunTimeoutMS {
		req.CaseTimeoutMS = eval.MaxRunTimeoutMS
	}

	exec, execErr := eval.NewReplayExecutor(in.Artifacts)
	if execErr != nil {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, execErr.Error())
		return
	}
	run, err := plane.runner.RunWithExecutor(r.Context(), req, exec)
	if err != nil {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	plane.runsTotal.Add(1)
	if !run.Scoreable {
		plane.runsInsufficient.Add(1)
	}
	if err := plane.Store().Save(run); err != nil {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusInternalServerError, "cannot store evaluation run: "+err.Error())
		return
	}

	resp := map[string]any{"run": evalRunRows([]eval.Result{run})[0]}
	scorecard, written, err := run.Scorecard()
	switch {
	case err != nil:
		plane.runsRejected.Add(1)
		resp["scorecard"] = nil
		resp["scorecard_written"] = false
		resp["scorecard_error"] = err.Error()
	case !written:
		resp["scorecard"] = nil
		resp["scorecard_written"] = false
		resp["reason"] = "insufficient_samples: no scorecard was written"
	default:
		stored, upErr := plane.Registry().Upsert(scorecard)
		if upErr != nil {
			plane.runsRejected.Add(1)
			resp["scorecard"] = nil
			resp["scorecard_written"] = false
			resp["scorecard_error"] = upErr.Error()
			break
		}
		plane.scorecardsWritten.Add(1)
		resp["scorecard"] = map[string]any{
			"deployment_id": stored.DeploymentID,
			"version":       stored.Version,
			"values":        len(stored.Values),
			"generated_at":  stored.GeneratedAt.Format(time.RFC3339),
		}
		resp["scorecard_written"] = true
	}
	// Durability is best-effort and never fails the run.
	plane.persistBestEffort()
	s.emitEvaluationEvent(run, resp)
	writeJSON(w, http.StatusOK, resp)
}

// deploymentForEvaluation resolves a deployment from the live routing registry so
// an evaluation cannot invent a deployment that does not exist.
func (s *Server) deploymentForEvaluation(id string) (deploymentView, bool) {
	s.runtimeMu.RLock()
	rt := s.rt
	s.runtimeMu.RUnlock()
	if rt == nil {
		return deploymentView{}, false
	}
	for _, d := range rt.All() {
		if d.ID == id {
			return deploymentView{ID: d.ID, ProviderID: d.ProviderID, Model: d.Model}, true
		}
	}
	return deploymentView{}, false
}

type deploymentView struct {
	ID         string
	ProviderID string
	Model      string
}

// emitEvaluationEvent publishes a bounded, privacy-safe evaluation event. Model
// outputs never enter the event: only counts, verdicts and the produced score.
func (s *Server) emitEvaluationEvent(run eval.Result, resp map[string]any) {
	if s.bus == nil {
		return
	}
	parts := make([]string, 0, len(run.Counts))
	for _, v := range eval.SortedVerdictKeys() {
		if n, ok := run.Counts[v]; ok && n > 0 {
			parts = append(parts, string(v)+"="+strconv.Itoa(n))
		}
	}
	status := "scorecard"
	if !run.Scoreable {
		status = "insufficient_samples"
	}
	if written, ok := resp["scorecard_written"].(bool); ok && !written && run.Scoreable {
		status = "no_scorecard"
	}
	message := "suite=" + run.SuiteID + " samples=" + strconv.Itoa(run.Samples) +
		" score=" + strconv.FormatFloat(run.Score, 'f', 4, 64) +
		" status=" + status + " verdicts=" + strings.Join(parts, ",")
	s.bus.Add(events.Event{
		RequestID:  run.RunID,
		Kind:       "eval_run",
		Deployment: run.DeploymentID,
		Message:    message,
		TaskType:   boundedEvalLabel(run.SuiteID),
		StatusCode: http.StatusOK,
	})
}

func boundedEvalLabel(s string) string {
	if len(s) > 32 {
		return s[:32]
	}
	return s
}
