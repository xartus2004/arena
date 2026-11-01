package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/redteam"
	rt "github.com/xartus2004/arena/runtime"
)

func cmdRedteam(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("redteam", flag.ExitOnError)
	runtimes := fs.String("runtimes", "docker,fake,gvisor,firecracker", "runtimes to report on (columns)")
	envDir := fs.String("envs", "environments", "environments directory")
	dataDir := fs.String("data", "data", "data directory (artifacts)")
	parallel := fs.Int("parallel", 3, "attacks run concurrently")
	out := fs.String("out", "", "also write the markdown table to this file")
	verbose := fs.Bool("v", false, "verbose logs")
	_ = fs.Parse(args)

	log := newLogger(*verbose)
	task, err := loadTask(*envDir, "fix-the-bug", "median")
	if err != nil {
		return err
	}
	var cols []redteam.RuntimeColumn
	baseline := 0.0
	for _, name := range strings.Split(*runtimes, ",") {
		name = strings.TrimSpace(name)
		r, closeFn, err := openRuntime(ctx, name, log)
		if err != nil {
			cols = append(cols, redteam.RuntimeColumn{Name: name, NAWhy: "unavailable: " + firstLine(err.Error())})
			continue
		}
		col, b, err := redteamColumn(ctx, r, task, *dataDir, *parallel, log)
		closeFn()
		if err != nil {
			return err
		}
		if b > 0 {
			baseline = b
		}
		cols = append(cols, col)
	}
	md := redteam.Markdown(cols, baseline)
	fmt.Println(md)
	if *out != "" {
		return os.WriteFile(*out, []byte(md), 0o644)
	}
	return nil
}

func redteamColumn(ctx context.Context, r rt.Runtime, task *env.Task, dataDir string, parallel int, log *slog.Logger) (redteam.RuntimeColumn, float64, error) {
	col := redteam.RuntimeColumn{Name: r.Name()}
	caps := r.Capabilities()
	if !caps.RealExecution {
		col.NAWhy = "executes nothing"
		if r.Name() == "fake" {
			col.NAWhy = "in-memory test double; executes nothing, isolates nothing"
		}
		// Probe whether it can even create a sandbox (stubs cannot).
		if sb, err := r.Create(ctx, rt.Spec{Image: "probe", Timeout: time.Second}); err != nil {
			if errors.Is(err, rt.ErrUnsupported) {
				col.NAWhy = "stub: " + firstLine(err.Error())
			}
		} else {
			_ = sb.Destroy(ctx)
		}
		return col, 0, nil
	}
	cfg := episodeConfig(r, dataDir, log)
	fmt.Fprintf(os.Stderr, "running %d attacks on %s ...\n", len(redteam.Attacks), r.Name())
	results, baseline, err := redteam.Run(ctx, redteam.Options{
		Config: cfg, Task: task, Parallel: parallel,
		Progress: func(res redteam.Result) {
			fmt.Fprintf(os.Stderr, "  %2d %-26s %s\n", res.Attack.ID, res.Attack.Name, res.Verdict)
		},
	})
	if err != nil {
		return col, 0, err
	}
	col.Results = map[int]redteam.Result{}
	for _, res := range results {
		col.Results[res.Attack.ID] = res
	}
	return col, baseline, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
