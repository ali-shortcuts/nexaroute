package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Task 8 migration: #691 directory file store -> canonical (#689) store.
//
// Source layout (read-only, never modified): drafts/<id>.json,
// versions/<id>/<revision>.json ({definition, version}), runs/<id>.json.
// The migration is two-phase: phase 1 parses and validates EVERYTHING
// (definitions, versions, runs) before a single write; any corrupt
// file, unknown extension, hash mismatch, or unrepresentable run
// aborts the whole migration with zero writes. Phase-2 failures can
// only be I/O errors, and are reported with what was written.
//
// What migrates, and what loudly does not:
//   - Drafts and versions migrate when they validate against the
//     canonical registry and their content hash recomputes exactly.
//     Revision numbers are reproduced by publishing in ascending
//     order into a destination that holds no revisions for that
//     definition; gaps, identical consecutive revisions (the
//     canonical Publish de-duplicates identical latest hashes), or
//     a final-state mismatch abort instead of renumbering silently.
//   - Definitions using #691-only extensions (top-level on_error, a
//     node for_each, or settings policy_approved) are REJECTED, never
//     imported with the extension silently dropped: dropping an error
//     handler or a policy approval would change execution semantics.
//   - Only terminal runs (succeeded/failed/cancelled) migrate, as
//     history. In-flight runs (pending/running/awaiting_approval) and
//     parked runs block the migration by id: their future depends on
//     the #691 executor and cannot be resumed faithfully by the
//     canonical engine. Resolve them in the source first.
//   - Steps with Iteration != 0, non-contiguous attempts, unknown
//     node ids, or waiting/ambiguous steps inside a terminal run
//     abort the migration: the canonical model cannot represent them.
//   - Legacy run inputs and outputs pass through the canonical
//     redactor before storage (#691 stored run inputs unredacted);
//     the report counts how many runs were redacted, so the operator
//     can see that history was scrubbed, not silently kept raw.

// MigrationReport summarises a completed migration.
type MigrationReport struct {
	Drafts          int
	Versions        int
	Runs            int
	RedactedInputs  int
	RedactedOutputs int
	BlockedRuns     []string
}

// legacyVersionRecord mirrors a #691 versions/<id>/<rev>.json file.
type legacyVersionRecord struct {
	Definition *Definition        `json:"definition"`
	Version    *DefinitionVersion `json:"version"`
}

// legacyRun mirrors a #691 runs/<id>.json file (subset used here).
type legacyRun struct {
	ID             string         `json:"id"`
	DefinitionID   string         `json:"definition_id"`
	Revision       uint64         `json:"revision"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Actor          string         `json:"actor"`
	Status         string         `json:"status"`
	Input          map[string]any `json:"input,omitempty"`
	Steps          []legacyStep   `json:"steps,omitempty"`
	Error          *NodeError     `json:"error,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty"`
}

type legacyStep struct {
	NodeID    string         `json:"node_id"`
	Attempt   int            `json:"attempt"`
	Iteration int            `json:"iteration"`
	Status    string         `json:"status"`
	Input     map[string]any `json:"input,omitempty"`
	Output    map[string]any `json:"output,omitempty"`
	Error     *NodeError     `json:"error,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	Finished  *time.Time     `json:"finished_at,omitempty"`
}

func checkLegacyDefinition(raw map[string]any, file string) error {
	if _, ok := raw["on_error"]; ok {
		return fmt.Errorf("nexaflow: migration %s: definition uses on_error, which the canonical engine does not support; refusing to drop it silently", file)
	}
	if sv, ok := raw["schema_version"]; !ok || sv != DefinitionSchemaVersion {
		return fmt.Errorf("nexaflow: migration %s: schema_version %v, want %q", file, sv, DefinitionSchemaVersion)
	}
	nodes, _ := raw["nodes"].([]any)
	for _, n := range nodes {
		nm, _ := n.(map[string]any)
		if nm == nil {
			continue
		}
		if _, ok := nm["for_each"]; ok {
			return fmt.Errorf("nexaflow: migration %s: node %v uses for_each, which the canonical engine does not support; refusing to drop it silently", file, nm["id"])
		}
	}
	if settings, ok := raw["settings"].(map[string]any); ok {
		if pa, ok := settings["policy_approved"].(bool); ok && pa {
			return fmt.Errorf("nexaflow: migration %s: definition carries policy_approved, which the canonical engine does not support; refusing to drop it silently", file)
		}
	}
	return nil
}

func readLegacyJSON(path string, v any) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("nexaflow: migration: read %s: %w", path, err)
	}
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		return nil, fmt.Errorf("nexaflow: migration: corrupt JSON in %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("nexaflow: migration: corrupt JSON in %s: %w", path, err)
	}
	return rawMap, nil
}

