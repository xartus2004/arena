package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xartus2004/arena/agent"
	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/internal/testenv"
	"github.com/xartus2004/arena/metrics"
	"github.com/xartus2004/arena/runtime/fake"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// slowPolicy writes the answer after sleeping, tracking concurrency.
type slowPolicy struct {
	delay     time.Duration
	cur, peak *atomic.Int32
	done      bool
}

func (p *slowPolicy) Act(ctx context.Context, obs episode.Observation) (episode.ToolCall, error) {
	if p.done {
		return agent.Submit(), nil
	}
	n := p.cur.Add(1)
	for {
		old := p.peak.Load()
		if n <= old || p.peak.CompareAndSwap(old, n) {
			break
		}
	}
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
	}
	p.cur.Add(-1)
	p.done = true
	return agent.Write(testenv.AnswerPath, testenv.Secret), nil
}

type fixture struct {
	task  *env.Task
	rt    *fake.Runtime
	cfg   Config
	saved atomic.Int32
}

type countSaver struct{ n *atomic.Int32 }

func (c countSaver) Save(context.Context, *episode.Outcome, string) error { c.n.Add(1); return nil }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	e, r := testenv.New(t)
	task, _ := e.Task("answer")
	f := &fixture{task: task, rt: r}
	f.cfg = Config{
		Concurrency: 2, QueueSize: 4, MaxAttempts: 2, Logger: quiet, Metrics: metrics.New(),
		Episode: episode.Config{Runtime: r, ArtifactDir: filepath.Join(t.TempDir(), "a"), Logger: quiet},
		Saver:   countSaver{&f.saved},
	}
	return f
}

func job(f *fixture, id string, p func() episode.Policy) Job {
	return Job{ID: id, Task: f.task, Agent: "test", NewPolicy: p}
}

func TestConcurrencyLimitAndResults(t *testing.T) {
	f := newFixture(t)
	s := New(f.cfg)
	var cur, peak atomic.Int32
	var chans []<-chan Result
	for i := 0; i < 6; i++ {
		ch, err := s.SubmitWait(context.Background(), job(f, "j"+string(rune('a'+i)), func() episode.Policy {
			return &slowPolicy{delay: 30 * time.Millisecond, cur: &cur, peak: &peak}
		}))
		if err != nil {
			t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	for _, ch := range chans {
		r := <-ch
		if r.Err != nil || r.Outcome.Reward != 1 {
			t.Fatalf("result: %+v", r)
		}
	}
	if p := peak.Load(); p > 2 || p < 1 {
		t.Fatalf("peak concurrency %d, limit 2", p)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.saved.Load() != 6 || len(f.rt.Live()) != 0 {
		t.Fatalf("saved %d live %d", f.saved.Load(), len(f.rt.Live()))
	}
}

func TestBackpressureRejects(t *testing.T) {
	f := newFixture(t)
	f.cfg.Concurrency, f.cfg.QueueSize = 1, 1
	s := New(f.cfg)
	defer s.Shutdown(context.Background())
	release := make(chan struct{})
	blocking := func() episode.Policy {
		return policyFunc(func(ctx context.Context, _ episode.Observation) (episode.ToolCall, error) {
			<-release
			return agent.Submit(), nil
		})
	}
	ch1, err := s.Submit(job(f, "a", blocking))
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the worker picked up "a" so the queue is empty.
	waitFor(t, func() bool { return s.Inflight() == 1 })
	ch2, err := s.Submit(job(f, "b", blocking))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(job(f, "c", blocking)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
	close(release)
	<-ch1
	<-ch2
}

func TestGracefulShutdownDrains(t *testing.T) {
	f := newFixture(t)
	s := New(f.cfg)
	var cur, peak atomic.Int32
	var chans []<-chan Result
	for i := 0; i < 4; i++ {
		ch, err := s.Submit(job(f, string(rune('a'+i)), func() episode.Policy {
			return &slowPolicy{delay: 50 * time.Millisecond, cur: &cur, peak: &peak}
		}))
		if err != nil {
			t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, ch := range chans {
		if r := <-ch; r.Err != nil || r.Outcome.Reward != 1 {
			t.Fatalf("not drained: %+v", r)
		}
	}
	if _, err := s.Submit(job(f, "late", agent.Noop)); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after shutdown: %v", err)
	}
}

func TestShutdownTimeoutCancelsInflight(t *testing.T) {
	f := newFixture(t)
	s := New(f.cfg)
	ch, err := s.Submit(job(f, "stuck", func() episode.Policy {
		return policyFunc(func(ctx context.Context, _ episode.Observation) (episode.ToolCall, error) {
			<-ctx.Done()
			return episode.ToolCall{}, ctx.Err()
		})
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Inflight() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown: %v", err)
	}
	r := <-ch
	if r.Err == nil || r.Outcome.Status != episode.StatusFailed {
		t.Fatalf("stuck episode not cancelled: %+v", r)
	}
	if len(f.rt.Live()) != 0 {
		t.Fatal("sandboxes leaked after forced shutdown")
	}
}

func TestPerEpisodeDeadline(t *testing.T) {
	f := newFixture(t)
	s := New(f.cfg)
	defer s.Shutdown(context.Background())
	j := job(f, "slow", func() episode.Policy {
		return policyFunc(func(ctx context.Context, _ episode.Observation) (episode.ToolCall, error) {
			<-ctx.Done()
			return episode.ToolCall{}, ctx.Err()
		})
	})
	j.Timeout = 30 * time.Millisecond
	ch, _ := s.Submit(j)
	r := <-ch
	if r.Err == nil || r.Outcome.Status != episode.StatusFailed {
		t.Fatalf("deadline not enforced: %+v", r)
	}
}

func TestInfraRetriesThroughScheduler(t *testing.T) {
	f := newFixture(t)
	f.rt.FailCreate.Store(1)
	s := New(f.cfg)
	defer s.Shutdown(context.Background())
	ch, _ := s.Submit(job(f, "retry", func() episode.Policy {
		return &agent.Script{Calls: []episode.ToolCall{agent.Write(testenv.AnswerPath, testenv.Secret)}}
	}))
	r := <-ch
	if r.Err != nil || r.Outcome.Attempt != 2 || r.Outcome.Reward != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestAdmissionSharesSlots(t *testing.T) {
	f := newFixture(t)
	s := New(f.cfg)
	defer s.Shutdown(context.Background())
	for i := 0; i < 2; i++ {
		if !s.TryAcquire() {
			t.Fatalf("slot %d should be admitted (concurrency 2)", i)
		}
	}
	if s.TryAcquire() {
		t.Fatal("should reject beyond concurrency")
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("slot not released")
	}
	s.Release()
	s.Release()
}

type policyFunc func(context.Context, episode.Observation) (episode.ToolCall, error)

func (f policyFunc) Act(ctx context.Context, o episode.Observation) (episode.ToolCall, error) {
	return f(ctx, o)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met")
}
