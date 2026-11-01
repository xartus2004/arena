package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/scheduler"
)

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var sf stackFlags
	sf.register(fs)
	envName := fs.String("env", "fix-the-bug", "environment")
	taskID := fs.String("task", "", "task id (default: first task)")
	agentName := fs.String("agent", "honest", "agent: honest, noop, attack-N, llm")
	n := fs.Int("n", 10, "number of episodes")
	prefix := fs.String("id-prefix", "", "episode id prefix (default: timestamp)")
	_ = fs.Parse(args)

	st, err := openStack(ctx, sf)
	if err != nil {
		return err
	}
	e, ok := st.envs[*envName]
	if !ok {
		st.close(ctx)
		return fmt.Errorf("unknown env %q", *envName)
	}
	task := e.Tasks[0]
	if *taskID != "" {
		if task, err = e.Task(*taskID); err != nil {
			st.close(ctx)
			return err
		}
	}
	newPolicy, err := policyFactory(*agentName, task)
	if err != nil {
		st.close(ctx)
		return err
	}
	st.warmFor(e)
	if *prefix == "" {
		*prefix = fmt.Sprintf("run-%d", time.Now().Unix())
	}
	start := time.Now()
	results := make(chan scheduler.Result, *n)
	go func() {
		for i := 0; i < *n; i++ {
			j := scheduler.Job{ID: fmt.Sprintf("%s-%04d", *prefix, i), Task: task, Agent: *agentName, NewPolicy: newPolicy}
			ch, err := st.sched.SubmitWait(ctx, j)
			if err != nil {
				results <- scheduler.Result{Job: j, Err: err}
				continue
			}
			go func() { results <- <-ch }()
		}
	}()
	var rewards []float64
	var resets []time.Duration
	statuses := map[string]int{}
	for i := 0; i < *n; i++ {
		r := <-results
		key := "error"
		if r.Outcome != nil {
			key = string(r.Outcome.Status)
			if r.Outcome.Failure != "" {
				key += "/" + string(r.Outcome.Failure)
			}
			if r.Outcome.Violation != nil {
				key += "/violation:" + string(r.Outcome.Violation.Kind)
			}
			if r.Outcome.Status == episode.StatusDone {
				rewards = append(rewards, r.Outcome.Reward)
				resets = append(resets, r.Outcome.Phases[episode.Provision]+r.Outcome.Phases[episode.Setup])
			}
		}
		statuses[key]++
		if sf.verbose && r.Err != nil {
			st.log.Warn("episode error", "id", r.Job.ID, "err", r.Err)
		}
	}
	elapsed := time.Since(start)
	err = st.close(context.WithoutCancel(ctx))

	fmt.Printf("env=%s task=%s agent=%s runtime=%s n=%d concurrency=%d\n", e.Name, task.ID, *agentName, sf.runtime, *n, sf.concurrency)
	fmt.Printf("wall=%s  episodes/sec=%.2f  warm hits=%d misses=%d\n", elapsed.Round(time.Millisecond), float64(*n)/elapsed.Seconds(), st.pool.Hits(), st.pool.Misses())
	fmt.Printf("mean reward=%.3f over %d completed\n", mean(rewards), len(rewards))
	fmt.Printf("reset latency p50=%s p95=%s\n", pct(resets, 50), pct(resets, 95))
	keys := make([]string, 0, len(statuses))
	for k := range statuses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-40s %d\n", k, statuses[k])
	}
	return err
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	idx := int(float64(len(c)-1) * p / 100)
	return c[idx].Round(time.Millisecond)
}
