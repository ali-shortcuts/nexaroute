package httpapi

import (
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// evaluationPlane owns Phase H state: the versioned scorecard registry, the
// bounded evaluation run store and the deterministic runner.
//
// Isolation contract: the plane is written by the admin surface only. It is not
// reachable from the data plane (no handler in the request path calls it), it does
// not import the router/health/providers packages, and nothing reads scorecards
// back into routing. Phase H therefore cannot change real routing.
type evaluationPlane struct {
	mu     sync.RWMutex
	cfg    config.EvaluationConfig
	reg    *scorecards.Registry
	runner *eval.Runner
	store  *eval.Store
	// importError records the last artifact import failure, if any, so the admin
	// surface can show why scorecards are missing instead of pretending.
	importError string
	imported    int
	// loadedStatePath records which state file has already been read so a hot
	// reload of unrelated settings cannot roll memory back to a stale file.
	loadedStatePath string

	stateWritesFailed atomic.Uint64
	runsTotal         atomic.Uint64
	runsInsufficient  atomic.Uint64
	runsRejected      atomic.Uint64
	scorecardsWritten atomic.Uint64
	importFailures    atomic.Uint64
}

func newEvaluationPlane(cfg config.EvaluationConfig, previous *evaluationPlane) *evaluationPlane {
	p := &evaluationPlane{cfg: cfg}
	if previous != nil {
		// Reuse the live registry/store so hot reloads do not silently discard
		// evidence that already exists.
		p.reg = previous.Registry()
		p.runner = previous.runner
		p.store = previous.Store()
		p.runsTotal.Store(previous.runsTotal.Load())
		p.runsInsufficient.Store(previous.runsInsufficient.Load())
		p.runsRejected.Store(previous.runsRejected.Load())
		p.scorecardsWritten.Store(previous.scorecardsWritten.Load())
		p.stateWritesFailed.Store(previous.stateWritesFailed.Load())
		p.importFailures.Store(previous.importFailures.Load())
		p.imported = previous.imported
		p.loadedStatePath = previous.loadedStatePath
	}
	if p.reg == nil {
		p.reg = scorecards.NewRegistry(cfg.MaxScorecards, 4)
	}
	if p.runner == nil {
		p.runner = eval.NewRunner()
	}
	if p.store == nil {
		p.store = eval.NewStore(cfg.MaxRuns)
	}
	return p
}

// Registry returns the scorecard registry (thread-safe).
func (p *evaluationPlane) Registry() *scorecards.Registry { return p.reg }

// Store returns the bounded run store (thread-safe).
func (p *evaluationPlane) Store() *eval.Store { return p.store }

// Enabled reports whether the plane is configured to accept runs and imports.
func (p *evaluationPlane) Enabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Enabled
}

func (p *evaluationPlane) config() config.EvaluationConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *evaluationPlane) setConfig(cfg config.EvaluationConfig) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Configure applies a new evaluation config to the plane: it re-points the state
// path, reloads the artifact import when the path changed, and keeps previously
// recorded runs and scorecards.
func (p *evaluationPlane) Configure(cfg config.EvaluationConfig, l *log.Logger) {
	p.setConfig(cfg)
	if cfg.StatePath != "" && p.loadedStatePath != cfg.StatePath {
		if err := p.loadState(cfg.StatePath); err != nil {
			p.setImportError(fmt.Sprintf("evaluation state file: %v", err))
			logf(l, "evaluation state file %q unusable: %v", cfg.StatePath, err)
		} else {
			p.mu.Lock()
			p.loadedStatePath = cfg.StatePath
			p.mu.Unlock()
		}
	}
	if cfg.Enabled && cfg.ImportPath != "" {
		if err := p.loadImport(cfg.ImportPath); err != nil {
			p.importFailures.Add(1)
			p.setImportError(err.Error())
			logf(l, "scorecard import %q failed: %v", cfg.ImportPath, err)
			return
		}
	}
}

func (p *evaluationPlane) setImportError(msg string) {
	p.mu.Lock()
	p.importError = msg
	p.mu.Unlock()
}

// ImportError returns the last artifact import error ("" when the last import
// succeeded).
func (p *evaluationPlane) ImportError() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.importError
}

// ImportedCount returns how many imported scorecards are loaded.
func (p *evaluationPlane) ImportedCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.imported
}