func mapRunState(status string) (RunState, bool) {
	switch status {
	case "succeeded":
		return RunSucceeded, true
	case "failed":
		return RunFailed, true
	case "cancelled":
		return RunCancelled, true
	default:
		return "", false
	}
}

func mapStepState(status string) (NodeState, bool) {
	switch status {
	case "pending":
		return NodePending, true
	case "running":
		return NodeRunning, true
	case "succeeded":
		return NodeSucceeded, true
	case "failed":
		return NodeFailed, true
	case "skipped":
		return NodeSkipped, true
	case "cancelled":
		return NodeCancelled, true
	default:
		return "", false
	}
}

func jsonChanged(before, after any) bool {
	b, err1 := json.Marshal(before)
	a, err2 := json.Marshal(after)
	return err1 == nil && err2 == nil && string(b) != string(a)
}

type migrationPlan struct {
	drafts   []Definition
	versions map[string][]legacyVersionRecord // definition id -> ascending
	runs     []legacyRun
	pinned   map[string]Definition // definition id + "@" + revision -> definition
	redIn    int
	redOut   int
}

// MigratePhase8Dir migrates a #691 directory file store into dst.
// Phase 1 validates everything; phase 2 writes. See the file header
// for the exact accept/reject rules.
func MigratePhase8Dir(ctx context.Context, srcRoot string, dst Store, reg *Registry) (MigrationReport, error) {
	if reg == nil {
		reg = DefaultRegistry()
	}
	plan, blocked, err := planMigration(srcRoot, reg)
	if err != nil {
		return MigrationReport{}, err
	}
	if len(blocked) > 0 {
		return MigrationReport{}, fmt.Errorf("nexaflow: migration blocked by non-terminal runs (resolve them in the source first): %s", strings.Join(blocked, ", "))
	}
	// Destination must not already hold these definitions or runs:
	// migration never overwrites.
	for _, d := range plan.drafts {
		if _, err := dst.GetDraft(ctx, d.ID); err == nil {
			return MigrationReport{}, fmt.Errorf("nexaflow: migration: destination already holds draft %q", d.ID)
		} else if !errors.Is(err, ErrNotFound) {
			return MigrationReport{}, err
		}
	}
	for _, r := range plan.runs {
		if _, err := dst.GetRun(ctx, r.ID); err == nil {
			return MigrationReport{}, fmt.Errorf("nexaflow: migration: destination already holds run %q", r.ID)
		} else if !errors.Is(err, ErrNotFound) {
			return MigrationReport{}, err
		}
	}
	for id := range plan.versions {
		revs, err := dst.ListRevisions(ctx, id)
		if err != nil {
			return MigrationReport{}, err
		}
		if len(revs) > 0 {
			return MigrationReport{}, fmt.Errorf("nexaflow: migration: destination already holds revisions for %q", id)
		}
	}

	rep := MigrationReport{RedactedInputs: plan.redIn, RedactedOutputs: plan.redOut}
	for _, d := range plan.drafts {
		if err := dst.SaveDraft(ctx, d); err != nil {
			return rep, fmt.Errorf("nexaflow: migration: save draft %q: %w", d.ID, err)
		}
		rep.Drafts++
	}
	ids := make([]string, 0, len(plan.versions))
	for id := range plan.versions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		recs := plan.versions[id]
		for i, rec := range recs {
			// Save the exact snapshot as the draft, then publish.
			// Publish de-duplicates an identical latest hash, so a
			// source with identical consecutive revisions is rejected
			// in phase 1 rather than renumbered here.
			if err := dst.SaveDraft(ctx, *rec.Definition); err != nil {
				return rep, fmt.Errorf("nexaflow: migration: save version draft %s@%d: %w", id, rec.Version.Revision, err)
			}
			pub, err := dst.Publish(ctx, id, "migration")
			if err != nil {
				return rep, fmt.Errorf("nexaflow: migration: publish %s@%d: %w", id, rec.Version.Revision, err)
			}
			if pub.Version.Revision != rec.Version.Revision {
				return rep, fmt.Errorf("nexaflow: migration: publish %s produced revision %d, want %d (source revisions cannot be reproduced)", id, pub.Version.Revision, rec.Version.Revision)
			}
			_ = i
			rep.Versions++
		}
		// Reproduce final superseded states: publishing in order
		// leaves only the last published; supersede the last as well
		// when the source says so.
		last := recs[len(recs)-1]
		if last.Version.Status == DefinitionSuperseded {
			if _, err := dst.SupersedeRevision(ctx, id, last.Version.Revision); err != nil {
				return rep, fmt.Errorf("nexaflow: migration: supersede %s@%d: %w", id, last.Version.Revision, err)
			}
		}
	}
	for _, r := range plan.runs {
		if err := writeMigratedRun(ctx, dst, plan, r); err != nil {
			return rep, err
		}
		rep.Runs++
	}
	return rep, nil
}

