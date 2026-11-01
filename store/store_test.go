package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/xartus2004/arena/agent"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/internal/testenv"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func run(t *testing.T, s *Store, id string, p episode.Policy) *episode.Outcome {
	t.Helper()
	e, r := testenv.New(t)
	task, _ := e.Task("answer")
	cfg := episode.Config{Runtime: r, Recorder: s, ArtifactDir: filepath.Join(t.TempDir(), "a"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	out, err := episode.Run(context.Background(), cfg, task, id, 2, func() episode.Policy { return p })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTrajectoryAndIndex(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	big := strings.Repeat("x", MaxField+100)
	honest := &agent.Script{Calls: []episode.ToolCall{agent.Write(testenv.AnswerPath, testenv.Secret), agent.Write("/workspace/big", big)}}
	o1 := run(t, s, "ep-good", honest)
	o2 := run(t, s, "ep-bad", agent.Noop())
	if err := s.Save(ctx, o1, "honest"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, o2, "noop"); err != nil {
		t.Fatal(err)
	}

	evs, err := s.Trajectory("ep-good", 1)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	truncated := false
	for _, ev := range evs {
		types = append(types, ev.Type)
		if ev.Tool != nil && strings.HasSuffix(ev.Tool.Content, "...[truncated 100 bytes]") {
			truncated = true
		}
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"transition", "tool_call", "tool_result", "custody", "grade", "outcome"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("trajectory lacks %s: %s", want, joined)
		}
	}
	if !truncated {
		t.Fatal("long tool content not truncated with marker")
	}

	one, half := 1.0, 0.5
	tests := []struct {
		name string
		f    Filter
		want []string
	}{
		{"all", Filter{}, []string{"ep-bad", "ep-good"}},
		{"by env", Filter{Env: "toy", Task: "answer"}, []string{"ep-bad", "ep-good"}},
		{"by reward", Filter{MinReward: &one}, []string{"ep-good"}},
		{"max reward", Filter{MaxReward: &half}, []string{"ep-bad"}},
		{"by agent", Filter{Agent: "noop"}, []string{"ep-bad"}},
		{"by status", Filter{Status: episode.StatusFailed}, nil},
		{"other env", Filter{Env: "nope"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.Query(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			if len(ids) > 1 && ids[0] > ids[1] {
				ids[0], ids[1] = ids[1], ids[0]
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v want %v", ids, tc.want)
			}
		})
	}
}

func TestSaveIsIdempotentAcrossRetries(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o := &episode.Outcome{EpisodeID: "x", Attempt: 2, Env: "e", Task: "t", Status: episode.StatusDone, Reward: 1}
	stale := &episode.Outcome{EpisodeID: "x", Attempt: 1, Env: "e", Task: "t", Status: episode.StatusFailed, Failure: episode.FailureInfra}
	for _, oo := range []*episode.Outcome{o, o, stale} {
		if err := s.Save(ctx, oo, "a"); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := s.Query(ctx, Filter{})
	if len(rows) != 1 || rows[0].Attempt != 2 || rows[0].Reward != 1 {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestConcurrentRecord(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Record(episode.Event{EpisodeID: "c", Attempt: 1, Type: "log"})
			}
		}()
	}
	wg.Wait()
	evs, err := s.Trajectory("c", 1)
	if err != nil || len(evs) != 400 {
		t.Fatalf("events %d err %v", len(evs), err)
	}
}

func TestBadKeys(t *testing.T) {
	b := &FSBlobs{Root: t.TempDir()}
	for _, k := range []string{"../x", "a/../../b", ""} {
		if err := b.Append(k, []byte("x")); err == nil {
			t.Fatalf("key %q accepted", k)
		}
	}
}
