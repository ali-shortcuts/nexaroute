package nexaflow

import (
	"errors"
	"sort"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// storeState is the whole durable dataset of one store, kept as plain
// values so MemoryStore (mutex only) and FileStore (mutex + atomic
// persist of a cloned state) share exactly the same transition logic.
// All methods assume the caller holds the store lock and operate on the
// receiver in place; FileStore applies them to a clone and only swaps
// the clone in after a successful persist, so a persist failure can
// never leave memory ahead of disk.
type storeState struct {
	drafts    map[string]Definition
	revisions map[string][]PublishedDefinition
	runs      map[string]*Run
	runByIdem map[string]string
	nodes     map[string][]*NodeExecution // keyed by run id
	approvals map[string]*Approval        // keyed by runID + "\x00" + nodeID
}

func newStoreState() *storeState {
	return &storeState{
		drafts:    make(map[string]Definition),
		revisions: make(map[string][]PublishedDefinition),
		runs:      make(map[string]*Run),
		runByIdem: make(map[string]string),
		nodes:     make(map[string][]*NodeExecution),
		approvals: make(map[string]*Approval),
	}
}

func (s *storeState) clone() *storeState {
	out := newStoreState()
	for k, v := range s.drafts {
		out.drafts[k] = cloneDefinition(v)
	}
	for k, list := range s.revisions {
		cp := make([]PublishedDefinition, len(list))
		for i, r := range list {
			cp[i] = r
			cp[i].Definition = cloneDefinition(r.Definition)
		}
		out.revisions[k] = cp
	}
	for k, v := range s.runs {
		cp := cloneRun(*v)
		out.runs[k] = &cp
	}
	for k, v := range s.runByIdem {
		out.runByIdem[k] = v
	}
	for k, list := range s.nodes {
		cp := make([]*NodeExecution, len(list))
		for i, e := range list {
			ec := cloneNodeExec(*e)
			cp[i] = &ec
		}
		out.nodes[k] = cp
	}
	for k, v := range s.approvals {
		cp := *v
		out.approvals[k] = &cp
	}
	return out
}

func cloneRun(r Run) Run {
	r.Definition = cloneDefinition(r.Definition)
	r.Input = deepCopyMap(r.Input)
	if r.PrincipalRoles != nil {
		roles := make([]authz.Role, len(r.PrincipalRoles))
		copy(roles, r.PrincipalRoles)
		r.PrincipalRoles = roles
	}
	return r
}

func cloneNodeExec(e NodeExecution) NodeExecution {
	e.Input = deepCopyMap(e.Input)
	e.Output = deepCopyMap(e.Output)
	e.ResumeInput = deepCopyMap(e.ResumeInput)
	return e
}

func approvalKey(runID, nodeID string) string { return runID + "\x00" + nodeID }

func (s *storeState) saveDraft(def Definition) error {
	if def.ID == "" {
		return errors.New("nexaflow: definition id is required")
	}
	s.drafts[def.ID] = cloneDefinition(def)
	return nil
}

func (s *storeState) getDraft(definitionID string) (Definition, error) {
	def, ok := s.drafts[definitionID]
	if !ok {
		return Definition{}, ErrNotFound
	}
	return cloneDefinition(def), nil
}

func (s *storeState) listDrafts() []Definition {
	ids := make([]string, 0, len(s.drafts))
	for id := range s.drafts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Definition, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneDefinition(s.drafts[id]))
	}
	return out
}

func (s *storeState) publish(definitionID, publishedBy string, now time.Time) (PublishedDefinition, error) {
	draft, ok := s.drafts[definitionID]
	if !ok {
		return PublishedDefinition{}, ErrNotFound
	}
	hash, err := ContentHash(&draft)
	if err != nil {
		return PublishedDefinition{}, err
	}
	existing := s.revisions[definitionID]
	if len(existing) > 0 {
		latest := existing[len(existing)-1]
		if latest.Version.Status == DefinitionPublished && latest.Version.ContentHash == hash {
			// Idempotent publish: identical content re-publishes the
			// same revision instead of appending a duplicate.
			return latest, nil
		}
	}
	rev := PublishedDefinition{
		Version: DefinitionVersion{
			DefinitionID: definitionID,
			Revision:     uint64(len(existing) + 1),
			ContentHash:  hash,
			Status:       DefinitionPublished,
		},
		Definition:  cloneDefinition(draft),
		PublishedBy: publishedBy,
		PublishedAt: now,
	}
	// Publishing a new revision supersedes the previous published one.
	// Runs pinned to it keep their snapshot and are unaffected.
	for i := range existing {
		if existing[i].Version.Status == DefinitionPublished {
			existing[i].Version.Status = DefinitionSuperseded
		}
	}
	s.revisions[definitionID] = append(existing, rev)
	return rev, nil
}