func planMigration(srcRoot string, reg *Registry) (*migrationPlan, []string, error) {
	plan := &migrationPlan{versions: map[string][]legacyVersionRecord{}, pinned: map[string]Definition{}}
	draftDir := filepath.Join(srcRoot, "drafts")
	if entries, err := os.ReadDir(draftDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(draftDir, e.Name())
			var def Definition
			raw, err := readLegacyJSON(path, &def)
			if err != nil {
				return nil, nil, err
			}
			if err := checkLegacyDefinition(raw, path); err != nil {
				return nil, nil, err
			}
			if err := ValidateDefinition(&def, reg); err != nil {
				return nil, nil, fmt.Errorf("nexaflow: migration %s: %w", path, err)
			}
			plan.drafts = append(plan.drafts, def)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	sort.Slice(plan.drafts, func(i, j int) bool { return plan.drafts[i].ID < plan.drafts[j].ID })

	verRoot := filepath.Join(srcRoot, "versions")
	if defDirs, err := os.ReadDir(verRoot); err == nil {
		for _, dd := range defDirs {
			if !dd.IsDir() {
				continue
			}
			id := dd.Name()
			dir := filepath.Join(verRoot, id)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, nil, err
			}
			var recs []legacyVersionRecord
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				rev, err := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".json"), 10, 64)
				if err != nil {
					return nil, nil, fmt.Errorf("nexaflow: migration: non-numeric version file %s", filepath.Join(dir, e.Name()))
				}
				path := filepath.Join(dir, e.Name())
				var rec legacyVersionRecord
				raw, err := readLegacyJSON(path, &rec)
				if err != nil {
					return nil, nil, err
				}
				if rec.Definition == nil || rec.Version == nil {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: missing definition or version record", path)
				}
				defRaw, _ := raw["definition"].(map[string]any)
				if defRaw != nil {
					if err := checkLegacyDefinition(defRaw, path); err != nil {
						return nil, nil, err
					}
				}
				if rec.Version.DefinitionID != id || rec.Version.Revision != rev {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: version record %s@%d does not match its path", path, rec.Version.DefinitionID, rec.Version.Revision)
				}
				if rec.Version.Status != DefinitionPublished && rec.Version.Status != DefinitionSuperseded {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: unknown version status %q", path, rec.Version.Status)
				}
				if err := ValidateDefinition(rec.Definition, reg); err != nil {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: %w", path, err)
				}
				hash, err := ContentHash(rec.Definition)
				if err != nil {
					return nil, nil, err
				}
				if hash != rec.Version.ContentHash {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: content hash mismatch (stored %q, recomputed %q)", path, rec.Version.ContentHash, hash)
				}
				recs = append(recs, rec)
			}
			sort.Slice(recs, func(i, j int) bool { return recs[i].Version.Revision < recs[j].Version.Revision })
			for i, rec := range recs {
				if want := uint64(i + 1); rec.Version.Revision != want {
					return nil, nil, fmt.Errorf("nexaflow: migration: definition %q has revision gap at %d (want %d); refusing to renumber", id, rec.Version.Revision, want)
				}
				if i > 0 && rec.Version.ContentHash == recs[i-1].Version.ContentHash {
					return nil, nil, fmt.Errorf("nexaflow: migration: definition %q revisions %d and %d are identical; canonical publish de-duplicates and cannot reproduce both", id, recs[i-1].Version.Revision, rec.Version.Revision)
				}
				plan.pinned[fmt.Sprintf("%s@%d", id, rec.Version.Revision)] = *rec.Definition
			}
			// Final-state rule: exactly the last revision may be
			// published; all earlier ones must be superseded.
			for i, rec := range recs {
				last := i == len(recs)-1
				if last && rec.Version.Status != DefinitionPublished && rec.Version.Status != DefinitionSuperseded {
					return nil, nil, fmt.Errorf("nexaflow: migration: definition %q: unexpected final status %q", id, rec.Version.Status)
				}
				if !last && rec.Version.Status != DefinitionSuperseded {
					return nil, nil, fmt.Errorf("nexaflow: migration: definition %q revision %d is %q, want superseded (only the last revision may be published)", id, rec.Version.Revision, rec.Version.Status)
				}
			}
			plan.versions[id] = recs
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}

	runDir := filepath.Join(srcRoot, "runs")
	var blocked []string
	if entries, err := os.ReadDir(runDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(runDir, e.Name())
			var r legacyRun
			if _, err := readLegacyJSON(path, &r); err != nil {
				return nil, nil, err
			}
			state, ok := mapRunState(r.Status)
			if !ok {
				blocked = append(blocked, r.ID)
				continue
			}
			_ = state
			pinned, ok := plan.pinned[fmt.Sprintf("%s@%d", r.DefinitionID, r.Revision)]
			if !ok {
				return nil, nil, fmt.Errorf("nexaflow: migration %s: run %q references unknown version %s@%d", path, r.ID, r.DefinitionID, r.Revision)
			}
			nodeIDs := map[string]bool{}
			for _, n := range pinned.Nodes {
				nodeIDs[n.ID] = true
			}
			seenAttempt := map[string]int{}
			for _, st := range r.Steps {
				if !nodeIDs[st.NodeID] {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: run %q step references unknown node %q", path, r.ID, st.NodeID)
				}
				if st.Iteration != 0 {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: run %q step %q has iteration %d, which the canonical model cannot represent", path, r.ID, st.NodeID, st.Iteration)
				}
				if _, ok := mapStepState(st.Status); !ok {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: run %q step %q has unrepresentable status %q", path, r.ID, st.NodeID, st.Status)
				}
				if want := seenAttempt[st.NodeID]; st.Attempt != want {
					return nil, nil, fmt.Errorf("nexaflow: migration %s: run %q step %q has non-contiguous attempt %d (want %d)", path, r.ID, st.NodeID, st.Attempt, want)
				}
				seenAttempt[st.NodeID]++
			}
			plan.runs = append(plan.runs, r)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	sort.Slice(plan.runs, func(i, j int) bool { return plan.runs[i].ID < plan.runs[j].ID })
	// Redaction counts are computed at write time from stored values;
	// phase 1 records that inputs/outputs will pass the redactor.
	for _, r := range plan.runs {
		if red, _ := redactJSONValue(deepCopyValue(r.Input)).(map[string]any); red != nil && jsonChanged(r.Input, red) {
			plan.redIn++
		}
		for _, st := range r.Steps {
			if red, _ := redactJSONValue(deepCopyValue(st.Output)).(map[string]any); red != nil && jsonChanged(st.Output, red) {
				plan.redOut++
				break
			}
		}
	}
	return plan, blocked, nil
}

