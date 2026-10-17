// Package jobs is a durable outbox job queue on top of the store.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
)

const defaultMaxAttempts = 10

// Job describes work to enqueue. MaxAttempts 0 means 10.
type Job struct {
	Kind        string
	Payload     any
	RunAt       time.Time
	MaxAttempts int
	DedupeKey   string
}

// Handler processes one job payload.
type Handler func(ctx context.Context, payload json.RawMessage) error

// Options configures a Queue. Zero values use the defaults noted.
type Options struct {
	Workers      int           // 4
	PollInterval time.Duration // 1s
	Lease        time.Duration // 5m
	Clock        func() time.Time
	Jitter       func() float64 // uniform in [-0.2, 0.2]
	Logger       *slog.Logger
}

// Queue claims and runs jobs from the jobs table.
type Queue struct {
	db   *store.DB
	opts Options

	mu       sync.RWMutex
	handlers map[string]Handler
	running  atomic.Bool
}

// New builds a Queue.
func New(db *store.DB, opts Options) *Queue {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.Lease <= 0 {
		opts.Lease = 5 * time.Minute
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Jitter == nil {
		opts.Jitter = func() float64 { return rand.Float64()*0.4 - 0.2 }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Queue{db: db, opts: opts, handlers: map[string]Handler{}}
}

// Register adds the handler for kind. It panics on a duplicate kind.
func (q *Queue) Register(kind string, h Handler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, dup := q.handlers[kind]; dup {
		panic(fmt.Sprintf("jobs: handler for %q already registered", kind))
	}
	q.handlers[kind] = h
}

// Enqueue inserts a job inside the caller's transaction, so it exists only if
// that transaction commits.
func (q *Queue) Enqueue(ctx context.Context, tx store.Tx, j Job) error {
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return fmt.Errorf("marshal payload for %q: %w", j.Kind, err)
	}
	if j.RunAt.IsZero() {
		j.RunAt = q.opts.Clock()
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = defaultMaxAttempts
	}
	return q.db.InsertJob(ctx, tx, store.JobRow{
		ID: ids.New("job"), Kind: j.Kind, Payload: payload, RunAt: j.RunAt,
		MaxAttempts: j.MaxAttempts, DedupeKey: j.DedupeKey,
	})
}

// RunOnce claims up to Workers due jobs, runs them concurrently and returns
// how many it processed.
func (q *Queue) RunOnce(ctx context.Context) (int, error) {
	return q.runOnce(ctx, ctx)
}

// runOnce claims with claimCtx but processes with workCtx, so shutdown can
// let in-flight jobs finish.
func (q *Queue) runOnce(claimCtx, workCtx context.Context) (int, error) {
	rows, err := q.db.ClaimJobs(claimCtx, q.opts.Clock(), q.opts.Lease, q.opts.Workers)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.process(workCtx, r)
		}()
	}
	wg.Wait()
	return len(rows), nil
}

func (q *Queue) process(ctx context.Context, r store.JobRow) {
	q.mu.RLock()
	h, ok := q.handlers[r.Kind]
	q.mu.RUnlock()
	if !ok {
		q.fail(ctx, r, fmt.Errorf("no handler registered for %q", r.Kind), true)
		return
	}
	q.finish(ctx, r, h)
}

func (q *Queue) finish(ctx context.Context, r store.JobRow, h Handler) {
	err := h(ctx, r.Payload)
	if err != nil {
		var perm permanentError
		q.fail(ctx, r, err, errors.As(err, &perm) || r.Attempts >= r.MaxAttempts)
		return
	}
	if err := q.db.CompleteJob(ctx, r.ID, q.opts.Clock()); err != nil {
		q.opts.Logger.Error("complete job", "job_id", r.ID, "err", err)
	}
}

func (q *Queue) fail(ctx context.Context, r store.JobRow, cause error, dead bool) {
	now := q.opts.Clock()
	next := now.Add(Backoff(r.Attempts, q.opts.Jitter()))
	if err := q.db.FailJob(ctx, r.ID, now, next, cause.Error(), dead); err != nil {
		q.opts.Logger.Error("fail job", "job_id", r.ID, "err", err)
	}
}

// Start runs the poll loop until ctx is cancelled.
func (q *Queue) Start(ctx context.Context) error {
	return errors.New("not implemented")
}

// Running reports whether Start is looping.
func (q *Queue) Running() bool { return q.running.Load() }

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks err as non-retryable: the job goes straight to dead.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}
