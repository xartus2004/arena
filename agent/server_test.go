package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xartus2004/arena/env"
	"github.com/xartus2004/arena/episode"
	"github.com/xartus2004/arena/internal/testenv"
	"github.com/xartus2004/arena/runtime/fake"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type slots struct {
	max, cur atomic.Int32
}

func (s *slots) TryAcquire() bool {
	if s.cur.Add(1) > s.max.Load() {
		s.cur.Add(-1)
		return false
	}
	return true
}
func (s *slots) Release() { s.cur.Add(-1) }

func newServer(t *testing.T, mut func(*ServerConfig)) (*Client, *fake.Runtime, *Server, *[]*episode.Outcome) {
	t.Helper()
	e, r := testenv.New(t)
	var mu sync.Mutex
	var outcomes []*episode.Outcome
	cfg := ServerConfig{
		Envs: map[string]*env.Environment{"toy": e},
		Episode: episode.Config{Runtime: r, ArtifactDir: filepath.Join(t.TempDir(), "a"),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		WatchdogInterval: 10 * time.Millisecond,
		OnOutcome: func(o *episode.Outcome) {
			mu.Lock()
			outcomes = append(outcomes, o)
			mu.Unlock()
		},
	}
	if mut != nil {
		mut(&cfg)
	}
	s := NewServer(cfg)
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		s.Close(context.Background())
	})
	return &Client{BaseURL: ts.URL, HTTP: ts.Client()}, r, s, &outcomes
}

func TestAPIDriveHonestAndCheater(t *testing.T) {
	c, r, _, outcomes := newServer(t, nil)
	ctx := context.Background()
	honest := &Script{Calls: []episode.ToolCall{Write(testenv.AnswerPath, testenv.Secret)}}
	out, err := c.Drive(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "h1"}, honest)
	if err != nil || out.Reward != 1 {
		t.Fatalf("honest: %+v %v", out, err)
	}
	// The instructions arrive in the first observation.
	if honest.Seen[0].Instructions == "" {
		t.Fatal("no instructions in first observation")
	}
	cheat := &Script{Calls: []episode.ToolCall{Read("/grader/answer.txt"), Bash("cat /grader/answer.txt")}}
	out, err = c.Drive(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "c1"}, cheat)
	if err != nil || out.Reward != 0 {
		t.Fatalf("cheat: %+v %v", out, err)
	}
	for _, o := range cheat.Seen {
		if o.Result != nil && o.Result.Output == testenv.Secret {
			t.Fatal("secret leaked to the student")
		}
	}
	v, err := c.Get(ctx, "h1")
	if err != nil || v.State != episode.Done || len(v.Transitions) != 6 {
		t.Fatalf("get: %+v %v", v, err)
	}
	if len(r.Live()) != 0 {
		t.Fatal("sandboxes leaked")
	}
	if len(*outcomes) != 2 {
		t.Fatalf("OnOutcome calls = %d", len(*outcomes))
	}
}

func TestAPIIdempotentCreateAndRetry(t *testing.T) {
	c, r, _, _ := newServer(t, func(c *ServerConfig) { c.MaxAttempts = 3 })
	ctx := context.Background()
	r.FailCreate.Store(1)
	_, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "x"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || !ae.Infra {
		t.Fatalf("want infra 503, got %v", err)
	}
	// Same ID: new attempt.
	cr, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "x"})
	if err != nil || cr.Attempt != 2 || cr.Observation == nil {
		t.Fatalf("retry: %+v %v", cr, err)
	}
	// Same ID while running: same attempt, no new sandbox.
	before := r.Created()
	cr2, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "x"})
	if err != nil || cr2.Attempt != 2 || r.Created() != before {
		t.Fatalf("idempotent: %+v %v created=%d", cr2, err, r.Created())
	}
	if _, err := c.Step(ctx, "x", Submit()); err != nil {
		t.Fatal(err)
	}
	cr3, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "x"})
	if err != nil || cr3.Outcome == nil || cr3.Attempt != 2 {
		t.Fatalf("after done: %+v %v", cr3, err)
	}
}

func TestAPIErrorsAndBackpressure(t *testing.T) {
	adm := &slots{}
	adm.max.Store(1)
	c, _, _, _ := newServer(t, func(c *ServerConfig) { c.Admission = adm })
	ctx := context.Background()
	tests := []struct {
		name   string
		req    CreateRequest
		status int
	}{
		{"unknown env", CreateRequest{Env: "nope", Task: "answer"}, 404},
		{"unknown task", CreateRequest{Env: "toy", Task: "nope"}, 404},
		{"bad id", CreateRequest{Env: "toy", Task: "answer", ID: "a/b"}, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Create(ctx, tc.req)
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != tc.status {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "b"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %v", err)
	}
	if _, err := c.Step(ctx, "a", Submit()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "b"}); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	if _, err := c.Step(ctx, "a", Bash("ls")); !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("step after done: %v", err)
	}
	if _, err := c.Step(ctx, "zzz", Bash("ls")); !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("unknown episode: %v", err)
	}
}

func TestAPIWatchdogExpiresIdleEpisode(t *testing.T) {
	now := atomic.Int64{}
	now.Store(time.Unix(1_700_000_000, 0).UnixNano())
	c, r, _, outcomes := newServer(t, func(c *ServerConfig) {
		c.Episode.Clock = func() time.Time { return time.Unix(0, now.Load()) }
	})
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{Env: "toy", Task: "answer", ID: "idle"}); err != nil {
		t.Fatal(err)
	}
	now.Add(int64(2 * time.Minute))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := c.Get(ctx, "idle")
		if v.Outcome != nil {
			if v.Outcome.Metadata["forced_submit"] != "time_limit" {
				t.Fatalf("outcome: %+v", v.Outcome)
			}
			if len(r.Live()) != 0 || len(*outcomes) != 1 {
				t.Fatal("watchdog did not clean up")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watchdog never expired the idle episode")
}

func TestServerCloseAbortsLiveEpisodes(t *testing.T) {
	c, r, s, _ := newServer(t, nil)
	if _, err := c.Create(context.Background(), CreateRequest{Env: "toy", Task: "answer", ID: "live"}); err != nil {
		t.Fatal(err)
	}
	s.Close(context.Background())
	if len(r.Live()) != 0 {
		t.Fatal("Close must destroy live sandboxes")
	}
}
