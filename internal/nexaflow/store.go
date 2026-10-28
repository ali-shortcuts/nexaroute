package nexaflow

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Workflow is an operator-owned automation definition. Draft is the
// editable graph; Versions are immutable published snapshots. Runs
// never reference Draft: they snapshot a Version, so editing the canvas
// cannot mutate an active run.
type Workflow struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Enabled        bool      `json:"enabled"`
	Draft          Graph     `json:"draft"`
	Versions       []Version `json:"versions"`
	CreatedBy      string    `json:"created_by"`
	UpdatedBy      string    `json:"updated_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	DraftUpdatedAt time.Time `json:"draft_updated_at"`
}

// Version is one immutable published snapshot of a draft.
// ContentHash is the hex sha256 of the version's graph JSON (the
// Phase 1 definition contract's integrity mechanism, applied to the
// Phase 6 graph shape): two versions with the same graph hash the
// same, and a published version's hash never changes.
type Version struct {
	Number      int       `json:"number"`
	Graph       Graph     `json:"graph"`
	ContentHash string    `json:"content_hash"`
	PublishedBy string    `json:"published_by"`
	PublishedAt time.Time `json:"published_at"`
}

// RunStatus values for runs and nodes.
const (
	RunPending           = "pending"
	RunRunning           = "running"
	RunAwaitingApproval  = "awaiting_approval"
	RunSucceeded         = "succeeded"
	RunFailed            = "failed"
	RunCancelled         = "cancelled"
	NodeSkipped          = "skipped"
	NodeAwaitingApproval = "awaiting_approval"
)

// NodeRun is the per-node execution record. Output holds the full
// node output as returned by Registry.Execute (already secret-redacted
// there, bounded by the node's MaxOutputBytes). It is stored server-side
// only: the HTTP API renders bounded previews from it, never the raw
// map, and the editor never persists it in workflow JSON.
type NodeRun struct {
	NodeID     string         `json:"node_id"`
	Type       string         `json:"type"`
	Status     string         `json:"status"`
	Attempts   int            `json:"attempts"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	DurationMs int64          `json:"duration_ms"`
	Output     map[string]any `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	ErrorCode  string         `json:"error_code,omitempty"`
}

// Run is one execution of a published version. Snapshot is the exact
// graph that executed, copied at start time.
type Run struct {
	ID             string              `json:"id"`
	WorkflowID     string              `json:"workflow_id"`
	WorkflowName   string              `json:"workflow_name"`
	VersionNumber  int                 `json:"version_number"`
	Snapshot       Graph               `json:"snapshot"`
	Status         string              `json:"status"`
	StartedBy      string              `json:"started_by"`
	StartedRoles   []string            `json:"started_roles,omitempty"`
	PolicyApproved bool                `json:"policy_approved"`
	Input          map[string]any      `json:"input,omitempty"`
	Nodes          map[string]*NodeRun `json:"nodes,omitempty"`
	PendingNodeID  string              `json:"pending_node_id,omitempty"`
	Error          string              `json:"error,omitempty"`
	StartedAt      time.Time           `json:"started_at"`
	FinishedAt     *time.Time          `json:"finished_at,omitempty"`
}

// Store limits keep the persisted file bounded.
const (
	MaxWorkflows       = 500
	MaxRunsPerWorkflow = 200
	MaxWorkflowName    = 128
	MaxRunInputBytes   = 256 << 10
	// MaxPreviewBytes bounds one node-output preview rendered by the API.
	MaxPreviewBytes = 16 << 10
)

type storeFile struct {
	Version   string      `json:"version"`
	Workflows []*Workflow `json:"workflows"`
	Runs      []*Run      `json:"runs"`
}

// Store is the durable workflow/run store. A Store with an empty path
// is memory-only (used by tests and by servers started without a
// config file); otherwise it persists atomically to path with mode
// 0600, the same pattern as the gateway config writer.
type Store struct {
	mu        sync.RWMutex
	path      string
	workflows map[string]*Workflow
	runs      map[string]*Run
	runsByWF  map[string][]string // workflow id -> run ids, oldest first
	loaded    bool
}

// NewStore creates a store. Path "" means memory-only.
func NewStore(path string) *Store {
	return &Store{
		path:      path,
		workflows: make(map[string]*Workflow),
		runs:      make(map[string]*Run),
		runsByWF:  make(map[string][]string),
	}
}

func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func (s *Store) loadLocked() error {
	if s.loaded {
		return nil
	}
	s.loaded = true
	if s.path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("nexaflow store corrupt: %w", err)
	}
	if f.Version != "" && f.Version != "nexaflow-store/v1" {
		return fmt.Errorf("nexaflow store version %q unsupported", f.Version)
	}
	for _, w := range f.Workflows {
		if w != nil && w.ID != "" {
			s.workflows[w.ID] = w
		}
	}
	for _, r := range f.Runs {
		if r != nil && r.ID != "" {
			s.runs[r.ID] = r
			s.runsByWF[r.WorkflowID] = append(s.runsByWF[r.WorkflowID], r.ID)
		}
	}
	return nil
}

// Load reads the store file, if any. A corrupt file fails closed: the
// store keeps no data rather than overwriting evidence with empty state.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	f := storeFile{Version: "nexaflow-store/v1"}
	for _, w := range s.workflows {
		f.Workflows = append(f.Workflows, w)
	}
	for _, r := range s.runs {
		f.Runs = append(f.Runs, r)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".nexaflow-store-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
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
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	// Post-rename mode check: a store that is not owner-only is removed
	// rather than left readable.
	if info, err := os.Stat(s.path); err != nil || info.Mode().Perm() != 0o600 {
		_ = os.Remove(s.path)
		return fmt.Errorf("nexaflow store write did not produce mode 0600")
	}
	return nil
}

// ValidateWorkflowName checks a name the operator supplies.
func ValidateWorkflowName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("workflow name is required")
	}
	if len(name) > MaxWorkflowName {
		return fmt.Errorf("workflow name exceeds %d characters", MaxWorkflowName)
	}
	return nil
}

// ListWorkflows returns workflows in creation order.
func (s *Store) ListWorkflows() ([]*Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	out := make([]*Workflow, 0, len(s.workflows))
	for _, w := range s.workflows {
		out = append(out, w)
	}
	// Deterministic order: creation time, then id.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.Before(out[i].CreatedAt) ||
				(out[j].CreatedAt.Equal(out[i].CreatedAt) && out[j].ID < out[i].ID) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// GetWorkflow returns a workflow by id, or nil when absent.
func (s *Store) GetWorkflow(id string) (*Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s.workflows[id], nil
}

// PutWorkflow inserts or replaces a workflow and persists it.
func (s *Store) PutWorkflow(w *Workflow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if _, ok := s.workflows[w.ID]; !ok && len(s.workflows) >= MaxWorkflows {
		return fmt.Errorf("workflow limit %d reached", MaxWorkflows)
	}
	s.workflows[w.ID] = w
	return s.saveLocked()
}

// DeleteWorkflow removes a workflow and all of its runs.
func (s *Store) DeleteWorkflow(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	delete(s.workflows, id)
	for _, rid := range s.runsByWF[id] {
		delete(s.runs, rid)
	}
	delete(s.runsByWF, id)
	return s.saveLocked()
}

// PutRun inserts or replaces a run and persists it, enforcing the
// per-workflow run cap by dropping the oldest runs first.
func (s *Store) PutRun(r *Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if _, ok := s.runs[r.ID]; !ok {
		s.runsByWF[r.WorkflowID] = append(s.runsByWF[r.WorkflowID], r.ID)
		for len(s.runsByWF[r.WorkflowID]) > MaxRunsPerWorkflow {
			oldest := s.runsByWF[r.WorkflowID][0]
			s.runsByWF[r.WorkflowID] = s.runsByWF[r.WorkflowID][1:]
			delete(s.runs, oldest)
		}
	}
	s.runs[r.ID] = r
	return s.saveLocked()
}

// GetRun returns a run by id, or nil when absent.
func (s *Store) GetRun(id string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s.runs[id], nil
}

// ListRuns returns a workflow's runs newest first, up to limit
// (0 = all, capped by MaxRunsPerWorkflow).
func (s *Store) ListRuns(workflowID string, limit int) ([]*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	ids := s.runsByWF[workflowID]
	out := make([]*Run, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if limit > 0 && len(out) >= limit {
			break
		}
		if r := s.runs[ids[i]]; r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// PreviewOutput renders a bounded, redacted preview of a node output
// for the API. It never returns the stored map itself.
func PreviewOutput(output map[string]any) (preview string, truncated bool) {
	if output == nil {
		return "", false
	}
	raw := RedactJSON(output)
	if len(raw) > MaxPreviewBytes {
		return raw[:MaxPreviewBytes], true
	}
	return raw, false
}