func (s *storeState) supersedeRevision(definitionID string, revision uint64) (PublishedDefinition, error) {
	for i, r := range s.revisions[definitionID] {
		if r.Version.Revision != revision {
			continue
		}
		if r.Version.Status != DefinitionPublished {
			return PublishedDefinition{}, ErrStateConflict
		}
		s.revisions[definitionID][i].Version.Status = DefinitionSuperseded
		return s.revisions[definitionID][i], nil
	}
	return PublishedDefinition{}, ErrNotFound
}

func (s *storeState) getRevision(definitionID string, revision uint64) (PublishedDefinition, error) {
	for _, r := range s.revisions[definitionID] {
		if r.Version.Revision == revision {
			cp := r
			cp.Definition = cloneDefinition(r.Definition)
			return cp, nil
		}
	}
	return PublishedDefinition{}, ErrNotFound
}

func (s *storeState) listRevisions(definitionID string) ([]PublishedDefinition, error) {
	list := s.revisions[definitionID]
	out := make([]PublishedDefinition, len(list))
	for i, r := range list {
		out[i] = r
		out[i].Definition = cloneDefinition(r.Definition)
	}
	return out, nil
}

func (s *storeState) createRun(run Run, now time.Time) error {
	if run.ID == "" {
		return errors.New("nexaflow: run id is required")
	}
	if _, ok := s.runs[run.ID]; ok {
		return ErrAlreadyExists
	}
	if run.IdempotencyKey != "" {
		if _, dup := s.runByIdem[run.IdempotencyKey]; dup {
			return ErrIdempotencyConflict
		}
	}
	if run.State == "" {
		run.State = RunCreated
	}
	run.Definition = cloneDefinition(run.Definition)
	run.Input = deepCopyMap(run.Input)
	run.CreatedAt = now
	run.UpdatedAt = now
	cp := run
	s.runs[run.ID] = &cp
	if run.IdempotencyKey != "" {
		s.runByIdem[run.IdempotencyKey] = run.ID
	}
	return nil
}

func (s *storeState) getRun(runID string) (Run, error) {
	r, ok := s.runs[runID]
	if !ok {
		return Run{}, ErrNotFound
	}
	return cloneRun(*r), nil
}

func (s *storeState) getRunByIdempotency(key string) (Run, error) {
	id, ok := s.runByIdem[key]
	if !ok {
		return Run{}, ErrNotFound
	}
	return s.getRun(id)
}

func (s *storeState) updateRunState(runID string, expected RunState, state RunState, errorCode, errMsg string, now time.Time) (Run, error) {
	r, ok := s.runs[runID]
	if !ok {
		return Run{}, ErrNotFound
	}
	if expected != "" && r.State != expected {
		return Run{}, ErrStateConflict
	}
	if r.State.Terminal() && r.State != state {
		return Run{}, ErrTerminalState
	}
	if !validRunTransition(r.State, state) {
		return Run{}, ErrStateConflict
	}
	if r.State != RunRunning && state == RunRunning && r.StartedAt.IsZero() {
		r.StartedAt = now
	}
	if state.Terminal() && r.FinishedAt.IsZero() {
		r.FinishedAt = now
	}
	r.State = state
	r.ErrorCode = errorCode
	r.Error = errMsg
	r.UpdatedAt = now
	return cloneRun(*r), nil
}

