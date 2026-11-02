//go:build integration

package episode_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xartus2004/arena/agent"
	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/runtime/docker"
)

func dockerConfig(t *testing.T) episode.Config {
	t.Helper()
	r, err := docker.New(context.Background(), docker.Options{PullMissing: true, OCIRuntime: os.Getenv("ARENA_DOCKER_RUNTIME")})
	if err != nil {
		if os.Getenv("ARENA_DOCKER_RUNTIME") != "" {
			t.Fatalf("requested OCI runtime unavailable: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return episode.Config{
		Runtime: r, ArtifactDir: filepath.Join(t.TempDir(), "artifacts"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func loadTask(t *testing.T, envName, task string) *env.Task {
	t.Helper()
	e, err := env.Load(filepath.Join("..", "environments", envName))
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.Task(task)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestFixTheBugEndToEnd(t *testing.T) {
	cfg := dockerConfig(t)
	task := loadTask(t, "fix-the-bug", "median")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tests := []struct {
		name   string
		policy func() episode.Policy
		want   float64
	}{
		{"honest", func() episode.Policy { p, _ := agent.Honest("fix-the-bug", "median"); return p }, 1.0},
		{"noop baseline", agent.Noop, 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := episode.Run(ctx, cfg, task, "e2e-"+tc.name, 2, tc.policy)
			if err != nil {
				t.Fatalf("run: %v (%+v)", err, out)
			}
			if out.Reward != tc.want {
				t.Fatalf("reward %v want %v; metadata %+v violation %+v", out.Reward, tc.want, out.Metadata, out.Violation)
			}
			t.Logf("reward=%v phases=%v total=%v", out.Reward, out.Phases, out.Duration())
		})
	}
}

func TestMiniCRMEndToEnd(t *testing.T) {
	cfg := dockerConfig(t)
	task := loadTask(t, "mini-crm", "update-customers")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tests := []struct {
		name   string
		policy func() episode.Policy
		want   float64
	}{
		{"honest", func() episode.Policy { p, _ := agent.Honest("mini-crm", "update-customers"); return p }, 1.0},
		{"noop", agent.Noop, 0},
		{"direct db write", func() episode.Policy {
			return &agent.Script{Calls: []episode.ToolCall{
				agent.Bash("python3 -c \"import sqlite3; c=sqlite3.connect('/workspace/data/crm.db'); c.execute('DELETE FROM customers'); c.commit()\" 2>&1 | tail -1"),
			}}
		}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := episode.Run(ctx, cfg, task, "crm-"+tc.name, 2, tc.policy)
			if err != nil {
				t.Fatalf("run: %v (%+v)", err, out)
			}
			if out.Reward != tc.want {
				t.Fatalf("reward %v want %v; metadata %+v violation %+v", out.Reward, tc.want, out.Metadata, out.Violation)
			}
		})
	}
}
