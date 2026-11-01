//go:build integration

package redteam

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/runtime/docker"
)

func TestSuiteOnDocker(t *testing.T) {
	r, err := docker.New(context.Background(), docker.Options{PullMissing: true})
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer r.Close()
	e, err := env.Load(filepath.Join("..", "environments", "fix-the-bug"))
	if err != nil {
		t.Fatal(err)
	}
	task, _ := e.Task("median")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	results, _, err := Run(ctx, Options{
		Config: episode.Config{Runtime: r, ArtifactDir: filepath.Join(t.TempDir(), "a"),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Task: task,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		t.Logf("%2d %-26s %-13s %s (reward %.2f neighbour %.2f, %v)", res.Attack.ID, res.Attack.Name, res.Verdict, res.Reason, res.Reward, res.Neighbour, res.Duration.Round(time.Millisecond))
		if res.Verdict == NotProtected {
			t.Errorf("attack %d (%s) NOT PROTECTED on docker: %s", res.Attack.ID, res.Attack.Name, res.Reason)
		}
	}
}
