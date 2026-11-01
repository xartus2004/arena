package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/xartus2004/arena/episode"
)

func TestObserve(t *testing.T) {
	m := New()
	m.ObserveOutcome(&episode.Outcome{Env: "e", Status: episode.StatusDone,
		Phases: map[episode.State]time.Duration{episode.Provision: time.Second, episode.Setup: time.Second, episode.StudentTurn: time.Second}})
	m.ObserveOutcome(&episode.Outcome{Env: "e", Status: episode.StatusFailed, Failure: episode.FailureInfra})
	m.ObserveCreate(true, time.Millisecond)
	m.ObserveCreate(false, time.Second)
	var nilM *Metrics
	nilM.ObserveOutcome(&episode.Outcome{}) // must not panic
	nilM.ObserveCreate(true, 0)

	if got := testutil.ToFloat64(m.InfraFailures.WithLabelValues("e")); got != 1 {
		t.Fatalf("infra failures = %v", got)
	}
	if got := testutil.ToFloat64(m.PoolHits); got != 1 {
		t.Fatalf("hits = %v", got)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{"arena_episodes_total", "arena_reset_seconds_count 1", `arena_sandbox_create_seconds_count{source="warm"} 1`, "arena_warm_pool_misses_total 1"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q", want)
		}
	}
}
