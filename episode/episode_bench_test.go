package episode

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkLifecycleFake measures the platform's own overhead for a full
// episode (state machine, custody, packing, grading plumbing) with the
// in-memory runtime, i.e. everything except real sandbox work.
func BenchmarkLifecycleFake(b *testing.B) {
	h := newHarness(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ep := New(h.cfg, h.task, fmt.Sprintf("bench-%d", i), 1)
		if _, err := ep.Start(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := ep.Step(ctx, writeCall("/workspace/out/answer.txt", secret)); err != nil {
			b.Fatal(err)
		}
		obs, err := ep.Step(ctx, ToolCall{Tool: ToolSubmit})
		if err != nil || obs.Outcome.Reward != 1 {
			b.Fatalf("%v %+v", err, obs.Outcome)
		}
	}
}
