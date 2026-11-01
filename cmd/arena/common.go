package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/docker"
	"github.com/xartus2004/arena/runtime/fake"
	"github.com/xartus2004/arena/runtime/firecracker"
	"github.com/xartus2004/arena/runtime/gvisor"
)

func newLogger(verbose bool) *slog.Logger {
	if !verbose {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// openRuntime returns the named runtime and a close func.
func openRuntime(ctx context.Context, name string, log *slog.Logger) (rt.Runtime, func(), error) {
	switch name {
	case "docker":
		r, err := docker.New(ctx, docker.Options{PullMissing: true, Logger: log})
		if err != nil {
			return nil, nil, err
		}
		return r, func() { r.Close() }, nil
	case "fake":
		return fake.New(), func() {}, nil
	case "gvisor":
		return gvisor.New(), func() {}, nil
	case "firecracker":
		return firecracker.New(), func() {}, nil
	}
	return nil, nil, fmt.Errorf("unknown runtime %q (docker, fake, gvisor, firecracker)", name)
}

func loadTask(envDir, envName, taskID string) (*env.Task, error) {
	e, err := env.Load(filepath.Join(envDir, envName))
	if err != nil {
		return nil, err
	}
	if taskID == "" {
		return e.Tasks[0], nil
	}
	return e.Task(taskID)
}

func artifactDir(dataDir string) string { return filepath.Join(dataDir, "artifacts") }

func episodeConfig(r rt.Runtime, dataDir string, log *slog.Logger) episode.Config {
	return episode.Config{Runtime: r, ArtifactDir: artifactDir(dataDir), Logger: log}
}
