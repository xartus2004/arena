package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/xartus2004/arena/metrics"
	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/fake"
)

var poolSpec = rt.Spec{Image: "fake", Timeout: time.Minute, Workdirs: []string{"/workspace"}}

func TestPoolWarmHitsAndRefill(t *testing.T) {
	inner := fake.New()
	inner.CreateDelay = 20 * time.Millisecond
	m := metrics.New()
	p := NewPool(inner, PoolConfig{Size: 2, Metrics: m, Logger: quiet})
	ctx := context.Background()
	p.Warm(poolSpec)
	waitFor(t, func() bool { return p.Idle() == 2 })

	start := time.Now()
	sb, err := p.Create(ctx, poolSpec)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 15*time.Millisecond {
		t.Fatalf("warm create took %v", time.Since(start))
	}
	if testutil.ToFloat64(m.PoolHits) != 1 {
		t.Fatal("hit not counted")
	}
	waitFor(t, func() bool { return p.Idle() == 2 }) // refilled in background
	_ = sb.Destroy(ctx)

	// A different spec is a miss (cold) and is then warmed too.
	other := poolSpec
	other.MemoryBytes = 1 << 20
	if _, err := p.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(m.PoolMisses) != 1 {
		t.Fatal("miss not counted")
	}
	waitFor(t, func() bool { return p.Idle() == 4 })
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the two handed-out sandboxes remain (callers own them).
	if n := len(inner.Live()); n != 1 {
		t.Fatalf("live after close = %d", n)
	}
}

func TestPoolExpiresIdle(t *testing.T) {
	inner := fake.New()
	p := NewPool(inner, PoolConfig{Size: 1, MaxIdle: 30 * time.Millisecond, Logger: quiet})
	defer p.Close(context.Background())
	p.Warm(poolSpec)
	waitFor(t, func() bool { return p.Idle() == 1 })
	first := inner.Live()[0]
	waitFor(t, func() bool { return first.Destroyed() })
	waitFor(t, func() bool { return p.Idle() == 1 })
}

func TestPoolSandboxesAreSingleUse(t *testing.T) {
	inner := fake.New()
	p := NewPool(inner, PoolConfig{Size: 1, Logger: quiet})
	defer p.Close(context.Background())
	p.Warm(poolSpec)
	waitFor(t, func() bool { return p.Idle() == 1 })
	a, _ := p.Create(context.Background(), poolSpec)
	waitFor(t, func() bool { return p.Idle() == 1 })
	b, _ := p.Create(context.Background(), poolSpec)
	if a.ID() == b.ID() {
		t.Fatal("a sandbox was handed out twice")
	}
	_ = a.Destroy(context.Background())
	_ = b.Destroy(context.Background())
}
