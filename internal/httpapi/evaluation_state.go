package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// The evaluation plane owns durability: it mirrors evaluation runs *and* the
// scorecards they produced into one bounded, 0600, atomically written state file.
// The state file is optional (evaluation.state_path) and best-effort: a write
// failure is reported and never breaks a run.
const (
	evaluationStateVersion  = 1
	maxEvaluationStateBytes = 8 << 20
)

type evaluationState struct {
	Version    int                    `json:"version"`
	Runs       []eval.Result          `json:"runs"`
	Scorecards []scorecards.Scorecard `json:"scorecards,omitempty"`
}

// loadState reads the configured state file into the plane. A malformed state
// file is reported and rejected as a whole: partially trusted evidence is worse
// than none.
func (p *evaluationPlane) loadState(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > maxEvaluationStateBytes {
		return fmt.Errorf("evaluation state exceeds %d bytes", maxEvaluationStateBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var doc evaluationState
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("invalid evaluation state: %w", err)
	}
	if doc.Version != evaluationStateVersion {
		return fmt.Errorf("unsupported evaluation state version %d", doc.Version)
	}
	if len(doc.Runs) > eval.MaxStoredRuns {
		return fmt.Errorf("evaluation state exceeds %d runs", eval.MaxStoredRuns)
	}
	if len(doc.Scorecards) > scorecards.MaxDeployments {
		return fmt.Errorf("evaluation state exceeds %d scorecards", scorecards.MaxDeployments)
	}
	// Validate everything before mutating live state.
	for _, run := range doc.Runs {
		if err := run.Validate(); err != nil {
			return fmt.Errorf("invalid stored run %q: %w", run.RunID, err)
		}
	}
	for i := range doc.Scorecards {
		if doc.Scorecards[i].Version < 1 {
			doc.Scorecards[i].Version = 1
		}
		if doc.Scorecards[i].GeneratedAt.IsZero() {
			return fmt.Errorf("stored scorecard %q has no generated_at", doc.Scorecards[i].DeploymentID)
		}
		if err := doc.Scorecards[i].Validate(); err != nil {
			return fmt.Errorf("stored scorecard %q: %w", doc.Scorecards[i].DeploymentID, err)
		}
	}
	// Runs first (oldest -> newest ordering is preserved by the store), then
	// scorecards in their recorded order.
	for _, run := range doc.Runs {
		if err := p.store.Save(run); err != nil {
			return err
		}
	}
	if len(doc.Scorecards) > 0 {
		if err := p.reg.Load(doc.Scorecards); err != nil {
			return err
		}
	}
	return nil
}

// persist writes the plane state atomically (temp file in the same directory,
// fsync, rename, 0600). It never panics and never returns an error that a caller
// must act on: a run is valid whether or not it could be written to disk.
func (p *evaluationPlane) persist() error {
	cfg := p.config()
	path := strings.TrimSpace(cfg.StatePath)
	if path == "" {
		return nil
	}
	doc := evaluationState{
		Version:    evaluationStateVersion,
		Runs:       p.store.All(),
		Scorecards: p.reg.Snapshot(),
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxEvaluationStateBytes {
		// Trim the oldest runs until the document fits; scorecards are never
		// dropped here (they are the durable product of the plane).
		for len(doc.Runs) > 1 && len(data) > maxEvaluationStateBytes {
			doc.Runs = doc.Runs[1:]
			data, err = json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return err
			}
		}
		if len(data) > maxEvaluationStateBytes {
			return errors.New("evaluation state exceeds safe size")
		}
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".evaluation-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// persistBestEffort writes state and records a failure for observability.
func (p *evaluationPlane) persistBestEffort() {
	if err := p.persist(); err != nil {
		p.stateWritesFailed.Add(1)
		p.setImportError("evaluation state write failed: " + err.Error())
	}
}

// stateModTime returns the state file modification time, used by the admin
// surface to show freshness (zero when unset or missing).
func (p *evaluationPlane) stateModTime() time.Time {
	path := strings.TrimSpace(p.config().StatePath)
	if path == "" {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}