func (s *storeState) listRuns(filter RunFilter) ([]Run, error) {
	out := []Run{}
	for _, r := range s.runs {
		if filter.DefinitionID != "" && r.DefinitionID != filter.DefinitionID {
			continue
		}
		if filter.State != "" && r.State != filter.State {
			continue
		}
		out = append(out, cloneRun(*r))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *storeState) prune(before time.Time) (int, error) {
	removed := 0
	for id, r := range s.runs {
		if !r.State.Terminal() || r.FinishedAt.IsZero() || !r.FinishedAt.Before(before) {
			continue
		}
		delete(s.runs, id)
		if r.IdempotencyKey != "" {
			delete(s.runByIdem, r.IdempotencyKey)
		}
		delete(s.nodes, id)
		for key := range s.approvals {
			if len(key) > len(id) && key[:len(id)] == id && key[len(id)] == 0 {
				delete(s.approvals, key)
			}
		}
		removed++
	}
	return removed, nil
}

// currentAttempt returns the highest-attempt row for (run, node).
func (s *storeState) currentAttempt(runID, nodeID string) *NodeExecution {
	var cur *NodeExecution
	for _, e := range s.nodes[runID] {
		if e.NodeID != nodeID {
			continue
		}
		if cur == nil || e.Attempt > cur.Attempt {
			cur = e
		}
	}
	return cur
}

func (s *storeState) appendNodeExecution(exec NodeExecution, now time.Time) (NodeExecution, error) {
	if _, ok := s.runs[exec.RunID]; !ok {
		return NodeExecution{}, ErrNotFound
	}
	attempt := 0
	if cur := s.currentAttempt(exec.RunID, exec.NodeID); cur != nil {
		attempt = cur.Attempt + 1
	}
	exec.Attempt = attempt
	exec.IdempotencyKey = NodeIDKey(exec.RunID, exec.NodeID, attempt)
	exec.Input = deepCopyMap(exec.Input)
	exec.Output = deepCopyMap(exec.Output)
	exec.ResumeInput = deepCopyMap(exec.ResumeInput)
	exec.CreatedAt = now
	exec.UpdatedAt = now
	if exec.State == "" {
		exec.State = NodePending
	}
	cp := exec
	s.nodes[exec.RunID] = append(s.nodes[exec.RunID], &cp)
	return cloneNodeExec(cp), nil
}

func (s *storeState) saveNodeExecution(exec NodeExecution, expected NodeState, now time.Time) (NodeExecution, error) {
	cur := s.currentAttempt(exec.RunID, exec.NodeID)
	if cur == nil {
		return NodeExecution{}, ErrNotFound
	}
	if expected != "" && cur.State != expected {
		return NodeExecution{}, ErrStateConflict
	}
	if cur.State.Terminal() && cur.State != exec.State {
		return NodeExecution{}, ErrTerminalState
	}
	if !validNodeTransition(cur.State, exec.State) {
		return NodeExecution{}, ErrStateConflict
	}
	if exec.Attempt != cur.Attempt {
		return NodeExecution{}, ErrStateConflict
	}
	exec.IdempotencyKey = cur.IdempotencyKey
	exec.CreatedAt = cur.CreatedAt
	exec.Input = deepCopyMap(exec.Input)
	exec.Output = deepCopyMap(exec.Output)
	exec.ResumeInput = deepCopyMap(exec.ResumeInput)
	if exec.State.Terminal() {
		exec.LeaseOwner = ""
		exec.LeaseUntil = time.Time{}
	}
	if cur.State != NodeRunning && exec.State == NodeRunning && exec.StartedAt.IsZero() {
		exec.StartedAt = now
	}
	if exec.State.Terminal() && exec.FinishedAt.IsZero() {
		exec.FinishedAt = now
	}
	exec.UpdatedAt = now
	*cur = exec
	return cloneNodeExec(*cur), nil
}

func (s *storeState) getNodeExecutions(runID string) ([]NodeExecution, error) {
	if _, ok := s.runs[runID]; !ok {
		return nil, ErrNotFound
	}
	list := s.nodes[runID]
	out := make([]NodeExecution, len(list))
	for i, e := range list {
		out[i] = cloneNodeExec(*e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].Attempt < out[j].Attempt
	})
	return out, nil
}

func (s *storeState) tryClaim(runID, nodeID, owner string, ttl time.Duration, now time.Time) (NodeExecution, bool, error) {
	cur := s.currentAttempt(runID, nodeID)
	if cur == nil {
		return NodeExecution{}, false, ErrNotFound
	}
	if cur.State != NodePending && cur.State != NodeClaimed {
		return NodeExecution{}, false, nil
	}
	if cur.LeaseOwner != "" && cur.LeaseOwner != owner && !cur.LeaseUntil.IsZero() && cur.LeaseUntil.After(now) {
		return NodeExecution{}, false, nil
	}
	cur.State = NodeClaimed
	cur.LeaseOwner = owner
	cur.LeaseUntil = now.Add(ttl)
	cur.UpdatedAt = now
	return cloneNodeExec(*cur), true, nil
}

func (s *storeState) releaseLease(runID, nodeID, owner string) error {
	cur := s.currentAttempt(runID, nodeID)
	if cur == nil {
		return ErrNotFound
	}
	if cur.LeaseOwner == owner {
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
	}
	return nil
}

func (s *storeState) recordApproval(appr Approval, now time.Time) (Approval, error) {
	if appr.Decision != "approved" && appr.Decision != "rejected" {
		return Approval{}, errors.New("nexaflow: approval decision must be approved or rejected")
	}
	if appr.Approver != "" && appr.Requester != "" && appr.Approver == appr.Requester {
		return Approval{}, errors.New("nexaflow: approver must not be the requester")
	}
	key := approvalKey(appr.RunID, appr.NodeID)
	if prev, ok := s.approvals[key]; ok {
		if prev.Decision == appr.Decision && prev.Approver == appr.Approver {
			return *prev, nil
		}
		return Approval{}, ErrApprovalConflict
	}
	appr.DecidedAt = now
	cp := appr
	s.approvals[key] = &cp
	return cp, nil
}

func (s *storeState) getApproval(runID, nodeID string) (Approval, error) {
	appr, ok := s.approvals[approvalKey(runID, nodeID)]
	if !ok {
		return Approval{}, ErrNoApprovalRecord
	}
	return *appr, nil
}
