package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type FileStore struct {
	mu sync.RWMutex
	path string
	jobs map[string]video.VideoJob
	idem map[string]string
}

type fileState struct {
	Jobs map[string]video.VideoJob `json:"jobs"`
	Idempotency map[string]string `json:"idempotency"`
}

func NewFileStore(path string) (*FileStore,error) {
	if path=="" { return nil,fmt.Errorf("store path is required") }
	s:=&FileStore{path:path,jobs:map[string]video.VideoJob{},idem:map[string]string{}}
	if data,err:=os.ReadFile(path); err==nil {
		var st fileState
		if err=json.Unmarshal(data,&st); err!=nil { return nil,fmt.Errorf("decode job store: %w",err) }
		if st.Jobs!=nil { s.jobs=st.Jobs }
		if st.Idempotency!=nil { s.idem=st.Idempotency }
		for id,job:=range s.jobs {
			if job.IdempotencyKey=="" { continue }
			if existing:=s.idem[job.IdempotencyKey]; existing=="" { s.idem[job.IdempotencyKey]=id
			} else if existing!=id { return nil,fmt.Errorf("duplicate persisted idempotency key") }
		}
	} else if !os.IsNotExist(err) { return nil,err }
	return s,nil
}

func (s *FileStore) persist() error {
	st:=fileState{Jobs:s.jobs,Idempotency:s.idem}
	data,err:=json.MarshalIndent(st,"","  ")
	if err!=nil { return err }
	dir:=filepath.Dir(s.path)
	if err=os.MkdirAll(dir,0700); err!=nil { return err }
	tmp,err:=os.CreateTemp(dir,".video-jobs-")
	if err!=nil { return err }
	name:=tmp.Name()
	defer os.Remove(name)
	if err=tmp.Chmod(0600); err==nil { _,err=tmp.Write(data) }
	if err==nil { err=tmp.Sync() }
	if closeErr:=tmp.Close(); err==nil { err=closeErr }
	if err!=nil { return err }
	if err=os.Rename(name,s.path); err!=nil { return err }
	if d,openErr:=os.Open(dir); openErr==nil { _=d.Sync(); _=d.Close() }
	return nil
}

func (s *FileStore) Create(_ context.Context,j video.VideoJob) error {
	s.mu.Lock(); defer s.mu.Unlock()
	if _,ok:=s.jobs[j.JobID]; ok { return fmt.Errorf("job already exists") }
	if j.IdempotencyKey!="" {
		if _,ok:=s.idem[j.IdempotencyKey]; ok { return fmt.Errorf("idempotency key already exists") }
		s.idem[j.IdempotencyKey]=j.JobID
	}
	s.jobs[j.JobID]=j
	if err:=s.persist(); err!=nil {
		delete(s.jobs,j.JobID)
		if j.IdempotencyKey!="" { delete(s.idem,j.IdempotencyKey) }
		return err
	}
	return nil
}

func (s *FileStore) Get(_ context.Context,id string) (video.VideoJob,error) {
	s.mu.RLock(); defer s.mu.RUnlock()
	j,ok:=s.jobs[id]; if !ok { return video.VideoJob{},ErrNotFound }; return j,nil
}

func (s *FileStore) GetByIdempotency(_ context.Context,key string) (video.VideoJob,error) {
	s.mu.RLock(); defer s.mu.RUnlock()
	id:=s.idem[key]; j,ok:=s.jobs[id]; if !ok { return video.VideoJob{},ErrNotFound }; return j,nil
}

func (s *FileStore) Update(_ context.Context,j video.VideoJob) error {
	s.mu.Lock(); defer s.mu.Unlock()
	old,ok:=s.jobs[j.JobID]; if !ok { return ErrNotFound }
	s.jobs[j.JobID]=j
	if err:=s.persist(); err!=nil { s.jobs[j.JobID]=old; return err }
	return nil
}

func (s *FileStore) List(_ context.Context) ([]video.VideoJob,error) {
	s.mu.RLock(); defer s.mu.RUnlock()
	out:=make([]video.VideoJob,0,len(s.jobs))
	for _,j:=range s.jobs { out=append(out,j) }
	return out,nil
}
