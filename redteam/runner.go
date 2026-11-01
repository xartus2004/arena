package redteam

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xartus2004/arena/agent"
	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	rt "github.com/xartus2004/arena/runtime"
)

// Result is one attack run on one runtime.
type Result struct {
	Attack    Attack
	Runtime   string
	Verdict   Verdict
	Reason    string
	Reward    float64
	Neighbour float64
	Duration  time.Duration
}

// recorder is a Policy wrapper that keeps every tool output.
type recorder struct {
	inner   episode.Policy
	mu      sync.Mutex
	outputs []string
}

func (r *recorder) Act(ctx context.Context, obs episode.Observation) (episode.ToolCall, error) {
	if obs.Result != nil {
		r.mu.Lock()
		r.outputs = append(r.outputs, obs.Result.Output+obs.Result.Stderr)
		r.mu.Unlock()
	}
	return r.inner.Act(ctx, obs)
}

// Options configures a red-team run.
type Options struct {
	Config   episode.Config // Runtime must be set
	Task     *env.Task      // fix-the-bug/median
	Parallel int            // attacks run concurrently (default 3)
	Only     []int          // attack IDs to run (default all)
	Progress func(Result)   // optional callback per finished attack
}

// Baseline runs the noop agent to get the do-nothing reward.
func Baseline(ctx context.Context, cfg episode.Config, task *env.Task) (float64, error) {
	out, err := episode.Run(ctx, cfg, task, "redteam-baseline", 2, agent.Noop)
	if err != nil {
		return 0, fmt.Errorf("redteam baseline: %w", err)
	}
	return out.Reward, nil
}

// Run executes the suite on opts.Config.Runtime and returns the results
// and the do-nothing baseline they were judged against.
func Run(ctx context.Context, opts Options) ([]Result, float64, error) {
	if opts.Parallel <= 0 {
		opts.Parallel = 3
	}
	baseline, err := Baseline(ctx, opts.Config, opts.Task)
	if err != nil {
		return nil, 0, err
	}
	selected := Attacks
	if len(opts.Only) > 0 {
		selected = nil
		for _, a := range Attacks {
			for _, id := range opts.Only {
				if a.ID == id {
					selected = append(selected, a)
				}
			}
		}
	}
	results := make([]Result, len(selected))
	sem := make(chan struct{}, opts.Parallel)
	var wg sync.WaitGroup
	var progMu sync.Mutex
	for i, a := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runOne(ctx, opts, a, baseline)
			if opts.Progress != nil {
				progMu.Lock()
				opts.Progress(results[i])
				progMu.Unlock()
			}
		}()
	}
	wg.Wait()
	return results, baseline, ctx.Err()
}

func runOne(ctx context.Context, opts Options, a Attack, baseline float64) Result {
	start := time.Now()
	res := Result{Attack: a, Runtime: opts.Config.Runtime.Name()}
	rec := &recorder{inner: &agent.Script{Calls: a.Calls()}}

	var wg sync.WaitGroup
	var neighbour *episode.Outcome
	var nErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		neighbour, nErr = episode.Run(ctx, opts.Config, opts.Task, fmt.Sprintf("redteam-%02d-neighbour", a.ID), 2,
			func() episode.Policy { p, _ := agent.Honest("fix-the-bug", "median"); return p })
	}()
	// The attack is not retried: an infra failure it causes must be visible.
	out, err := episode.Run(ctx, opts.Config, opts.Task, fmt.Sprintf("redteam-%02d-attack", a.ID), 1,
		func() episode.Policy { return rec })
	wg.Wait()
	res.Duration = time.Since(start)
	if out != nil {
		res.Reward = out.Reward
	}
	if neighbour != nil {
		res.Neighbour = neighbour.Reward
	}
	ev := Evidence{Outcome: out, Outputs: rec.outputs, Baseline: baseline, Neighbour: neighbour}
	if len(ev.Outputs) == 0 {
		ev.Outputs = []string{""}
	}
	res.Verdict, res.Reason = judge(a, ev, err, nErr)
	return res
}

func judge(a Attack, e Evidence, err, nErr error) (Verdict, string) {
	switch {
	case err != nil && rt.IsInfra(err):
		return NotProtected, "platform infrastructure failure: " + err.Error()
	case err != nil:
		return NotProtected, "episode error: " + err.Error()
	case nErr != nil:
		return NotProtected, "honest neighbour failed: " + nErr.Error()
	case e.Neighbour == nil || e.Neighbour.Reward != 1:
		return NotProtected, fmt.Sprintf("honest neighbour scored %v", e.Neighbour.Reward)
	}
	if ge, ok := e.Outcome.Metadata["grader_error"]; ok {
		return NotProtected, fmt.Sprintf("grader errored: %v", ge)
	}
	if a.Succeeded != nil {
		if ok, why := a.Succeeded(e); ok {
			return NotProtected, why
		}
	}
	if e.Outcome.Reward > e.Baseline+1e-9 || math.IsNaN(e.Outcome.Reward) {
		return NotProtected, fmt.Sprintf("reward %.2f above do-nothing baseline %.2f", e.Outcome.Reward, e.Baseline)
	}
	if v := e.Outcome.Violation; v != nil {
		return Detected, fmt.Sprintf("custody %s at %s; %s", v.Kind, shortPath(v.Path), a.Contained(e))
	}
	return Blocked, a.Contained(e)
}

func shortPath(p string) string {
	if p == "" {
		return "-"
	}
	if i := strings.Index(p, "/submission/"); i >= 0 {
		return p[i+len("/submission/"):]
	}
	return p
}

// RuntimeColumn describes one column of the report.
type RuntimeColumn struct {
	Name    string
	Results map[int]Result // by attack ID; nil => whole column N/A
	NAWhy   string
}

// Markdown renders the attack × runtime table.
func Markdown(cols []RuntimeColumn, baseline float64) string {
	var b strings.Builder
	b.WriteString("| # | Attack |")
	for _, c := range cols {
		b.WriteString(" " + c.Name + " |")
	}
	b.WriteString("\n|---|---|")
	for range cols {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, a := range Attacks {
		fmt.Fprintf(&b, "| %d | %s |", a.ID, a.Name)
		for _, c := range cols {
			r, ok := c.Results[a.ID]
			switch {
			case c.Results == nil:
				fmt.Fprintf(&b, " %s[^%s] |", NotApplicable, c.Name)
			case !ok:
				b.WriteString(" not run |")
			default:
				fmt.Fprintf(&b, " **%s** — %s (reward %.2f, neighbour %.2f) |", r.Verdict, esc(r.Reason), r.Reward, r.Neighbour)
			}
		}
		b.WriteString("\n")
	}
	for _, c := range cols {
		if c.Results == nil {
			fmt.Fprintf(&b, "\n[^%s]: %s: %s", c.Name, c.Name, esc(c.NAWhy))
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "\nDo-nothing baseline reward: %.2f. Honest reward: 1.00. ", baseline)
	b.WriteString("BLOCKED = the attack had no effect; DETECTED = custody caught it and scored 0; NOT PROTECTED = see reason.\n")
	return b.String()
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

// Summary counts verdicts.
func Summary(rs []Result) map[Verdict]int {
	m := map[Verdict]int{}
	for _, r := range rs {
		m[r.Verdict]++
	}
	return m
}

// SortByID orders results by attack ID.
func SortByID(rs []Result) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Attack.ID < rs[j].Attack.ID })
}
