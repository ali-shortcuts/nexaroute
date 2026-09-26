package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/evallive"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
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
	liveEnabled := false
	var liveBounds map[string]int
	if plane != nil {
		cfg := plane.config()
		liveEnabled = plane.Enabled() && cfg.LiveEnabled
		liveBounds = map[string]int{
			"max_prompts_per_run": evallive.MaxPromptsPerRun,
			"max_prompt_bytes":    evallive.MaxPromptBytes,
			"max_output_tokens":   evallive.MaxOutputTokens,
		}
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
		"modes": []string{evaluationModeReplay, evaluationModeLive},
		"mode_notes": map[string]string{
			evaluationModeReplay: "offline: grades recorded artifacts, performs no upstream I/O",
			evaluationModeLive:   "online: sends prompts to exactly one explicitly selected physical deployment, bypassing all DecisionProviders",
		},
		"live_enabled":    liveEnabled,
		"live_bounds":     liveBounds,
		"live_note":       "live evaluation is opt-in (evaluation.live_enabled), targets one explicit deployment_id, and is isolated from production health, affinity, cache and routing state",
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

// Evaluation modes. Evaluation never guesses: a run must say whether it grades
// recorded evidence (replay) or a physically selected deployment (live).
const (
	evaluationModeReplay = "replay"
	evaluationModeLive   = "live"
)

// livePrompt binds one live input to one suite case.
type livePrompt struct {
	CaseID string `json:"case_id"`
	Prompt string `json:"prompt"`
}

// evaluationRunRequest is the POST /admin/api/evaluation/run payload.
//
// mode=replay (default) carries recorded artifacts — evidence, never prompts:
// the gateway judges what a model already produced.
//
// mode=live carries the prompts to send to exactly one explicitly selected
// physical deployment. It accepts no artifacts: a live run measures a model, it
// does not grade someone else's recording.
type evaluationRunRequest struct {
	Mode            string         `json:"mode,omitempty"`
	SuiteID         string         `json:"suite_id"`
	DeploymentID    string         `json:"deployment_id"`
	ProviderID      string         `json:"provider_id,omitempty"`
	Model           string         `json:"model,omitempty"`
	CaseTimeoutMS   int            `json:"case_timeout_ms,omitempty"`
	LatencyTargetMS float64        `json:"latency_target_ms,omitempty"`
	TTFTTargetMS    float64        `json:"ttft_target_ms,omitempty"`
	Artifacts       []eval.Outcome `json:"artifacts,omitempty"`
	Prompts         []livePrompt   `json:"prompts,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
}

// normalizeEvaluationMode resolves the run mode. Empty means replay, so every
// existing caller keeps its offline, no-network behaviour.
func normalizeEvaluationMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return evaluationModeReplay, nil
	}
	switch mode {
	case evaluationModeReplay, evaluationModeLive:
		return mode, nil
	}
	return "", fmt.Errorf("unknown evaluation mode %q (want %q or %q)", raw, evaluationModeReplay, evaluationModeLive)
}

// adminEvaluationRun executes a deterministic suite in one of two explicit modes:
//
//   - mode=replay (default) grades recorded artifacts. It performs no I/O, so it
//     cannot consume quota, pollute provider health or affect data-plane routing.
//   - mode=live sends real prompts to exactly one explicitly selected physical
//     deployment. It bypasses every DecisionProvider and never writes to
//     production health, affinity, cache or routing state.
//
// Both modes store the run and write a scorecard only when the run produced
// scoreable evidence.
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
	mode, modeErr := normalizeEvaluationMode(in.Mode)
	if modeErr != nil {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, modeErr.Error())
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
	suite, ok := eval.LookupSuite(in.SuiteID)
	if !ok {
		errorJSON(w, http.StatusBadRequest, "unknown suite_id: "+in.SuiteID)
		return
	}

	if mode == evaluationModeLive {
		s.adminEvaluationRunLive(w, r, plane, cfg, in, dep, suite)
		return
	}
	s.adminEvaluationRunReplay(w, r, plane, cfg, in, dep, suite)
}

// adminEvaluationRunLive executes one suite against one physically selected
// deployment.
//
// Isolation is enforced here and in the executor, not assumed: the upstream call
// goes through an evaluation-isolated adapter clone, no DecisionProvider runs,
// no health is recorded, no session pin is created, and the response cache is
// never read or written.
func (s *Server) adminEvaluationRunLive(w http.ResponseWriter, r *http.Request, plane *evaluationPlane,
	cfg config.EvaluationConfig, in evaluationRunRequest, dep deploymentView, suite eval.Suite) {
	if !cfg.LiveEnabled {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusConflict, "live evaluation is disabled (set evaluation.live_enabled)")
		return
	}
	if len(in.Artifacts) > 0 {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, "live evaluation accepts prompts, not artifacts")
		return
	}
	if len(in.Prompts) == 0 {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, "prompts are required for live evaluation")
		return
	}
	if len(in.Prompts) > evallive.MaxPromptsPerRun {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, "too many prompts for one live run")
		return
	}
	known := make(map[string]struct{}, len(suite.Cases))
	for _, c := range suite.Cases {
		known[c.ID] = struct{}{}
	}
	prompts := make([]evallive.Prompt, 0, len(in.Prompts))
	for _, p := range in.Prompts {
		if _, ok := known[p.CaseID]; !ok {
			plane.runsRejected.Add(1)
			errorJSON(w, http.StatusBadRequest, "unknown case_id for suite "+suite.ID+": "+p.CaseID)
			return
		}
		prompts = append(prompts, evallive.Prompt{CaseID: p.CaseID, Prompt: p.Prompt})
	}

	// The deployment is already selected by the request: no candidate set is
	// built, no DecisionProvider is consulted and no fallback exists.
	adapter, ok := s.reg.Get(dep.ProviderID)
	if !ok {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusNotFound, "no provider adapter for deployment: "+dep.ID)
		return
	}
	twin, ok := providers.EvaluationTwin(adapter)
	if !ok {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusConflict, "provider does not support isolated live evaluation: "+dep.ProviderID)
		return
	}
	exec, execErr := evallive.NewLiveEvaluationExecutor(
		evallive.Deployment{ID: dep.ID, ProviderID: dep.ProviderID, Model: dep.Model, ProviderType: dep.ProviderType},
		twin, prompts, in.MaxOutputTokens)
	if execErr != nil {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, execErr.Error())
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

	resp := s.evaluationRunResponse(plane, run)
	resp["mode"] = evaluationModeLive
	// Privacy: prompts are inputs, never record contents. Only the count of real
	// upstream calls is reported, never what was sent or returned.
	resp["upstream_calls"] = run.Upstream
	resp["live"] = map[string]any{
		"deployment_id":     dep.ID,
		"provider_id":       dep.ProviderID,
		"model":             dep.Model,
		"upstream_calls":    run.Upstream,
		"executor_calls":    exec.Calls(),
		"cases_with_prompt": len(exec.PromptCaseIDs()),
		"failed_calls":      exec.Failures(),
		"missing_prompts":   exec.Missing(),
		"max_output_tokens": exec.MaxOutputTokensForRun(),
		"isolation":         "evaluation-isolated adapter clone; no DecisionProvider, health, affinity or cache state touched",
	}
	plane.persistBestEffort()
	s.emitEvaluationEvent(run, resp)
	writeJSON(w, http.StatusOK, resp)
}

// adminEvaluationRunLive is documented above adminEvaluationRun.
// adminEvaluationRunReplay grades recorded artifacts. It performs no network
// I/O at all: a replayed run cannot reach an upstream, consume quota or change
// production state.
func (s *Server) adminEvaluationRunReplay(w http.ResponseWriter, r *http.Request, plane *evaluationPlane,
	cfg config.EvaluationConfig, in evaluationRunRequest, dep deploymentView, suite eval.Suite) {
	if len(in.Prompts) > 0 {
		plane.runsRejected.Add(1)
		errorJSON(w, http.StatusBadRequest, "replay evaluation accepts artifacts, not prompts")
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

	resp := s.evaluationRunResponse(plane, run)
	resp["mode"] = evaluationModeReplay
	// Durability is best-effort and never fails the run.
	plane.persistBestEffort()
	s.emitEvaluationEvent(run, resp)
	writeJSON(w, http.StatusOK, resp)
}

// evaluationRunResponse builds the shared run payload: the bounded run row plus
// the scorecard outcome. It never includes case inputs or model outputs, so no
// live prompt or completion can reach an admin payload.
func (s *Server) evaluationRunResponse(plane *evaluationPlane, run eval.Result) map[string]any {
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
	return resp
}

// deploymentForEvaluation resolves a deployment from the live routing registry so
// an evaluation cannot invent a deployment that does not exist. It is a read-only
// projection: nothing here can change routing state.
func (s *Server) deploymentForEvaluation(id string) (deploymentView, bool) {
	s.runtimeMu.RLock()
	rt := s.rt
	s.runtimeMu.RUnlock()
	if rt == nil {
		return deploymentView{}, false
	}
	for _, d := range rt.All() {
		if d.ID == id {
			return deploymentView{ID: d.ID, ProviderID: d.ProviderID, Model: d.Model, ProviderType: d.ProviderType}, true
		}
	}
	return deploymentView{}, false
}

type deploymentView struct {
	ID           string
	ProviderID   string
	Model        string
	ProviderType string
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
