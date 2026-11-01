package main

import (
	"context"
	"errors"
)

func cmdServe(context.Context, []string) error { return errors.New("serve: arrives in M4") }
func cmdRun(context.Context, []string) error   { return errors.New("run: arrives in M4") }
func cmdBench(context.Context, []string) error { return errors.New("bench: arrives in M5") }
