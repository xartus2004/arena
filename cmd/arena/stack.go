package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/xartus2004/arena/agent"
	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/metrics"
	"github.com/xartus2004/arena/redteam"
	"github.com/xartus2004/arena/scheduler"
	"github.com/xartus2004/arena/store"
)

type stackFlags struct {
	runtime     string
	envDir      string
	dataDir     string
	concurrency int
	queue       int
	warm        int
	attempts    int
	verbose     bool
}

func (f *stackFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.runtime, "runtime", "docker", "sandbox runtime (docker, fake)")
	fs.StringVar(&f.envDir, "envs", "environments", "environments directory")
	fs.StringVar(&f.dataDir, "data", "data", "data directory (trajectories, index, artifacts)")
	fs.IntVar(&f.concurrency, "concurrency", 4, "global limit on running episodes")
	fs.IntVar(&f.queue, "queue", 0, "queue size (default 4x concurrency)")
	fs.IntVar(&f.warm, "warm", -1, "warm sandboxes per spec (default = concurrency, 0 disables)")
	fs.IntVar(&f.attempts, "max-attempts", 3, "attempts per episode for infrastructure failures")
	fs.BoolVar(&f.verbose, "v", false, "verbose logs")
}

type stack struct {
	log     *slog.Logger
	envs    map[string]*env.Environment
	pool    *scheduler.Pool
	store   *store.Store
	metrics *metrics.Metrics
	sched   *scheduler.Scheduler
	epCfg   episode.Config
	closeRT func()
}

func openStack(ctx context.Context, f stackFlags) (*stack, error) {
	log := newLogger(f.verbose)
	envs, err := env.LoadAll(f.envDir)
	if err != nil {
		return nil, err
	}
	r, closeRT, err := openRuntime(ctx, f.runtime, log)
	if err != nil {
		return nil, err
	}
	m := metrics.New()
	warm := f.warm
	if warm < 0 {
		warm = f.concurrency
	}
	pool := scheduler.NewPool(r, scheduler.PoolConfig{Size: warm, Metrics: m, Logger: log})
	st, err := store.Open(f.dataDir)
	if err != nil {
		closeRT()
		return nil, err
	}
	st.OnError = func(err error) { log.Warn("trajectory write", "err", err) }
	epCfg := episode.Config{Runtime: pool, Recorder: st, ArtifactDir: filepath.Join(f.dataDir, "artifacts"), Logger: log}
	sched := scheduler.New(scheduler.Config{
		Concurrency: f.concurrency, QueueSize: f.queue, MaxAttempts: f.attempts,
		Episode: epCfg, Metrics: m, Saver: st, Logger: log,
	})
	return &stack{log: log, envs: envs, pool: pool, store: st, metrics: m, sched: sched, epCfg: epCfg, closeRT: closeRT}, nil
}

// warmFor pre-warms student and grader sandboxes of an env.
func (s *stack) warmFor(e *env.Environment) {
	if warm := s.pool; warm != nil {
		warm.Warm(e.StudentSpec())
		warm.Warm(e.GraderSpec())
	}
}

func (s *stack) close(ctx context.Context) error {
	err := s.sched.Shutdown(ctx)
	_ = s.pool.Close(context.WithoutCancel(ctx))
	_ = s.store.Close()
	s.closeRT()
	return err
}

// policyFactory resolves --agent.
func policyFactory(name string, task *env.Task) (func() episode.Policy, error) {
	switch {
	case name == "honest":
		if _, err := agent.Honest(task.Env().Name, task.ID); err != nil {
			return nil, err
		}
		return func() episode.Policy { p, _ := agent.Honest(task.Env().Name, task.ID); return p }, nil
	case name == "noop":
		return agent.Noop, nil
	case strings.HasPrefix(name, "attack-"):
		var id int
		if _, err := fmt.Sscanf(name, "attack-%d", &id); err == nil {
			for _, a := range redteam.Attacks {
				if a.ID == id {
					return func() episode.Policy { return &agent.Script{Calls: a.Calls()} }, nil
				}
			}
		}
	case name == "llm":
		if _, err := agent.NewLLMFromEnv(); err != nil {
			return nil, err
		}
		return func() episode.Policy { p, _ := agent.NewLLMFromEnv(); return p }, nil
	}
	return nil, fmt.Errorf("unknown agent %q (honest, noop, attack-N, llm)", name)
}
