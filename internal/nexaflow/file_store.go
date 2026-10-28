package nexaflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileStore is the single-host durable Store. Layout under root:
//
//	meta.json                  {"format_version": N}
//	drafts/<id>.json           mutable working copy
//	versions/<id>/<rev>.json   {"definition": {...}, "version": {...}}
//	runs/<id>.json             run record
//	claims/<key>_<rfc3339>.json
//	deliveries/<sha256>.json
//	schedules/<key>.json
//	audit.jsonl                one AuditEvent per line
//
// Writes are atomic (temp file in the same directory + rename + fsync) and
// all operations are serialized by one mutex, which is what makes claims
// and idempotency keys duplicate-safe inside a single gateway process.
// FileStore makes no cross-process claim: two gateway processes sharing
// one directory are not a supported topology in V1.
type FileStore struct {
	mu   sync.Mutex
	root string
}

// fileVersionRecord is the on-disk wrapper for one published revision.
type fileVersionRecord struct {
	Definition *Definition        `json:"definition"`
	Version    *DefinitionVersion `json:"version"`
}

// NewFileStore creates the directory layout under root. It does not run
// Migrate; callers (engine startup) run Migrate explicitly so an upgrade is
// observable and its result reportable.
func NewFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("nexaflow: file store root must not be empty")
	}
	dirs := []string{
		root,
		filepath.Join(root, "drafts"),
		filepath.Join(root, "versions"),
		filepath.Join(root, "runs"),
		filepath.Join(root, "claims"),
		filepath.Join(root, "deliveries"),
		filepath.Join(root, "schedules"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("nexaflow: cannot create store directory %s: %w", d, err)
		}
	}
	return &FileStore{root: root}, nil
}

type storeMeta struct {
	FormatVersion int `json:"format_version"`
}

func (s *FileStore) writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func writeJSONFile(s *FileStore, path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.writeAtomic(path, append(raw, '\n'))
}

func readJSONFile[T any](path string) (T, error) {
	var v T
	raw, err := os.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, nil
}

func (s *FileStore) draftPath(id string) string { return filepath.Join(s.root, "drafts", id+".json") }

func (s *FileStore) versionPath(definitionID string, revision uint64) string {
	return filepath.Join(s.root, "versions", definitionID, fmt.Sprintf("%d.json", revision))
}

func (s *FileStore) runPath(id string) string { return filepath.Join(s.root, "runs", id+".json") }

// SaveDraft stores the mutable working copy.
func (s *FileStore) SaveDraft(_ context.Context, d *Definition) error {
	if d == nil || d.ID == "" {
		return fmt.Errorf("nexaflow: cannot save draft without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSONFile(s, s.draftPath(d.ID), cloneDefinition(d))
}

// GetDraft loads a draft.
func (s *FileStore) GetDraft(_ context.Context, id string) (*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := readJSONFile[*Definition](s.draftPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &ErrNotFound{Kind: "draft", ID: id}
		}
		return nil, err
	}
	return d, nil
}

// ListDrafts returns drafts in the tenant scope, sorted by id.
func (s *FileStore) ListDrafts(_ context.Context, tenantScope string) ([]*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.root, "drafts"))
	if err != nil {
		return nil, err
	}
	var out []*Definition
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		d, err := readJSONFile[*Definition](filepath.Join(s.root, "drafts", e.Name()))
		if err != nil {
			return nil, err
		}
		if tenantScope == "" || d.TenantScope == tenantScope {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Publish snapshots a draft as a new published revision, superseding any
// previously published revision.
func (s *FileStore) Publish(_ context.Context, draft *Definition, _ string) (*DefinitionVersion, *Definition, error) {
	if draft == nil || draft.ID == "" {
		return nil, nil, fmt.Errorf("nexaflow: cannot publish draft without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, err := ContentHash(draft)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(s.root, "versions", draft.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var next uint64 = 1
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rev, err := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".json"), 10, 64)
		if err != nil {
			continue
		}
		if rev >= next {
			next = rev + 1
		}
		rec, err := readJSONFile[fileVersionRecord](filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		if rec.Version != nil && rec.Version.Status == DefinitionPublished {
			rec.Version.Status = DefinitionSuperseded
			if err := writeJSONFile(s, filepath.Join(dir, e.Name()), rec); err != nil {
				return nil, nil, err
			}
		}
	}
	ver := &DefinitionVersion{DefinitionID: draft.ID, Revision: next, ContentHash: hash, Status: DefinitionPublished}
	content := cloneDefinition(draft)
	rec := fileVersionRecord{Definition: content, Version: ver}
	if err := writeJSONFile(s, s.versionPath(draft.ID, next), rec); err != nil {
		return nil, nil, err
	}
	return cloneVersion(ver), cloneDefinition(content), nil
}

// GetVersion loads one published revision's content and record.
func (s *FileStore) GetVersion(_ context.Context, definitionID string, revision uint64) (*Definition, *DefinitionVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := readJSONFile[fileVersionRecord](s.versionPath(definitionID, revision))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, &ErrNotFound{Kind: "version", ID: fmt.Sprintf("%s@%d", definitionID, revision)}
		}
		return nil, nil, err
	}
	return cloneDefinition(rec.Definition), cloneVersion(rec.Version), nil
}