func writeMigratedRun(ctx context.Context, dst Store, plan *migrationPlan, r legacyRun) error {
	pinned := plan.pinned[fmt.Sprintf("%s@%d", r.DefinitionID, r.Revision)]
	state, _ := mapRunState(r.Status)
	input, _ := redactJSONValue(deepCopyValue(r.Input)).(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	run := Run{
		ID:             r.ID,
		DefinitionID:   r.DefinitionID,
		Revision:       r.Revision,
		Definition:     pinned,
		State:          state,
		IdempotencyKey: r.IdempotencyKey,
		Principal:      r.Actor,
		Input:          input,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if r.FinishedAt != nil {
		run.FinishedAt = *r.FinishedAt
	}
	if r.Error != nil {
		run.ErrorCode = r.Error.Code
		run.Error = r.Error.Message
	}
	if err := dst.CreateRun(ctx, run); err != nil {
		return fmt.Errorf("nexaflow: migration: create run %q: %w", r.ID, err)
	}
	// Steps were validated ascending per node in phase 1, so appending
	// in file order reproduces attempt numbers 0..N per node.
	steps := append([]legacyStep{}, r.Steps...)
	sort.SliceStable(steps, func(i, j int) bool {
		if steps[i].NodeID != steps[j].NodeID {
			return steps[i].NodeID < steps[j].NodeID
		}
		return steps[i].Attempt < steps[j].Attempt
	})
	for _, st := range steps {
		stState, _ := mapStepState(st.Status)
		out, truncated := sanitizeNodeOutput(st.Output)
		exec := NodeExecution{
			RunID:           r.ID,
			NodeID:          st.NodeID,
			State:           stState,
			Input:           st.Input,
			Output:          out,
			OutputTruncated: truncated,
			CreatedAt:       st.StartedAt,
			UpdatedAt:       st.StartedAt,
			StartedAt:       st.StartedAt,
		}
		if st.Finished != nil {
			exec.FinishedAt = *st.Finished
			exec.UpdatedAt = *st.Finished
		}
		if st.Error != nil {
			exec.ErrorCode = st.Error.Code
			exec.Error = st.Error.Message
		}
		if _, err := dst.AppendNodeExecution(ctx, exec); err != nil {
			return fmt.Errorf("nexaflow: migration: append step %q for run %q: %w", st.NodeID, r.ID, err)
		}
	}
	return nil
}
