package queue

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errJobFault = errors.New("injected file failure")

type faultJobTemp struct {
	jobStoreTemp
	stage string
}

func (f faultJobTemp) Chmod(mode os.FileMode) error {
	if f.stage == "chmod" {
		return errJobFault
	}
	return f.jobStoreTemp.Chmod(mode)
}
func (f faultJobTemp) Write(data []byte) (int, error) {
	if f.stage == "write" {
		return 0, errJobFault
	}
	if f.stage == "short" {
		return f.jobStoreTemp.Write(data[:len(data)-1])
	}
	return f.jobStoreTemp.Write(data)
}
func (f faultJobTemp) Sync() error {
	if f.stage == "file-sync" {
		return errJobFault
	}
	return f.jobStoreTemp.Sync()
}
func (f faultJobTemp) Close() error {
	err := f.jobStoreTemp.Close()
	if f.stage == "file-close" {
		return errJobFault
	}
	return err
}

type faultJobDir struct {
	jobStoreDir
	stage string
}

func (f faultJobDir) Sync() error {
	if f.stage == "dir-sync" {
		return errJobFault
	}
	return f.jobStoreDir.Sync()
}
func (f faultJobDir) Close() error {
	err := f.jobStoreDir.Close()
	if f.stage == "dir-close" {
		return errJobFault
	}
	return err
}
func injectJobFS(s *FileStore, stage string) {
	fs := defaultJobStoreFS()
	mkdir, create, rename, open := fs.mkdirAll, fs.createTemp, fs.rename, fs.openDir
	fs.mkdirAll = func(path string, mode os.FileMode) error {
		if stage == "mkdir" {
			return errJobFault
		}
		return mkdir(path, mode)
	}
	fs.createTemp = func(dir, pattern string) (jobStoreTemp, error) {
		if stage == "create-temp" {
			return nil, errJobFault
		}
		f, err := create(dir, pattern)
		if err != nil {
			return nil, err
		}
		return faultJobTemp{f, stage}, nil
	}
	fs.rename = func(a, b string) error {
		if stage == "rename" {
			return errJobFault
		}
		return rename(a, b)
	}
	fs.openDir = func(path string) (jobStoreDir, error) {
		if stage == "dir-open" {
			return nil, errJobFault
		}
		d, err := open(path)
		if err != nil {
			return nil, err
		}
		return faultJobDir{d, stage}, nil
	}
	s.fs = fs
}

func TestFileStorePersistenceFaultMatrix(t *testing.T) {
	ctx := context.Background()
	stages := []string{"mkdir", "create-temp", "chmod", "write", "short", "file-sync", "file-close", "rename", "dir-open", "dir-sync", "dir-close"}
	for _, operation := range []string{"create", "update", "delete", "claim"} {
		for _, stage := range stages {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "jobs.json")
				s, err := NewFileStore(path)
				if err != nil {
					t.Fatal(err)
				}
				original := queueJob("a", "old")
				if err := s.Create(ctx, original); err != nil {
					t.Fatal(err)
				}
				injectJobFS(s, stage)
				switch operation {
				case "create":
					err = s.Create(ctx, queueJob("b", "new"))
				case "update":
					changed := original
					changed.IdempotencyKey = "new"
					err = s.Update(ctx, changed)
				case "delete":
					err = s.Delete(ctx, "a")
				case "claim":
					_, _, err = s.TryClaim(ctx, "a", "owner", time.Minute, time.Now())
				}
				uncertain := stage == "dir-open" || stage == "dir-sync" || stage == "dir-close"
				if err == nil || errors.Is(err, ErrFileStoreCommitUncertain) != uncertain {
					t.Fatalf("fault: %v uncertain=%v", err, uncertain)
				}
				if stage == "short" && !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("short write: %v", err)
				}
				if stage != "short" && !errors.Is(err, errJobFault) {
					t.Fatalf("lost cause: %v", err)
				}
				if uncertain {
					checks := []error{}
					_, e := s.Get(ctx, "a")
					checks = append(checks, e)
					_, e = s.GetByIdempotency(ctx, "old")
					checks = append(checks, e)
					_, e = s.List(ctx)
					checks = append(checks, e)
					checks = append(checks, s.Create(ctx, queueJob("c", "c")), s.Update(ctx, original), s.Delete(ctx, "a"))
					_, _, e = s.TryClaim(ctx, "a", "other", time.Minute, time.Now())
					checks = append(checks, e)
					for _, e := range checks {
						if !errors.Is(e, ErrFileStoreCommitUncertain) {
							t.Fatalf("unguarded operation: %v", e)
						}
					}
				} else {
					got, e := s.GetByIdempotency(ctx, "old")
					if e != nil || got.JobID != "a" || got.IdempotencyKey != "old" {
						t.Fatalf("rollback index: %+v %v", got, e)
					}
					if _, e := s.GetByIdempotency(ctx, "new"); !errors.Is(e, ErrNotFound) {
						t.Fatalf("uncommitted index: %v", e)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Get(ctx, "a"); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("closed: %v", err)
				}
				r, err := NewFileStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				if !uncertain || operation == "claim" || operation == "create" {
					got, e := r.GetByIdempotency(ctx, "old")
					if e != nil || got.JobID != "a" {
						t.Fatalf("reopen old key: %+v %v", got, e)
					}
				}
				if uncertain && operation == "update" {
					got, e := r.GetByIdempotency(ctx, "new")
					if e != nil || got.JobID != "a" {
						t.Fatalf("reopen new key: %+v %v", got, e)
					}
					if _, e = r.GetByIdempotency(ctx, "old"); !errors.Is(e, ErrNotFound) {
						t.Fatalf("old key retained: %v", e)
					}
				}
				if uncertain && operation == "delete" {
					if _, e := r.GetByIdempotency(ctx, "old"); !errors.Is(e, ErrNotFound) {
						t.Fatalf("deleted key retained: %v", e)
					}
				}
				if uncertain && operation == "create" {
					got, e := r.GetByIdempotency(ctx, "new")
					if e != nil || got.JobID != "b" {
						t.Fatalf("created key missing: %+v %v", got, e)
					}
				}
				if uncertain && operation == "claim" {
					got, e := r.Get(ctx, "a")
					if e != nil || got.LeaseOwner != "owner" {
						t.Fatalf("claim lost: %+v %v", got, e)
					}
				}
			})
		}
	}
}
