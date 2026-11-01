package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xartus2004/arena/metrics"
	rt "github.com/xartus2004/arena/runtime"
)

// PoolConfig configures a warm pool.
type PoolConfig struct {
	// Size is the number of idle sandboxes kept per distinct spec.
	Size int
	// MaxIdle discards warm sandboxes older than this (default: half the
	// spec's lifetime, so a handed-out sandbox always has time left).
	MaxIdle time.Duration
	Metrics *metrics.Metrics
	Logger  *slog.Logger
}

type warm struct {
	sb      rt.Sandbox
	created time.Time
}

type poolKey struct {
	spec    rt.Spec
	idle    []warm
	filling int
}

// Pool is a runtime.Runtime that keeps pre-created sandboxes per spec and
// refills them in the background. Sandboxes are single-use: a handed-out
// sandbox never returns to the pool.
type Pool struct {
	inner rt.Runtime
	cfg   PoolConfig

	mu   sync.Mutex
	keys map[string]*poolKey

	kick   chan struct{}
	stop   chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool
	hits   atomic.Int64
	misses atomic.Int64
}

// NewPool wraps inner. Call Close to stop refilling and destroy idle
// sandboxes.
func NewPool(inner rt.Runtime, cfg PoolConfig) *Pool {
	if cfg.Size < 0 {
		cfg.Size = 0
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	p := &Pool{inner: inner, cfg: cfg, keys: map[string]*poolKey{}, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	p.wg.Add(1)
	go p.loop()
	return p
}

// Name implements runtime.Runtime.
func (p *Pool) Name() string { return p.inner.Name() }

// Capabilities implements runtime.Runtime.
func (p *Pool) Capabilities() rt.Capabilities { return p.inner.Capabilities() }

// Hits and Misses report pool effectiveness.
func (p *Pool) Hits() int64   { return p.hits.Load() }
func (p *Pool) Misses() int64 { return p.misses.Load() }

// Idle returns the number of idle warm sandboxes across all specs.
func (p *Pool) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, k := range p.keys {
		n += len(k.idle)
	}
	return n
}

func normalise(spec rt.Spec) rt.Spec {
	spec.Labels = map[string]string{"arena.role": "warm"}
	return spec
}

// Warm registers spec so the pool keeps Size sandboxes of it ready.
func (p *Pool) Warm(spec rt.Spec) {
	p.mu.Lock()
	k := spec.Key()
	if _, ok := p.keys[k]; !ok {
		p.keys[k] = &poolKey{spec: normalise(spec)}
	}
	p.mu.Unlock()
	p.poke()
}

func (p *Pool) poke() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *Pool) maxIdle(spec rt.Spec) time.Duration {
	if p.cfg.MaxIdle > 0 {
		return p.cfg.MaxIdle
	}
	return spec.Timeout / 2
}

// Create implements runtime.Runtime: a warm sandbox if one is ready,
// otherwise a cold one. Either way the spec becomes warmed.
func (p *Pool) Create(ctx context.Context, spec rt.Spec) (rt.Sandbox, error) {
	start := time.Now()
	key := spec.Key()
	var got rt.Sandbox
	p.mu.Lock()
	k, ok := p.keys[key]
	if !ok {
		k = &poolKey{spec: normalise(spec)}
		p.keys[key] = k
	}
	for len(k.idle) > 0 && got == nil {
		w := k.idle[len(k.idle)-1]
		k.idle = k.idle[:len(k.idle)-1]
		if time.Since(w.created) < p.maxIdle(k.spec) {
			got = w.sb
		} else {
			go func() { _ = w.sb.Destroy(context.Background()) }()
		}
	}
	p.mu.Unlock()
	p.poke()
	if got != nil {
		p.hits.Add(1)
		p.cfg.Metrics.ObserveCreate(true, time.Since(start))
		p.gauge()
		return got, nil
	}
	sb, err := p.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	p.misses.Add(1)
	p.cfg.Metrics.ObserveCreate(false, time.Since(start))
	return sb, nil
}

func (p *Pool) gauge() {
	if p.cfg.Metrics != nil {
		p.cfg.Metrics.PoolIdle.Set(float64(p.Idle()))
	}
}

func (p *Pool) loop() {
	defer p.wg.Done()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-p.kick:
		case <-t.C:
		}
		p.refill()
	}
}

// refill reaps expired sandboxes and starts creations to reach Size.
func (p *Pool) refill() {
	var expired []rt.Sandbox
	type job struct {
		key  string
		spec rt.Spec
	}
	var jobs []job
	p.mu.Lock()
	for key, k := range p.keys {
		keep := k.idle[:0]
		for _, w := range k.idle {
			if time.Since(w.created) >= p.maxIdle(k.spec) {
				expired = append(expired, w.sb)
			} else {
				keep = append(keep, w)
			}
		}
		k.idle = keep
		for n := len(k.idle) + k.filling; n < p.cfg.Size; n++ {
			k.filling++
			jobs = append(jobs, job{key, k.spec})
		}
	}
	p.mu.Unlock()
	for _, sb := range expired {
		_ = sb.Destroy(context.Background())
	}
	for _, j := range jobs {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			sb, err := p.inner.Create(ctx, j.spec)
			p.mu.Lock()
			k := p.keys[j.key]
			k.filling--
			if err == nil && !p.closed.Load() {
				k.idle = append(k.idle, warm{sb: sb, created: time.Now()})
				sb = nil
			}
			p.mu.Unlock()
			if err != nil {
				p.cfg.Logger.Warn("warm pool refill failed", "err", err)
			}
			if sb != nil {
				_ = sb.Destroy(ctx)
			}
			p.gauge()
		}()
	}
	p.gauge()
}

// Close stops refilling, waits for in-progress creations and destroys idle
// sandboxes. Sandboxes already handed out are owned by their callers.
func (p *Pool) Close(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.stop)
	p.wg.Wait()
	p.mu.Lock()
	var all []rt.Sandbox
	for _, k := range p.keys {
		for _, w := range k.idle {
			all = append(all, w.sb)
		}
		k.idle = nil
	}
	p.mu.Unlock()
	var firstErr error
	for _, sb := range all {
		if err := sb.Destroy(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.gauge()
	return firstErr
}
