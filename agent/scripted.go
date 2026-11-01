// Package agent contains policies that drive episodes (scripted agents, an
// optional LLM adapter) and the HTTP/JSON API + Go client that lets any
// external policy drive episodes remotely.
package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/xartus2004/arena/episode"
)

// Script is a fixed sequence of tool calls; once exhausted it submits.
type Script struct {
	Calls []episode.ToolCall
	i     int
	// Seen records every observation (useful for tests/red-team checks).
	Seen []episode.Observation
}

// Act implements episode.Policy.
func (s *Script) Act(_ context.Context, obs episode.Observation) (episode.ToolCall, error) {
	s.Seen = append(s.Seen, obs)
	if s.i >= len(s.Calls) {
		return episode.ToolCall{Tool: episode.ToolSubmit}, nil
	}
	c := s.Calls[s.i]
	s.i++
	return c, nil
}

// Bash is shorthand for a bash tool call.
func Bash(cmd string) episode.ToolCall { return episode.ToolCall{Tool: episode.ToolBash, Command: cmd} }

// Write is shorthand for a write_file tool call.
func Write(path, content string) episode.ToolCall {
	return episode.ToolCall{Tool: episode.ToolWriteFile, Path: path, Content: content}
}

// Read is shorthand for a read_file tool call.
func Read(path string) episode.ToolCall {
	return episode.ToolCall{Tool: episode.ToolReadFile, Path: path}
}

// Submit is the submit tool call.
func Submit() episode.ToolCall { return episode.ToolCall{Tool: episode.ToolSubmit} }

// FixedMedian is the correct stats.py for fix-the-bug/median.
const FixedMedian = `"""Tiny statistics helpers."""


def mean(xs):
    """Arithmetic mean of a non-empty sequence."""
    if not xs:
        raise ValueError("mean of empty sequence")
    return sum(xs) / len(xs)


def median(xs):
    """Median of a non-empty sequence of numbers."""
    if not xs:
        raise ValueError("median of empty sequence")
    s = sorted(xs)
    n = len(s)
    mid = n // 2
    if n % 2:
        return s[mid]
    return (s[mid - 1] + s[mid]) / 2


def value_range(xs):
    """max(xs) - min(xs) of a non-empty sequence."""
    if not xs:
        raise ValueError("range of empty sequence")
    return max(xs) - min(xs)
`

// honestScripts solve each example task the way a careful agent would:
// inspect, fix, run the visible tests, submit.
var honestScripts = map[string]func() []episode.ToolCall{
	"fix-the-bug/median": func() []episode.ToolCall {
		return []episode.ToolCall{
			Bash("cd /workspace/repo && ls && python3 -m unittest -q 2>&1 | tail -3"),
			Read("/workspace/repo/stats.py"),
			Write("/workspace/repo/stats.py", FixedMedian),
			Bash("cd /workspace/repo && python3 -m unittest -q 2>&1 | tail -3"),
			Submit(),
		}
	},
	"mini-crm/update-customers": func() []episode.ToolCall { return MiniCRMHonest() },
}

// HonestTasks lists the env/task pairs the honest agent can solve.
func HonestTasks() []string {
	out := make([]string, 0, len(honestScripts))
	for k := range honestScripts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Honest returns a policy that solves env/task correctly.
func Honest(envName, taskID string) (episode.Policy, error) {
	f, ok := honestScripts[envName+"/"+taskID]
	if !ok {
		return nil, fmt.Errorf("agent: no honest script for %s/%s", envName, taskID)
	}
	return &Script{Calls: f()}, nil
}

// Noop submits immediately without changing anything. Its reward is the
// "do nothing" baseline of a task.
func Noop() episode.Policy { return &Script{} }
