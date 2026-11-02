package episode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xartus2004/arena/custody"
	"github.com/xartus2004/arena/env"
	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/fake"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const envYAML = `
name: toy
sandbox:
  image: fake
  memory: 64Mi
  pids: 32
  disk: 8Mi
limits:
  max_files: 10
  max_bytes: 1Ki
tasks:
  - id: answer
    instructions: write the answer to /workspace/out/answer.txt
    files: files
    hidden: hidden
    submission: /workspace/out
    setup:
      - run: chown -R 1000:1000 /workspace
    grader:
      command: ["python3", "/grader/grade.py", "/submission"]
    time_limit: 1m
    max_steps: 5
`

const secret = "THE-ANSWER-IS-42"

// toyGrader implements the grader of the toy env inside the fake runtime:
// reward 1 if /submission/out/answer.txt equals /grader/answer.txt.
func toyGrader(log *eventLog) fake.Handler {
	return func(ctx context.Context, sb *fake.Sandbox, req rt.ExecRequest) (rt.Result, bool, error) {
		if len(req.Cmd) >= 2 && req.Cmd[0] == "python3" && req.Cmd[1] == "/grader/grade.py" {
			log.add("grade:" + sb.ID())
			want, _ := sb.ReadFile("/grader/answer.txt")
			got, ok := sb.ReadFile("/submission/out/answer.txt")
			reward := 0.0
			if ok && strings.TrimSpace(string(got)) == strings.TrimSpace(string(want)) {
				reward = 1
			}
			out := fmt.Sprintf("some log line\n{\"reward\": %g, \"metadata\": {\"checked\": true}}\n", reward)
			return rt.Result{Stdout: []byte(out)}, true, nil
		}
		if len(req.Cmd) == 3 && req.Cmd[0] == "sh" && req.Cmd[2] == "sleep-forever" {
			<-ctx.Done()
			return rt.Result{}, true, nil
		}
		return rt.Result{}, false, nil
	}
}

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) { l.mu.Lock(); l.ev = append(l.ev, s); l.mu.Unlock() }

type memRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *memRecorder) Record(e Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }
func (r *memRecorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

type harness struct {
	task *env.Task
	rt   *fake.Runtime
	cfg  Config
	rec  *memRecorder
	log  *eventLog
	now  time.Time
	mu   sync.Mutex
}

func (h *harness) clock() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func newHarness(t testing.TB) *harness {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"env.yaml":          envYAML,
		"files/README":      "hello",
		"hidden/answer.txt": secret,
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e, err := env.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := e.Task("answer")
	h := &harness{task: task, rt: fake.New(), rec: &memRecorder{}, log: &eventLog{}, now: time.Unix(1_700_000_000, 0)}
	h.rt.Handler = toyGrader(h.log)
	h.cfg = Config{Runtime: h.rt, Recorder: h.rec, ArtifactDir: filepath.Join(t.TempDir(), "artifacts"), Clock: h.clock,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return h
}

func writeCall(p, content string) ToolCall {
	return ToolCall{Tool: ToolWriteFile, Path: p, Content: content}
}

func states(ep *Episode) []State {
	var out []State
	for _, tr := range ep.Transitions() {
		out = append(out, tr.To)
	}
	return out
}

func studentSandbox(t *testing.T, h *harness) *fake.Sandbox {
	t.Helper()
	live := h.rt.Live()
	if len(live) != 1 {
		t.Fatalf("want 1 live sandbox, got %d", len(live))
	}
	return live[0]
}

func TestHappyPath(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-1", 1)
	obs, err := ep.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Instructions == "" || obs.State != StudentTurn {
		t.Fatalf("bad first observation: %+v", obs)
	}
	sb := studentSandbox(t, h)
	// Hidden files never enter the student sandbox.
	if _, ok := sb.Get("/grader/answer.txt"); ok {
		t.Fatal("hidden file leaked into student sandbox")
	}
	if b, ok := sb.ReadFile("/workspace/README"); !ok || string(b) != "hello" {
		t.Fatalf("student files not copied: %q", b)
	}

	obs, err = ep.Step(ctx, writeCall("/workspace/out/answer.txt", secret))
	if err != nil || obs.Result.ExitCode != 0 {
		t.Fatalf("write: %+v %v", obs.Result, err)
	}
	obs, err = ep.Step(ctx, ToolCall{Tool: ToolReadFile, Path: "/workspace/out/answer.txt"})
	if err != nil || obs.Result.Output != secret {
		t.Fatalf("read: %+v %v", obs.Result, err)
	}
	obs, err = ep.Step(ctx, ToolCall{Tool: ToolSubmit})
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Done || obs.Outcome == nil || obs.Outcome.Reward != 1 || obs.Outcome.Status != StatusDone {
		t.Fatalf("outcome: %+v", obs.Outcome)
	}
	if obs.Outcome.Metadata["checked"] != true {
		t.Fatalf("grader metadata lost: %+v", obs.Outcome.Metadata)
	}
	want := []State{Provision, Setup, StudentTurn, Custody, Grade, Done}
	if fmt.Sprint(states(ep)) != fmt.Sprint(want) {
		t.Fatalf("transitions = %v, want %v", states(ep), want)
	}
	if len(h.rt.Live()) != 0 {
		t.Fatal("all sandboxes must be destroyed at the end")
	}
	if h.rt.Created() != 2 {
		t.Fatalf("want separate student + grader sandboxes, created %d", h.rt.Created())
	}
	if obs.Outcome.Manifest == nil || obs.Outcome.Manifest.TotalBytes != int64(len(secret)) {
		t.Fatalf("manifest: %+v", obs.Outcome.Manifest)
	}
	if _, err := os.Stat(obs.Outcome.ArtifactPath); err != nil {
		t.Fatalf("artifact not saved: %v", err)
	}
	types := strings.Join(h.rec.types(), ",")
	for _, want := range []string{"transition", "tool_call", "custody", "grade"} {
		if !strings.Contains(types, want) {
			t.Fatalf("recorder missing %s: %s", want, types)
		}
	}
	// Steps after DONE are rejected.
	if _, err := ep.Step(ctx, ToolCall{Tool: ToolBash, Command: "true"}); !errors.Is(err, ErrNotStudentTurn) {
		t.Fatalf("step after done: %v", err)
	}
}

func TestWrongAnswerIsAgentFailure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-wrong", 1)
	mustStart(t, ep)
	_, _ = ep.Step(ctx, writeCall("/workspace/out/answer.txt", "41"))
	obs, err := ep.Step(ctx, ToolCall{Tool: ToolSubmit})
	if err != nil {
		t.Fatal(err)
	}
	if obs.Outcome.Reward != 0 || obs.Outcome.Status != StatusDone || obs.Outcome.Failure != "" {
		t.Fatalf("outcome: %+v", obs.Outcome)
	}
}

func mustStart(t *testing.T, ep *Episode) {
	t.Helper()
	if _, err := ep.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCustodyKillsStudentBeforeCopy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-bg", 1)
	mustStart(t, ep)
	sb := studentSandbox(t, h)
	_, _ = ep.Step(ctx, writeCall("/workspace/out/answer.txt", secret))
	sb.Spawn(rt.Student.UID) // background process
	sb.Spawn(rt.Student.UID)
	obs, err := ep.Step(ctx, ToolCall{Tool: ToolSubmit})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Procs(rt.Student.UID) != 0 {
		t.Fatal("student processes survived custody")
	}
	if !sb.Destroyed() {
		t.Fatal("student sandbox must be destroyed before grading")
	}
	if obs.Outcome.Reward != 1 {
		t.Fatalf("reward = %v", obs.Outcome.Reward)
	}
}

func TestCustodyViolations(t *testing.T) {
	tests := []struct {
		name  string
		shape func(sb *fake.Sandbox)
		kind  custody.Kind
	}{
		{"symlink to hidden", func(sb *fake.Sandbox) {
			sb.Put("/workspace/out/answer.txt", fake.Node{Kind: fake.Symlink, Target: "/grader/answer.txt"})
		}, custody.KindSymlink},
		{"fifo", func(sb *fake.Sandbox) {
			sb.Put("/workspace/out/answer.txt", fake.Node{Kind: fake.FIFO})
		}, custody.KindSpecial},
		{"sparse", func(sb *fake.Sandbox) {
			sb.Put("/workspace/out/answer.txt", fake.Node{Kind: fake.File, Size: 1 << 40})
		}, custody.KindFileTooLarge},
		{"missing", func(sb *fake.Sandbox) {}, custody.KindMissing},
		{"stubborn process", func(sb *fake.Sandbox) {
			sb.Put("/workspace/out/answer.txt", fake.Node{Kind: fake.File, Data: []byte(secret), Mode: 0o644})
			sb.Spawn(rt.Student.UID)
			sb.SetStubborn(rt.Student.UID)
		}, custody.KindProcesses},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			ep := New(h.cfg, h.task, "ep-v", 1)
			mustStart(t, ep)
			sb := studentSandbox(t, h)
			tc.shape(sb)
			obs, err := ep.Step(ctx, ToolCall{Tool: ToolSubmit})
			if err != nil {
				t.Fatal(err)
			}
			o := obs.Outcome
			if o.Reward != 0 || o.Status != StatusDone || o.Violation == nil || o.Violation.Kind != tc.kind {
				t.Fatalf("outcome: %+v violation %+v", o, o.Violation)
			}
			if fmt.Sprint(states(ep)) != fmt.Sprint([]State{Provision, Setup, StudentTurn, Custody, Done}) {
				t.Fatalf("grade must be skipped: %v", states(ep))
			}
			if len(h.log.ev) != 0 {
				t.Fatal("grader must not run on a rejected submission")
			}
			if len(h.rt.Live()) != 0 {
				t.Fatal("sandboxes leaked")
			}
		})
	}
}

func TestInfraFailureAndRetry(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.rt.FailCreate.Store(1)
	ep := New(h.cfg, h.task, "ep-infra", 1)
	_, err := ep.Start(ctx)
	if !rt.IsInfra(err) {
		t.Fatalf("want infra error, got %v", err)
	}
	o := ep.Outcome()
	if o == nil || o.Status != StatusFailed || o.Failure != FailureInfra {
		t.Fatalf("outcome: %+v", o)
	}

	// Run retries infra failures with the same episode ID.
	h.rt.FailCreate.Store(2)
	out, err := Run(ctx, h.cfg, h.task, "ep-retry", 3, func() Policy {
		return &scripted{calls: []ToolCall{
			writeCall("/workspace/out/answer.txt", secret), {Tool: ToolSubmit},
		}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reward != 1 || out.Attempt != 3 || out.EpisodeID != "ep-retry" {
		t.Fatalf("retry outcome: %+v", out)
	}

	// Exhausted retries stay failed.
	h.rt.FailCreate.Store(5)
	out, err = Run(ctx, h.cfg, h.task, "ep-exhaust", 2, func() Policy { return &scripted{} })
	if err == nil || out.Failure != FailureInfra || out.Attempt != 2 {
		t.Fatalf("exhausted: %+v %v", out, err)
	}
}

func TestExecInfraFailureDuringTurn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-exec", 1)
	mustStart(t, ep)
	studentSandbox(t, h).SetFailExec(true)
	_, err := ep.Step(ctx, ToolCall{Tool: ToolBash, Command: "ls"})
	if !rt.IsInfra(err) {
		t.Fatalf("want infra, got %v", err)
	}
	if ep.State() != Failed || len(h.rt.Live()) != 0 {
		t.Fatalf("state %s, live %d", ep.State(), len(h.rt.Live()))
	}
}

// A sandbox killed by the agent's own resource use (e.g. OOM under gVisor)
// is an agent failure: reward 0 with a violation, never an infra retry.
func TestResourceExhaustionIsAgentFailure(t *testing.T) {
	for _, where := range []string{"during tool call", "during custody"} {
		t.Run(where, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			ep := New(h.cfg, h.task, "ep-oom", 1)
			mustStart(t, ep)
			sb := studentSandbox(t, h)
			var obs Observation
			var err error
			if where == "during tool call" {
				sb.SetExhausted(true)
				obs, err = ep.Step(ctx, ToolCall{Tool: ToolBash, Command: "hog"})
			} else {
				_, _ = ep.Step(ctx, writeCall("/workspace/out/answer.txt", secret))
				sb.SetExhausted(true)
				obs, err = ep.Step(ctx, ToolCall{Tool: ToolSubmit})
			}
			if err != nil || rt.IsInfra(err) {
				t.Fatalf("err = %v", err)
			}
			o := obs.Outcome
			if !obs.Done || o.Status != StatusDone || o.Reward != 0 || o.Violation == nil || o.Violation.Kind != custody.KindResources {
				t.Fatalf("outcome %+v violation %+v", o, o.Violation)
			}
			if len(h.rt.Live()) != 0 {
				t.Fatal("sandboxes leaked")
			}
		})
	}
	if rt.IsInfra(rt.Infra("x", fmt.Errorf("wrapped: %w", rt.ErrResourceExhausted))) {
		t.Fatal("Infra must not relabel resource exhaustion")
	}
}

func TestTimeLimitForcesSubmission(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-time", 1)
	mustStart(t, ep)
	_, _ = ep.Step(ctx, writeCall("/workspace/out/answer.txt", secret))
	h.advance(2 * time.Minute)
	obs, err := ep.Step(ctx, ToolCall{Tool: ToolBash, Command: "echo late"})
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Done || obs.Outcome.Metadata["forced_submit"] != "time_limit" {
		t.Fatalf("outcome: %+v", obs.Outcome)
	}
	// Work done before the deadline still counts.
	if obs.Outcome.Reward != 1 {
		t.Fatalf("reward %v", obs.Outcome.Reward)
	}
}

func TestMaxSteps(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-steps", 1)
	mustStart(t, ep)
	var obs Observation
	var err error
	for i := 0; i < 6 && !obs.Done; i++ {
		obs, err = ep.Step(ctx, ToolCall{Tool: ToolBash, Command: "echo hi"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !obs.Done || obs.Outcome.Metadata["forced_submit"] != "max_steps" {
		t.Fatalf("outcome: %+v", obs.Outcome)
	}
}

func TestToolValidation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ep := New(h.cfg, h.task, "ep-tools", 1)
	if _, err := ep.Step(ctx, ToolCall{Tool: ToolBash}); !errors.Is(err, ErrNotStudentTurn) {
		t.Fatalf("step before start: %v", err)
	}
	mustStart(t, ep)
	tests := []struct {
		call    ToolCall
		wantErr string
	}{
		{ToolCall{Tool: "rm_rf"}, "unknown tool"},
		{ToolCall{Tool: ToolBash}, "command is required"},
		{ToolCall{Tool: ToolReadFile}, "path is required"},
		{ToolCall{Tool: ToolWriteFile, Path: "relative"}, "absolute"},
	}
	for _, tc := range tests {
		obs, err := ep.Step(ctx, tc.call)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(obs.Result.Error, tc.wantErr) {
			t.Fatalf("%+v: got %q", tc.call, obs.Result.Error)
		}
	}
	// Student cannot read root-only files through read_file.
	studentSandbox(t, h).Put("/etc/shadow", fake.Node{Kind: fake.File, Mode: 0o600, Data: []byte("root:x")})
	obs, _ := ep.Step(ctx, ToolCall{Tool: ToolReadFile, Path: "/etc/shadow"})
	if obs.Result.ExitCode == 0 || strings.Contains(obs.Result.Output, "root:x") {
		t.Fatalf("read_file escalated: %+v", obs.Result)
	}
	ep.Close(ctx)
	if len(h.rt.Live()) != 0 {
		t.Fatal("Close must destroy sandboxes")
	}
}

func TestToolTimeoutUsesRemainingTime(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.cfg.ToolTimeout = 50 * time.Millisecond
	ep := New(h.cfg, h.task, "ep-tt", 1)
	mustStart(t, ep)
	obs, err := ep.Step(ctx, ToolCall{Tool: ToolBash, Command: "sleep-forever"})
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Result.TimedOut {
		t.Fatalf("want timeout: %+v", obs.Result)
	}
}

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		reward float64
		errSub string
	}{
		{"simple", `{"reward": 0.5}`, 0.5, ""},
		{"last line wins", "{\"reward\": 0.1}\nnoise\n{\"reward\": 0.9, \"metadata\": {\"a\": 1}}\n", 0.9, ""},
		{"clamp high", `{"reward": 7}`, 1, ""},
		{"clamp low", `{"reward": -3}`, 0, ""},
		{"missing", "all good!", 0, "no verdict"},
		{"nan", `{"reward": "NaN"}`, 0, "no verdict"},
		{"no reward field", `{"metadata": {}}`, 0, "no verdict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseVerdict([]byte(tc.out))
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil || v.Reward != tc.reward {
				t.Fatalf("got %+v %v", v, err)
			}
		})
	}
}

func TestStateMachineRejectsIllegalTransitions(t *testing.T) {
	for from, tos := range allowed {
		for _, to := range []State{Provision, Setup, StudentTurn, Custody, Grade, Done, Failed} {
			legal := false
			for _, t2 := range tos {
				legal = legal || t2 == to
			}
			if got := canTransition(from, to); got != legal {
				t.Fatalf("%s -> %s: got %v want %v", from, to, got, legal)
			}
		}
	}
	if canTransition(Done, Failed) || canTransition(Failed, Done) {
		t.Fatal("terminal states are final")
	}
}

func TestObservationJSON(t *testing.T) {
	b, err := json.Marshal(ToolCall{Tool: ToolBash, Command: "ls"})
	if err != nil || !strings.Contains(string(b), `"tool":"bash"`) {
		t.Fatalf("%s %v", b, err)
	}
}

type scripted struct {
	calls []ToolCall
	i     int
}

func (s *scripted) Act(ctx context.Context, obs Observation) (ToolCall, error) {
	if s.i >= len(s.calls) {
		return ToolCall{Tool: ToolSubmit}, nil
	}
	c := s.calls[s.i]
	s.i++
	return c, nil
}
