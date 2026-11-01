package episode

import (
	"context"
	"errors"
	"fmt"

	"github.com/xartus2004/arena/env"
	rt "github.com/xartus2004/arena/runtime"
)

// RunOnce drives one attempt with policy until it is terminal.
func RunOnce(ctx context.Context, ep *Episode, policy Policy) (*Outcome, error) {
	defer ep.Close(ctx)
	obs, err := ep.Start(ctx)
	if err != nil {
		return ep.Outcome(), err
	}
	for !obs.Done {
		if err := ctx.Err(); err != nil {
			ep.Abort(ctx, rt.Infra("episode cancelled", err))
			return ep.Outcome(), err
		}
		call, err := policy.Act(ctx, obs)
		if err != nil {
			err = fmt.Errorf("policy: %w", err)
			if ctx.Err() != nil {
				err = rt.Infra("episode cancelled", ctx.Err())
			}
			ep.Abort(ctx, err)
			return ep.Outcome(), err
		}
		obs, err = ep.Step(ctx, call)
		if err != nil {
			return ep.Outcome(), err
		}
	}
	return obs.Outcome, nil
}

// Run executes an episode, retrying infrastructure failures up to
// maxAttempts times under the same episode ID. Agent failures (bad answers,
// custody violations) are final and never retried. newPolicy is called once
// per attempt so policies start fresh.
func Run(ctx context.Context, cfg Config, task *env.Task, id string, maxAttempts int, newPolicy func() Policy) (*Outcome, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var last *Outcome
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ep := New(cfg, task, id, attempt)
		out, err := RunOnce(ctx, ep, newPolicy())
		last, lastErr = out, err
		if err == nil {
			return out, nil
		}
		if !rt.IsInfra(err) || ctx.Err() != nil {
			break
		}
		ep.log.Warn("retrying after infra failure", "err", err)
	}
	if last == nil {
		last = &Outcome{EpisodeID: id, Status: StatusFailed, Failure: FailureInfra}
	}
	if lastErr == nil {
		lastErr = errors.New("episode failed")
	}
	return last, lastErr
}
