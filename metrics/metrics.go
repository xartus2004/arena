// Package metrics exposes arena's Prometheus metrics on a private registry.
//
//	arena_episodes_total{env,status,failure}   rate() = episodes/sec
//	arena_reset_seconds                          PROVISION+SETUP latency
//	arena_sandbox_create_seconds{source}         warm vs cold sandbox start
//	arena_phase_seconds{phase}                   per-phase durations
//	arena_infra_failures_total{env}              infra failure rate numerator
//	arena_warm_pool_{hits,misses}_total, arena_warm_pool_idle
//	arena_queue_depth, arena_inflight, arena_rejected_total
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xartus2004/arena/episode"
)

// Metrics holds every collector.
type Metrics struct {
	Registry      *prometheus.Registry
	Episodes      *prometheus.CounterVec
	Reset         prometheus.Histogram
	SandboxCreate *prometheus.HistogramVec
	Phase         *prometheus.HistogramVec
	InfraFailures *prometheus.CounterVec
	PoolHits      prometheus.Counter
	PoolMisses    prometheus.Counter
	PoolIdle      prometheus.Gauge
	QueueDepth    prometheus.Gauge
	Inflight      prometheus.Gauge
	Rejected      prometheus.Counter
}

var latencyBuckets = []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// New registers all metrics on a fresh registry.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Episodes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "arena_episodes_total", Help: "Terminal episode attempts.",
		}, []string{"env", "status", "failure"}),
		Reset: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "arena_reset_seconds", Help: "Time from episode start to first observation (provision + setup).", Buckets: latencyBuckets,
		}),
		SandboxCreate: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "arena_sandbox_create_seconds", Help: "Sandbox acquisition latency by source.", Buckets: latencyBuckets,
		}, []string{"source"}),
		Phase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "arena_phase_seconds", Help: "Time spent in each lifecycle phase.", Buckets: latencyBuckets,
		}, []string{"phase"}),
		InfraFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "arena_infra_failures_total", Help: "Episode attempts that failed for infrastructure reasons.",
		}, []string{"env"}),
		PoolHits:   prometheus.NewCounter(prometheus.CounterOpts{Name: "arena_warm_pool_hits_total", Help: "Sandboxes served from the warm pool."}),
		PoolMisses: prometheus.NewCounter(prometheus.CounterOpts{Name: "arena_warm_pool_misses_total", Help: "Sandboxes created cold."}),
		PoolIdle:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "arena_warm_pool_idle", Help: "Idle warm sandboxes."}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{Name: "arena_queue_depth", Help: "Queued episodes."}),
		Inflight:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "arena_inflight", Help: "Running episodes."}),
		Rejected:   prometheus.NewCounter(prometheus.CounterOpts{Name: "arena_rejected_total", Help: "Episodes rejected by backpressure."}),
	}
	m.Registry.MustRegister(m.Episodes, m.Reset, m.SandboxCreate, m.Phase, m.InfraFailures,
		m.PoolHits, m.PoolMisses, m.PoolIdle, m.QueueDepth, m.Inflight, m.Rejected,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ObserveOutcome records a terminal attempt. Nil-safe.
func (m *Metrics) ObserveOutcome(o *episode.Outcome) {
	if m == nil || o == nil {
		return
	}
	m.Episodes.WithLabelValues(o.Env, string(o.Status), string(o.Failure)).Inc()
	if o.Failure == episode.FailureInfra {
		m.InfraFailures.WithLabelValues(o.Env).Inc()
	}
	for phase, d := range o.Phases {
		m.Phase.WithLabelValues(string(phase)).Observe(d.Seconds())
	}
	if _, ok := o.Phases[episode.StudentTurn]; ok {
		m.Reset.Observe((o.Phases[episode.Provision] + o.Phases[episode.Setup]).Seconds())
	}
}

// ObserveCreate records a sandbox acquisition. Nil-safe.
func (m *Metrics) ObserveCreate(warm bool, d time.Duration) {
	if m == nil {
		return
	}
	src := "cold"
	if warm {
		src = "warm"
		m.PoolHits.Inc()
	} else {
		m.PoolMisses.Inc()
	}
	m.SandboxCreate.WithLabelValues(src).Observe(d.Seconds())
}
