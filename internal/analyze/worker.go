package analyze

import (
	"context"
	"errors"
	"sync"

	"github.com/tarunjain/agent-history/internal/store"
)

// ErrAlreadyQueued prevents duplicate paid work for one session.
var ErrAlreadyQueued = errors.New("analysis is already queued or running")

// Worker serializes analysis requests to protect subscription usage.
type Worker struct {
	engine  *Engine
	store   *store.Store
	ctx     context.Context
	cancel  context.CancelFunc
	queue   chan queuedAnalysis
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[string]bool
}

type queuedAnalysis struct {
	sessionID string
	options   Options
	done      chan error
}

// NewWorker starts one analysis worker with bounded queue capacity.
func NewWorker(database *store.Store, engine *Engine, queueCapacity int) *Worker {
	if queueCapacity < 1 {
		queueCapacity = 16
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &Worker{
		engine: engine, store: database, ctx: ctx, cancel: cancel,
		queue: make(chan queuedAnalysis, queueCapacity), pending: make(map[string]bool),
	}
	worker.wg.Add(1)
	go worker.run()
	return worker
}

// Enqueue records queued state and schedules one analysis. The returned channel
// receives exactly one completion error and is then closed.
func (w *Worker) Enqueue(ctx context.Context, sessionID string, options Options) (<-chan error, error) {
	w.mu.Lock()
	if w.pending[sessionID] {
		w.mu.Unlock()
		return nil, ErrAlreadyQueued
	}
	w.pending[sessionID] = true
	w.mu.Unlock()
	removePending := func() {
		w.mu.Lock()
		delete(w.pending, sessionID)
		w.mu.Unlock()
	}
	if err := w.store.SetAnalysisStatus(ctx, sessionID, "queued", ""); err != nil {
		removePending()
		return nil, err
	}
	done := make(chan error, 1)
	job := queuedAnalysis{sessionID: sessionID, options: options, done: done}
	select {
	case w.queue <- job:
		return done, nil
	case <-ctx.Done():
		removePending()
		_ = w.store.SetAnalysisStatus(context.WithoutCancel(ctx), sessionID, "failed", ctx.Err().Error())
		return nil, ctx.Err()
	case <-w.ctx.Done():
		removePending()
		return nil, errors.New("analysis worker is closed")
	}
}

// Close cancels the active analysis and drains queued requests with cancellation.
func (w *Worker) Close() {
	w.cancel()
	w.wg.Wait()
}

func (w *Worker) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			w.drain()
			return
		case job := <-w.queue:
			_, err := w.engine.Analyze(w.ctx, job.sessionID, job.options)
			w.removePending(job.sessionID)
			job.done <- err
			close(job.done)
		}
	}
}

func (w *Worker) drain() {
	for {
		select {
		case job := <-w.queue:
			w.removePending(job.sessionID)
			job.done <- context.Canceled
			close(job.done)
		default:
			return
		}
	}
}

func (w *Worker) removePending(sessionID string) {
	w.mu.Lock()
	delete(w.pending, sessionID)
	w.mu.Unlock()
}
