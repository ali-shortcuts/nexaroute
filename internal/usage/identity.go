package usage

import (
	"sort"
	"sync"
)

// IdentityTotals is a privacy-safe aggregate; it never stores prompt bodies or
// credentials. Empty identity fields represent legacy unauthenticated traffic.
type IdentityTotals struct {
	Identity  string `json:"identity"`
	TenantID  string `json:"tenant_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
	Requests  int64  `json:"requests"`
	Input     int64  `json:"input_tokens"`
	Output    int64  `json:"output_tokens"`
}

type IdentityTracker struct {
	mu   sync.Mutex
	rows map[string]*IdentityTotals
}

func NewIdentityTracker() *IdentityTracker {
	return &IdentityTracker{rows: map[string]*IdentityTotals{}}
}

func (t *IdentityTracker) Record(identity, tenant, project, team string, input, output int64) {
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	if input == 0 && output == 0 {
		return
	}
	if identity == "" {
		identity = "anonymous"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.rows[identity]
	if row == nil {
		row = &IdentityTotals{Identity: identity, TenantID: tenant, ProjectID: project, TeamID: team}
		t.rows[identity] = row
	}
	row.Input += input
	row.Output += output
	row.Requests++
}

func (t *IdentityTracker) Snapshot() []IdentityTotals {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]IdentityTotals, 0, len(t.rows))
	for _, row := range t.rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

func (t *IdentityTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rows = map[string]*IdentityTotals{}
}
