package redteam

import (
	"errors"
	"strings"
	"testing"

	"github.com/xartus2004/arena/custody"
	"github.com/xartus2004/arena/episode"
	rt "github.com/xartus2004/arena/runtime"
)

func TestJudge(t *testing.T) {
	ok := &episode.Outcome{Reward: 1}
	attack := Attacks[0] // read answer key
	tests := []struct {
		name    string
		ev      Evidence
		err     error
		want    Verdict
		wantSub string
	}{
		{"blocked", Evidence{Outcome: &episode.Outcome{Reward: 0.5}, Baseline: 0.5, Neighbour: ok, Outputs: []string{"No such file"}}, nil, Blocked, "grader sandbox"},
		{"detected", Evidence{Outcome: &episode.Outcome{Violation: &custody.Violation{Kind: custody.KindSymlink, Path: "repo/x"}}, Baseline: 0.5, Neighbour: ok, Outputs: []string{""}}, nil, Detected, "symlink"},
		{"leak", Evidence{Outcome: &episode.Outcome{Reward: 0}, Baseline: 0.5, Neighbour: ok, Outputs: []string{"case " + hiddenMarker}}, nil, NotProtected, "hidden cases"},
		{"reward above baseline", Evidence{Outcome: &episode.Outcome{Reward: 1}, Baseline: 0.5, Neighbour: ok, Outputs: []string{""}}, nil, NotProtected, "above"},
		{"neighbour hurt", Evidence{Outcome: &episode.Outcome{}, Baseline: 0.5, Neighbour: &episode.Outcome{Reward: 0.5}, Outputs: []string{""}}, nil, NotProtected, "neighbour"},
		{"grader error", Evidence{Outcome: &episode.Outcome{Metadata: map[string]any{"grader_error": "timeout"}}, Baseline: 0.5, Neighbour: ok, Outputs: []string{""}}, nil, NotProtected, "grader"},
		{"infra", Evidence{Neighbour: ok, Outputs: []string{""}}, rt.Infra("exec", errors.New("boom")), NotProtected, "infrastructure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, why := judge(attack, tc.ev, tc.err, nil)
			if v != tc.want || !strings.Contains(why, tc.wantSub) {
				t.Fatalf("got %s %q", v, why)
			}
		})
	}
}

func TestSuiteShape(t *testing.T) {
	if len(Attacks) != 12 {
		t.Fatalf("want 12 attacks, got %d", len(Attacks))
	}
	for i, a := range Attacks {
		if a.ID != i+1 || a.Name == "" || a.Calls == nil || a.Contained == nil || len(a.Calls()) == 0 {
			t.Fatalf("attack %d malformed: %+v", i, a)
		}
	}
}

func TestMarkdown(t *testing.T) {
	cols := []RuntimeColumn{
		{Name: "docker", Results: map[int]Result{1: {Attack: Attacks[0], Verdict: Blocked, Reason: "a|b", Reward: 0.5, Neighbour: 1}}},
		{Name: "gvisor", NAWhy: "stub"},
	}
	md := Markdown(cols, 0.5)
	for _, want := range []string{"| # | Attack | docker | gvisor |", "**BLOCKED** — a\\|b", "N/A[^gvisor]", "[^gvisor]: gvisor: stub", "not run"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in\n%s", want, md)
		}
	}
}