// ListVersions returns all version records, ascending by revision.
func (s *FileStore) ListVersions(_ context.Context, definitionID string) ([]*DefinitionVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, "versions", definitionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*DefinitionVersion
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := readJSONFile[fileVersionRecord](filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, cloneVersion(rec.Version))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out, nil
}

// LatestPublished returns the currently published revision, if any.
func (s *FileStore) LatestPublished(ctx context.Context, definitionID string) (*Definition, *DefinitionVersion, error) {
	versions, err := s.ListVersions(ctx, definitionID)
	if err != nil {
		return nil, nil, err
	}
	// Re-read content for the published record (ListVersions returns
	// records only).
	for _, v := range versions {
		if v.Status == DefinitionPublished {
			return s.GetVersion(ctx, definitionID, v.Revision)
		}
	}
	return nil, nil, &ErrNotFound{Kind: "published version", ID: definitionID}
}

// ListPublished returns the latest published content of every definition.
func (s *FileStore) ListPublished(_ context.Context) ([]*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := filepath.Join(s.root, "versions")
	defEntries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Definition
	for _, de := range defEntries {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(base, de.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			rec, err := readJSONFile[fileVersionRecord](filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			if rec.Version != nil && rec.Version.Status == DefinitionPublished {
				out = append(out, cloneDefinition(rec.Definition))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CreateRun stores a new run record.
func (s *FileStore) CreateRun(_ context.Context, r *Run) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("nexaflow: cannot create run without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.runPath(r.ID)); err == nil {
		return fmt.Errorf("nexaflow: run %q already exists", r.ID)
	}
	return writeJSONFile(s, s.runPath(r.ID), cloneRun(r))
}

// GetRun loads a run record.
func (s *FileStore) GetRun(_ context.Context, id string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := readJSONFile[*Run](s.runPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &ErrNotFound{Kind: "run", ID: id}
		}
		return nil, err
	}
	return r, nil
}

// ListRuns returns runs matching the filter, newest first.
func (s *FileStore) ListRuns(_ context.Context, filter RunFilter) ([]*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		return nil, err
	}
	var out []*Run
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := readJSONFile[*Run](filepath.Join(s.root, "runs", e.Name()))
		if err != nil {
			return nil, err
		}
		if filter.DefinitionID != "" && r.DefinitionID != filter.DefinitionID {
			continue
		}
		if filter.TenantScope != "" && r.TenantScope != filter.TenantScope {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// UpdateRun persists a run, rejecting a shrinking step history.
func (s *FileStore) UpdateRun(_ context.Context, r *Run) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("nexaflow: cannot update run without id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, err := readJSONFile[*Run](s.runPath(r.ID))
	if err != nil {
		if os.IsNotExist(err) {
			return &ErrNotFound{Kind: "run", ID: r.ID}
		}
		return err
	}
	if len(r.Steps) < len(prev.Steps) {
		return fmt.Errorf("nexaflow: run %q step history must be append-only", r.ID)
	}
	cp := cloneRun(r)
	cp.Version = prev.Version + 1
	if err := writeJSONFile(s, s.runPath(r.ID), cp); err != nil {
		return err
	}
	r.Version = cp.Version
	return nil
}

// FindRunByIdempotency scans run records for the given key.
func (s *FileStore) FindRunByIdempotency(_ context.Context, definitionID, key string) (*Run, error) {
	if key == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := readJSONFile[*Run](filepath.Join(s.root, "runs", e.Name()))
		if err != nil {
			return nil, err
		}
		if r.DefinitionID == definitionID && r.IdempotencyKey == key {
			return r, nil
		}
	}
	return nil, nil
}

// ClaimSchedule returns true only for the first claim of a fire slot.
func (s *FileStore) ClaimSchedule(_ context.Context, scheduleKey string, fireTime time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := scheduleKey + "_" + fireTime.UTC().Format(time.RFC3339) + ".json"
	path := filepath.Join(s.root, "claims", name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	_ = json.NewEncoder(f).Encode(map[string]any{"schedule_key": scheduleKey, "fire_time": fireTime.UTC()})
	_ = f.Sync()
	_ = f.Close()
	return true, nil
}

// RecordDelivery returns true only the first time a hash is seen.
func (s *FileStore) RecordDelivery(_ context.Context, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "deliveries", hash+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	_ = f.Close()
	return true, nil
}

// GetScheduleState loads fire bookkeeping, if any.
func (s *FileStore) GetScheduleState(_ context.Context, scheduleKey string) (*ScheduleState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := readJSONFile[*ScheduleState](filepath.Join(s.root, "schedules", scheduleKey+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return st, nil
}

// SaveScheduleState persists fire bookkeeping.
func (s *FileStore) SaveScheduleState(_ context.Context, st *ScheduleState) error {
	if st == nil || st.ScheduleKey == "" {
		return fmt.Errorf("nexaflow: cannot save schedule state without key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSONFile(s, filepath.Join(s.root, "schedules", st.ScheduleKey+".json"), st)
}

// AppendAudit appends one event line to audit.jsonl.
func (s *FileStore) AppendAudit(_ context.Context, event AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.root, "audit.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ListAudit returns the most recent events first, up to limit.
func (s *FileStore) ListAudit(_ context.Context, limit int) ([]AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.root, "audit.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []AuditEvent
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneRuns deletes terminal runs finished before t.
func (s *FileStore) PruneRuns(_ context.Context, t time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(t, "")
}

func (s *FileStore) pruneLocked(t time.Time, definitionID string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.root, "runs", e.Name())
		r, err := readJSONFile[*Run](path)
		if err != nil {
			return n, err
		}
		if definitionID != "" && r.DefinitionID != definitionID {
			continue
		}
		if r.Status.IsTerminal() && r.FinishedAt != nil && r.FinishedAt.Before(t) {
			if err := os.Remove(path); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// PruneRunsFor deletes terminal runs of one definition finished before t.
func (s *FileStore) PruneRunsFor(_ context.Context, definitionID string, t time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(t, definitionID)
}

// FormatVersion reads the stored format version (1 when no meta exists).
func (s *FileStore) FormatVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readJSONFile[storeMeta](filepath.Join(s.root, "meta.json"))
	if err != nil {
		return 1
	}
	if meta.FormatVersion == 0 {
		return 1
	}
	return meta.FormatVersion
}

// Migrate upgrades stored records to the current format in place. Version 1
// records lack ContentHash on version records and lease/version fields on
// runs; migration fills those defaults without touching definition content
// or run histories, then stamps meta.json. It is a no-op when the stored
// version is already current.
func (s *FileStore) Migrate(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readJSONFile[storeMeta](filepath.Join(s.root, "meta.json"))
	if err == nil && meta.FormatVersion >= CurrentFormatVersion {
		return nil
	}
	vdir := filepath.Join(s.root, "versions")
	if defEntries, err := os.ReadDir(vdir); err == nil {
		for _, de := range defEntries {
			if !de.IsDir() {
				continue
			}
			dir := filepath.Join(vdir, de.Name())
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				path := filepath.Join(dir, e.Name())
				rec, err := readJSONFile[fileVersionRecord](path)
				if err != nil {
					return err
				}
				changed := false
				if rec.Version != nil && rec.Version.ContentHash == "" && rec.Definition != nil {
					hash, err := ContentHash(rec.Definition)
					if err != nil {
						return err
					}
					rec.Version.ContentHash = hash
					changed = true
				}
				if changed {
					if err := writeJSONFile(s, path, rec); err != nil {
						return err
					}
				}
			}
		}
	}
	rdir := filepath.Join(s.root, "runs")
	if entries, err := os.ReadDir(rdir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(rdir, e.Name())
			r, err := readJSONFile[*Run](path)
			if err != nil {
				return err
			}
			if r.Version == 0 {
				r.Version = 1
				if err := writeJSONFile(s, path, r); err != nil {
					return err
				}
			}
		}
	}
	return writeJSONFile(s, filepath.Join(s.root, "meta.json"), storeMeta{FormatVersion: CurrentFormatVersion})
}

// Record implements AuditSink by appending to the audit log.
func (s *FileStore) Record(event AuditEvent) {
	_ = s.AppendAudit(context.Background(), event)
}
