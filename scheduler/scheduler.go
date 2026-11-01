// Package scheduler runs episodes on a bounded worker pool with a global
// concurrency limit, a bounded queue (backpressure), per-episode deadlines
// and graceful shutdown that drains in-flight and queued work.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/metrics"
	rt "github.com/xartus2004/arena/runtime"
)

// Errors returned by Submit.
var (
	ErrQueueFull = errors.New("scheduler: queue full")
	ErrClosed    = errors.New("scheduler: shut down")
)

// Saver persists final outcomes (store.Store implements it).
type Saver interface {
	Save(ctx context.Context, o *episode.Outcome, agent string) error
}

// Config configures a Scheduler.
type Config struct {
	// Concurrency is the global limit on running episodes (default 4).
	Concurrency int
	// QueueSize bounds waiting jobs (default 4×Concurrency).
	QueueSize int
	// MaxAttempts per episode for infrastructure failures (default 3).
	MaxAttempts int
	// DefaultTimeout is the per-episode deadline when a Job sets none
	// (default: task time limit + 5 minutes for setup, custody, grading).
	DefaultTimeout time.Duration
	Episode        episode.Config
	Metrics        *metrics.Metrics
	Saver          Saver
	Logger         *slog.Logger
}

// Job is one episode to run.
type Job struct {
	ID        string
	Task      *env.Task
	Agent     string
	NewPolicy func() episode.Policy
	Timeout   time.Duration
}

// Result is delivered once per job.
type Result struct {
	Job     Job
	Outcome *episode.Outcome
	Err     error
}

type item struct {
	job Job
	out chan Result
}

// Scheduler runs jobs.
type Scheduler struct {
	cfg    Config
	queue  chan item
	slots  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.RWMutex
	closed   bool
	inflight atomic.Int32
}

// New starts the workers.
func New(cfg Config) *Scheduler {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4 * cfg.Concurrency
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		cfg: cfg, queue: make(chan item, cfg.QueueSize), slots: make(chan struct{}, cfg.Concurrency),
		ctx: ctx, cancel: cancel,
	}
	// Every attempt (including retried infra failures) reaches metrics.
	s.cfg.Episode.Recorder = &observer{next: cfg.Episode.Recorder, m: cfg.Metrics}
	for i := 0; i < cfg.Concurrency; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

type observer struct {
	next episode.Recorder
	m    *metrics.Metrics
}

func (o *observer) Record(ev episode.Event) {
	if ev.Type == "outcome" {
		o.m.ObserveOutcome(ev.Outcome)
	}
	if o.next != nil {
		o.next.Record(ev)
	}
}

// Inflight returns the number of running episodes.
func (s *Scheduler) Inflight() int { return int(s.inflight.Load()) }

// Queued returns the number of waiting jobs.
func (s *Scheduler) Queued() int { return len(s.queue) }

func (s *Scheduler) validate(j Job) error {
	if j.ID == "" || j.Task == nil || j.NewPolicy == nil {
		return fmt.Errorf("scheduler: job needs ID, Task and NewPolicy")
	}
	return nil
}

// Submit enqueues a job without blocking; it returns ErrQueueFull when the
// queue is saturated (callers may retry later).
func (s *Scheduler) Submit(j Job) (<-chan Result, error) {
	if err := s.validate(j); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	it := item{job: j, out: make(chan Result, 1)}
	select {
	case s.queue <- it:
		s.gauges()
		return it.out, nil
	default:
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.Rejected.Inc()
		}
		return nil, ErrQueueFull
	}
}

// SubmitWait enqueues a job, waiting for queue space.
func (s *Scheduler) SubmitWait(ctx context.Context, j Job) (<-chan Result, error) {
	for {
		ch, err := s.Submit(j)
		if !errors.Is(err, ErrQueueFull) {
			return ch, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TryAcquire takes a concurrency slot without blocking (used by the HTTP
// API so interactive and batch episodes share one global limit).
func (s *Scheduler) TryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		s.inflight.Add(1)
		s.gauges()
		return true
	default:
		return false
	}
}

// Release returns a slot taken with TryAcquire.
func (s *Scheduler) Release() {
	<-s.slots
	s.inflight.Add(-1)
	s.gauges()
}

func (s *Scheduler) gauges() {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.QueueDepth.Set(float64(len(s.queue)))
		s.cfg.Metrics.Inflight.Set(float64(s.inflight.Load()))
	}
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for it := range s.queue {
		select {
		case s.slots <- struct{}{}:
		case <-s.ctx.Done():
			it.out <- Result{Job: it.job, Err: ErrClosed,
				Outcome: &episode.Outcome{EpisodeID: it.job.ID, Status: episode.StatusFailed, Failure: episode.FailureInfra, Error: ErrClosed.Error()}}
			continue
		}
		s.inflight.Add(1)
		s.gauges()
		it.out <- s.run(it.job)
		<-s.slots
		s.inflight.Add(-1)
		s.gauges()
	}
}

func (s *Scheduler) run(j Job) Result {
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = s.cfg.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = time.Duration(j.Task.TimeLimit) + 5*time.Minute
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	out, err := episode.Run(ctx, s.cfg.Episode, j.Task, j.ID, s.cfg.MaxAttempts, j.NewPolicy)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = rt.Infra("episode deadline exceeded", err)
	}
	if out != nil && s.cfg.Saver != nil {
		if serr := s.cfg.Saver.Save(context.WithoutCancel(ctx), out, j.Agent); serr != nil {
			s.cfg.Logger.Warn("save outcome", "episode", j.ID, "err", serr)
		}
	}
	return Result{Job: j, Outcome: out, Err: err}
}

// Shutdown stops accepting jobs and waits for queued and in-flight
// episodes to finish. If ctx expires first, running episodes are cancelled
// (their sandboxes are destroyed) and ctx's error is returned.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		<-done
		return ctx.Err()
	}
}
