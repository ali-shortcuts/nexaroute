package queue

import (
	"context"
	"errors"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var ErrFull = errors.New("video queue is full")

type Queue struct {
	jobs chan video.VideoJob
	once sync.Once
}

func New(size int) *Queue {
	if size < 1 {
		size = 1
	}
	return &Queue{jobs: make(chan video.VideoJob, size)}
}
func (q *Queue) Enqueue(ctx context.Context, j video.VideoJob) error {
	select {
	case q.jobs <- j:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrFull
	}
}
func (q *Queue) Next(ctx context.Context) (video.VideoJob, error) {
	select {
	case j, ok := <-q.jobs:
		if !ok {
			return video.VideoJob{}, context.Canceled
		}
		return j, nil
	case <-ctx.Done():
		return video.VideoJob{}, ctx.Err()
	}
}
func (q *Queue) Len() int { return len(q.jobs) }
func (q *Queue) Close()   { q.once.Do(func() { close(q.jobs) }) }

type Handler func(context.Context, video.VideoJob) error
type WorkerPool struct {
	q       *Queue
	workers int
	handler Handler
}

func NewWorkerPool(q *Queue, workers int, handler Handler) *WorkerPool {
	if workers < 1 {
		workers = 1
	}
	return &WorkerPool{q: q, workers: workers, handler: handler}
}
func (p *WorkerPool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := p.q.Next(ctx)
				if err != nil {
					return
				}
				_ = p.handler(ctx, j)
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
}