// loadImport replaces the registry contents with an externally produced artifact.
// The load is all-or-nothing: a malformed artifact never partially replaces
// existing evidence.
func (p *evaluationPlane) loadImport(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > 8<<20 {
		return fmt.Errorf("artifact exceeds safe size (%d bytes)", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg := p.config()
	max := cfg.MaxScorecards
	if max <= 0 {
		max = scorecards.MaxDeployments
	}
	list, err := scorecards.ImportJSON(data, path, max)
	if err != nil {
		return err
	}
	// Capacity pre-check keeps the import atomic even when the registry is nearly
	// full: the artifact is rejected before anything is written, never half
	// applied. Updating known deployments is always allowed.
	newDeployments := 0
	for _, sc := range list {
		if _, ok := p.reg.Get(sc.DeploymentID); !ok {
			newDeployments++
		}
	}
	if newDeployments > 0 && p.reg.Len()+newDeployments > max {
		return fmt.Errorf("scorecard artifact would exceed the %d-deployment registry bound", max)
	}
	for _, sc := range list {
		if _, err := p.reg.Upsert(sc); err != nil {
			return fmt.Errorf("scorecard %q: %w", sc.DeploymentID, err)
		}
	}
	p.mu.Lock()
	p.imported = len(list)
	p.importError = ""
	p.mu.Unlock()
	return nil
}

// Retain drops evaluation evidence for deployments that no longer exist.
func (p *evaluationPlane) Retain(valid map[string]struct{}) {
	p.reg.Retain(valid)
	p.store.Retain(valid)
}

// Stats is the bounded observability view of the evaluation plane.
type evaluationStats struct {
	Enabled           bool                 `json:"enabled"`
	LiveEnabled       bool                 `json:"live_enabled"`
	JudgeRegistered   bool                 `json:"judge_registered"`
	ImportPath        string               `json:"import_path,omitempty"`
	StatePath         string               `json:"state_path,omitempty"`
	Imported          int                  `json:"imported_scorecards"`
	ImportError       string               `json:"import_error,omitempty"`
	Runs              int                  `json:"runs_stored"`
	RunsTotal         uint64               `json:"runs_total"`
	RunsInsufficient  uint64               `json:"runs_insufficient_samples"`
	RunsRejected      uint64               `json:"runs_rejected"`
	ScorecardsWritten uint64               `json:"scorecards_written"`
	StateWritesFailed uint64               `json:"state_writes_failed"`
	Scorecards        scorecards.Stats     `json:"scorecard_registry"`
	VerdictCounts     map[eval.Verdict]int `json:"verdict_counts"`
	SuiteRuns         map[string]int       `json:"suite_runs"`
	Evaluators        []string             `json:"evaluators"`
}

func (p *evaluationPlane) Stats() evaluationStats {
	cfg := p.config()
	st := evaluationStats{
		Enabled:           cfg.Enabled,
		LiveEnabled:       cfg.Enabled && cfg.LiveEnabled,
		JudgeRegistered:   len(p.runner.Evaluators().Judges()) > 0,
		ImportPath:        cfg.ImportPath,
		StatePath:         cfg.StatePath,
		Imported:          p.ImportedCount(),
		ImportError:       p.ImportError(),
		Runs:              p.store.Len(),
		RunsTotal:         p.runsTotal.Load(),
		RunsInsufficient:  p.runsInsufficient.Load(),
		RunsRejected:      p.runsRejected.Load(),
		ScorecardsWritten: p.scorecardsWritten.Load(),
		StateWritesFailed: p.stateWritesFailed.Load(),
		Scorecards:        p.reg.Stats(),
		VerdictCounts:     p.store.Counts(),
		SuiteRuns:         p.store.SuiteCounts(),
		Evaluators:        p.runner.Evaluators().IDs(),
	}
	return st
}

// evaluationHealthRows returns the evaluation-plane health namespace. It is
// derived only from evaluation runs and never used by routing.
func (p *evaluationPlane) evaluationHealthRows(limit int) []eval.EvaluationHealth {
	runs := p.store.All()
	if limit > 0 && len(runs) > limit {
		runs = runs[len(runs)-limit:]
	}
	return eval.HealthFromRuns(runs)
}

// scorecardRows builds the bounded admin/snapshot view of scorecards.
func (p *evaluationPlane) scorecardRows(limit int) []map[string]any {
	list := p.reg.Snapshot()
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	out := make([]map[string]any, 0, len(list))
	for _, sc := range list {
		values := make([]map[string]any, 0, len(sc.Values))
		dims := make([]string, 0, len(sc.Values))
		for d := range sc.Values {
			dims = append(dims, string(d))
		}
		sort.Strings(dims)
		for _, d := range dims {
			v := sc.Values[scorecards.Dimension(d)]
			row := map[string]any{
				"dimension":    d,
				"score":        v.Score,
				"provenance":   string(v.Provenance),
				"sample_count": v.SampleCount,
				"confidence":   v.Confidence,
			}
			if v.Unit != "" {
				row["unit"] = v.Unit
			}
			if v.Source != "" {
				row["source"] = v.Source
			}
			if v.SuiteVersion != "" {
				row["suite_version"] = v.SuiteVersion
			}
			if !v.EvaluatedAt.IsZero() {
				row["evaluated_at"] = v.EvaluatedAt.Format(time.RFC3339)
			}
			values = append(values, row)
		}
		present, total := sc.Coverage()
		row := map[string]any{
			"deployment_id":    sc.DeploymentID,
			"provider_id":      sc.ProviderID,
			"model":            sc.Model,
			"version":          sc.Version,
			"generated_at":     sc.GeneratedAt.Format(time.RFC3339),
			"values":           values,
			"quality_coverage": map[string]int{"present": present, "total": total},
		}
		if sc.Evaluation != nil {
			row["evaluation"] = map[string]any{
				"suite_id":      sc.Evaluation.SuiteID,
				"suite_version": sc.Evaluation.SuiteVersion,
				"run_id":        sc.Evaluation.RunID,
				"sample_count":  sc.Evaluation.SampleCount,
				"evaluated_at":  sc.Evaluation.EvaluatedAt.Format(time.RFC3339),
			}
		}
		out = append(out, row)
	}
	return out
}

// scorecardDetail builds the single-deployment view including version history.
func (p *evaluationPlane) scorecardDetail(deploymentID string) (map[string]any, bool) {
	sc, ok := p.reg.Get(deploymentID)
	if !ok {
		return nil, false
	}
	history := []map[string]any{}
	for _, h := range p.reg.History(deploymentID) {
		history = append(history, map[string]any{
			"version":      h.Version,
			"generated_at": h.GeneratedAt.Format(time.RFC3339),
			"values":       len(h.Values),
		})
	}
	rows := p.scorecardRows(0)
	var row map[string]any
	for _, r := range rows {
		if r["deployment_id"] == deploymentID {
			row = r
			break
		}
	}
	if row == nil {
		row = map[string]any{"deployment_id": sc.DeploymentID}
	}
	row["history"] = history
	return row, true
}

// SuiteCatalog returns the deterministic suite catalog (bounded).
func (p *evaluationPlane) SuiteCatalog() []eval.CatalogEntry { return eval.Catalog() }

// provenanceCounts returns provenance-labelled value counts keyed by the
// canonical provenance vocabulary (stable keys, bounded cardinality).
func (p *evaluationPlane) provenanceCounts() map[string]int {
	stats := p.reg.Stats()
	out := make(map[string]int, len(scorecards.AllProvenances))
	for _, prov := range scorecards.AllProvenances {
		out[string(prov)] = stats.ByProvenance[prov]
	}
	return out
}

// provenanceKeys returns the canonical provenance order.
func (p *evaluationPlane) provenanceKeys() []string {
	out := make([]string, 0, len(scorecards.AllProvenances))
	for _, prov := range scorecards.AllProvenances {
		out = append(out, string(prov))
	}
	return out
}

// verdictCounts returns verdict counts keyed by the canonical verdict
// vocabulary.
func (p *evaluationPlane) verdictCounts() map[string]int {
	counts := p.store.Counts()
	out := make(map[string]int, len(eval.AllVerdicts))
	for _, v := range eval.AllVerdicts {
		out[string(v)] = counts[v]
	}
	return out
}

// verdictKeys returns the canonical verdict order.
func (p *evaluationPlane) verdictKeys() []string {
	out := make([]string, 0, len(eval.AllVerdicts))
	for _, v := range eval.AllVerdicts {
		out = append(out, string(v))
	}
	sort.Strings(out)
	return out
}

func logf(l *log.Logger, format string, args ...any) {
	if l == nil {
		return
	}
	l.Printf(format, args...)
}
