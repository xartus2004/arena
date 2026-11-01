// Command arena runs RL environments for AI agents with cheat-proof grading.
//
//	arena serve    HTTP episode API + Prometheus metrics
//	arena run      run N episodes of a scripted agent through the scheduler
//	arena redteam  run the adversarial suite and print the verdict table
//	arena bench    throughput / reset-latency / custody benchmarks
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: arena <command> [flags]

commands:
  serve     serve the episode HTTP API and /metrics
  run       run episodes with a scripted agent
  redteam   run the red-team suite and print a markdown table
  bench     run benchmarks and write bench/RESULTS.md

run 'arena <command> -h' for flags`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmds := map[string]func(context.Context, []string) error{
		"serve":   cmdServe,
		"run":     cmdRun,
		"redteam": cmdRedteam,
		"bench":   cmdBench,
	}
	f, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := f(ctx, os.Args[2:]); err != nil {
		slog.Error(os.Args[1]+" failed", "err", err)
		os.Exit(1)
	}
}
