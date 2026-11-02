package nexaflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func assertUnavailableFileStore(t *testing.T, s *FileStore, want error) {
	t.Helper()
	ctx := context.Background()
	checks := map[string]func() error{
		"SaveDraft":           func() error { return s.SaveDraft(ctx, Definition{ID: "x"}) },
		"GetDraft":            func() error { _, err := s.GetDraft(ctx, "x"); return err },
		"ListDrafts":          func() error { _, err := s.ListDrafts(ctx); return err },
		"Publish":             func() error { _, err := s.Publish(ctx, "x", "x"); return err },
		"PublishIfUnchanged":  func() error { _, err := s.PublishIfUnchanged(ctx, "x", "x", ""); return err },
		"SupersedeRevision":   func() error { _, err := s.SupersedeRevision(ctx, "x", 1); return err },
		"GetRevision":         func() error { _, err := s.GetRevision(ctx, "x", 1); return err },
		"ListRevisions":       func() error { _, err := s.ListRevisions(ctx, "x"); return err },
		"CreateRun":           func() error { return s.CreateRun(ctx, Run{}) },
		"GetRun":              func() error { _, err := s.GetRun(ctx, "x"); return err },
		"GetRunByIdempotency": func() error { _, err := s.GetRunByIdempotency(ctx, "x"); return err },
		"UpdateRunState":      func() error { _, err := s.UpdateRunState(ctx, "x", "", "", "", ""); return err },
		"ListRuns":            func() error { _, err := s.ListRuns(ctx, RunFilter{}); return err },
		"Prune":               func() error { _, err := s.Prune(ctx, time.Time{}); return err },
		"AppendNodeExecution": func() error { _, err := s.AppendNodeExecution(ctx, NodeExecution{}); return err },
		"SaveNodeExecution":   func() error { _, err := s.SaveNodeExecution(ctx, NodeExecution{}, ""); return err },
		"GetNodeExecutions":   func() error { _, err := s.GetNodeExecutions(ctx, "x"); return err },
		"TryClaim":            func() error { _, _, err := s.TryClaim(ctx, "x", "x", "x", time.Second, time.Now()); return err },
		"ReleaseLease":        func() error { return s.ReleaseLease(ctx, "x", "x", "x") },
		"RecordApproval":      func() error { _, err := s.RecordApproval(ctx, Approval{}); return err },
		"GetApproval":         func() error { _, err := s.GetApproval(ctx, "x", "x"); return err },
	}
	for name, check := range checks {
		if err := check(); !errors.Is(err, want) {
			t.Errorf("%s returned %v, want %v", name, err, want)
		}
	}
}

func TestFileStoreRejectsEveryOperationWhenUnavailable(t *testing.T) {
	s, err := OpenFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	injectFileStoreFailure(s, "dir-sync")
	if err := s.SaveDraft(context.Background(), Definition{ID: "uncertain"}); !errors.Is(err, ErrFileStoreCommitUncertain) {
		t.Fatalf("expected uncertain commit: %v", err)
	}
	assertUnavailableFileStore(t, s, ErrFileStoreCommitUncertain)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertUnavailableFileStore(t, s, os.ErrClosed)
}

func TestFileStoreCloseReleaseErrorFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	release := s.fs.releaseLock
	s.fs.releaseLock = func(f *os.File) error {
		if err := release(f); err != nil {
			return err
		}
		return errInjectedFileStore
	}
	if err := s.Close(); !errors.Is(err, errInjectedFileStore) {
		t.Fatalf("Close: got %v, expected release error", err)
	}
	assertUnavailableFileStore(t, s, os.ErrClosed)
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen after actual unlock: %v", err)
	}
	defer reopened.Close()
}
