// Package episode implements the lifecycle of one RL episode:
//
//	PROVISION → SETUP → STUDENT_TURN → CUSTODY → GRADE → DONE
//	                 (any phase can fail) ↘ FAILED
//
// Invariants enforced here:
//   - hidden task files are only ever copied into the grader sandbox;
//   - the agent's tools run as the unprivileged Student user;
//   - custody kills every student process and verifies none remain before
//     the submission is copied, then destroys the student sandbox;
//   - grading runs on the custody copy in a fresh sandbox, so the student
//     never shares a process, filesystem or clock with the grader;
//   - all time is measured on the host.
package episode

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xartus2004/arena/custody"
	"github.com/xartus2004/arena/env"
	rt "github.com/xartus2004/arena/runtime"
)

// Errors returned to callers driving an episode.
var (
	ErrNotStudentTurn = errors.New("episode: not the student's turn")
	ErrAlreadyStarted = errors.New("episode: already started")
)

// Config holds the dependencies of an episode.
type Config struct {
	// Runtime creates student sandboxes.
	Runtime rt.Runtime
	// GraderRuntime creates grader sandboxes (defaults to Runtime).
	GraderRuntime rt.Runtime
	// Recorder receives trajectory events (optional).
	Recorder Recorder
	// ArtifactDir is the host directory for custody copies; it is created
	// with mode 0700.
	ArtifactDir string
	// Logger (optional).
	Logger *slog.Logger
	// Clock returns host time (optional; for tests).
	Clock func() time.Time
	// ToolTimeout caps each bash/read/write call (default 60s).
	ToolTimeout time.Duration
	// MaxToolOutput caps the output returned to the agent per stream
	// (default 64 KiB). Longer output is cut with a visible marker.
	MaxToolOutput int
	// SetupTimeout caps each setup step (default 2m).
	SetupTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.GraderRuntime == nil {
		c.GraderRuntime = c.Runtime
	}
	if c.Recorder == nil {
		c.Recorder = nopRecorder{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.ToolTimeout <= 0 {
		c.ToolTimeout = 60 * time.Second
	}
	if c.MaxToolOutput <= 0 {
		c.MaxToolOutput = 64 << 10
	}
	if c.SetupTimeout <= 0 {
		c.SetupTimeout = 2 * time.Minute
	}
	if c.ArtifactDir == "" {
		c.ArtifactDir = filepath.Join(os.TempDir(), "arena-artifacts")
	}
	return c
}

// Episode is one attempt at one task. It is safe for concurrent use; calls
// are serialised.
type Episode struct {
	ID      string
	Attempt int

	cfg  Config
	task *env.Task
	log  *slog.Logger

	mu          sync.Mutex
	state       State
	transitions []Transition
	student     rt.Sandbox
	grader      rt.Sandbox
	started     time.Time
	phaseStart  time.Time
	phases      map[State]time.Duration
	deadline    time.Time
	steps       int
	outcome     *Outcome
}

// New creates an episode in its initial state. Call Start to provision.
func New(cfg Config, task *env.Task, id string, attempt int) *Episode {
	cfg = cfg.withDefaults()
	return &Episode{
		ID:      id,
		Attempt: attempt,
		cfg:     cfg,
		task:    task,
		log:     cfg.Logger.With("episode", id, "attempt", attempt, "env", task.Env().Name, "task", task.ID),
		phases:  map[State]time.Duration{},
	}
}

// State returns the current state.
func (e *Episode) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// Transitions returns a copy of the transition history.
func (e *Episode) Transitions() []Transition {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Transition(nil), e.transitions...)
}

// Outcome returns the final outcome, or nil while the episode is running.
func (e *Episode) Outcome() *Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.outcome
}

// Task returns the task.
func (e *Episode) Task() *env.Task { return e.task }

func (e *Episode) record(ev Event) {
	ev.Time = e.cfg.Clock()
	ev.EpisodeID = e.ID
	ev.Attempt = e.Attempt
	e.cfg.Recorder.Record(ev)
}

// transition must be called with e.mu held.
func (e *Episode) transition(to State, reason string) {
	from := e.state
	if !canTransition(from, to) {
		panic(fmt.Sprintf("episode %s: illegal transition %s -> %s", e.ID, from, to))
	}
	now := e.cfg.Clock()
	if from != "" {
		e.phases[from] += now.Sub(e.phaseStart)
	} else {
		e.started = now
	}
	e.phaseStart = now
	e.state = to
	e.transitions = append(e.transitions, Transition{From: from, To: to, At: now, Reason: reason})
	e.log.Debug("transition", "from", from, "to", to, "reason", reason)
	e.record(Event{Type: "transition", From: from, To: to, Data: map[string]any{"reason": reason}})
}

// Start provisions the student sandbox, runs setup and opens the student
// turn. On error the episode is FAILED and its sandboxes destroyed.
func (e *Episode) Start(ctx context.Context) (Observation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != "" {
		return Observation{}, ErrAlreadyStarted
	}
	e.transition(Provision, "")
	spec := e.task.Env().StudentSpec()
	spec.Labels = mergeLabels(spec.Labels, map[string]string{"arena.episode": e.ID, "arena.role": "student"})
	sb, err := e.cfg.Runtime.Create(ctx, spec)
	if err != nil {
		return Observation{}, e.failLocked(ctx, rt.Infra("provision student sandbox", err))
	}
	e.student = sb

	e.transition(Setup, "")
	if err := e.setupLocked(ctx); err != nil {
		return Observation{}, e.failLocked(ctx, err)
	}
	e.transition(StudentTurn, "")
	e.deadline = e.cfg.Clock().Add(time.Duration(e.task.TimeLimit))
	return e.observationLocked(nil), nil
}

func (e *Episode) setupLocked(ctx context.Context) error {
	files, err := e.task.FilesTar()
	if err != nil {
		return rt.Infra("read task files", err)
	}
	if err := e.student.CopyIn(ctx, e.task.FilesDest, files, rt.Root); err != nil {
		return rt.Infra("copy task files", err)
	}
	return runSteps(ctx, e.log, e.student, e.task.Setup, e.cfg.SetupTimeout, "setup")
}

func runSteps(ctx context.Context, log *slog.Logger, sb rt.Sandbox, steps []env.Step, defTimeout time.Duration, what string) error {
	for i, st := range steps {
		u, err := env.UserByName(st.User)
		if err != nil {
			return fmt.Errorf("%s step %d: %w", what, i, err)
		}
		timeout := time.Duration(st.Timeout)
		if timeout <= 0 {
			timeout = defTimeout
		}
		res, err := sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"sh", "-c", st.Run}, User: u, Timeout: timeout})
		log.Debug(what+" step", "step", i, "user", u.Name, "duration", res.Duration, "exit", res.ExitCode)
		if err != nil {
			return rt.Infra(fmt.Sprintf("%s step %d", what, i), err)
		}
		if res.ExitCode != 0 || res.TimedOut {
			// A broken setup is an environment/infra problem, never the agent's.
			return rt.Infra(fmt.Sprintf("%s step %d", what, i),
				fmt.Errorf("exit %d (timed out: %v): %s", res.ExitCode, res.TimedOut, truncate(string(res.Stderr), 2048)))
		}
	}
	return nil
}

func (e *Episode) observationLocked(res *ToolResult) Observation {
	obs := Observation{
		EpisodeID: e.ID, Attempt: e.Attempt, State: e.state, Step: e.steps,
		Result: res, Deadline: e.deadline,
	}
	if e.steps == 0 && res == nil {
		obs.Instructions = e.task.Instructions
	}
	if e.outcome != nil {
		obs.Done = true
		obs.Outcome = e.outcome
	}
	return obs
}

// Step executes one tool call. Invalid calls produce a ToolResult with Error
// set (not a Go error). Go errors are infrastructure failures (the episode
// is then FAILED) or misuse (ErrNotStudentTurn).
func (e *Episode) Step(ctx context.Context, call ToolCall) (Observation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != StudentTurn {
		return e.observationLocked(nil), ErrNotStudentTurn
	}
	now := e.cfg.Clock()
	switch {
	case !now.Before(e.deadline):
		return e.finishLocked(ctx, "time_limit")
	case e.steps >= e.task.MaxSteps:
		return e.finishLocked(ctx, "max_steps")
	}
	e.steps++
	e.record(Event{Type: "tool_call", Step: e.steps, Tool: &call})
	if call.Tool == ToolSubmit {
		return e.finishLocked(ctx, "")
	}
	res, err := e.runToolLocked(ctx, call, e.deadline.Sub(now))
	if err != nil {
		return Observation{}, e.failLocked(ctx, err)
	}
	e.record(Event{Type: "tool_result", Step: e.steps, Result: &res})
	return e.observationLocked(&res), nil
}

func (e *Episode) runToolLocked(ctx context.Context, call ToolCall, remaining time.Duration) (ToolResult, error) {
	timeout := min(e.cfg.ToolTimeout, remaining)
	if call.TimeoutSec > 0 {
		timeout = min(timeout, time.Duration(call.TimeoutSec*float64(time.Second)))
	}
	req := rt.ExecRequest{
		User:      rt.Student,
		Dir:       e.task.FilesDest,
		Env:       []string{"HOME=/tmp", "USER=student", "PATH=/usr/local/bin:/usr/bin:/bin"},
		Timeout:   timeout,
		MaxOutput: e.cfg.MaxToolOutput * 4,
	}
	switch call.Tool {
	case ToolBash:
		if strings.TrimSpace(call.Command) == "" {
			return ToolResult{Error: "bash: command is required"}, nil
		}
		req.Cmd = []string{"sh", "-c", call.Command}
	case ToolReadFile:
		if call.Path == "" {
			return ToolResult{Error: "read_file: path is required"}, nil
		}
		if !path.IsAbs(call.Path) {
			return ToolResult{Error: "read_file: path must be absolute"}, nil
		}
		req.Cmd = []string{"cat", "--", call.Path}
	case ToolWriteFile:
		if call.Path == "" {
			return ToolResult{Error: "write_file: path is required"}, nil
		}
		if !path.IsAbs(call.Path) {
			return ToolResult{Error: "write_file: path must be absolute"}, nil
		}
		req.Cmd = []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", call.Path}
		req.Stdin = strings.NewReader(call.Content)
	default:
		return ToolResult{Error: fmt.Sprintf("unknown tool %q (want bash, read_file, write_file, submit)", call.Tool)}, nil
	}
	r, err := e.student.Exec(ctx, req)
	if err != nil {
		return ToolResult{}, rt.Infra("exec "+call.Tool, err)
	}
	out, t1 := truncateMarked(string(r.Stdout), e.cfg.MaxToolOutput)
	serr, t2 := truncateMarked(string(r.Stderr), e.cfg.MaxToolOutput)
	return ToolResult{
		Output: out, Stderr: serr, ExitCode: r.ExitCode, TimedOut: r.TimedOut,
		Truncated: r.Truncated || t1 || t2, Duration: r.Duration,
	}, nil
}

// Expire forces submission if the deadline has passed (used by watchdogs
// so an idle agent cannot hold a sandbox forever). It is a no-op otherwise.
func (e *Episode) Expire(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != StudentTurn || e.cfg.Clock().Before(e.deadline) {
		return false, nil
	}
	_, err := e.finishLocked(ctx, "time_limit")
	return true, err
}

// Deadline returns the student-turn deadline (zero before Start).
func (e *Episode) Deadline() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deadline
}

// finishLocked runs CUSTODY and GRADE. forced is "" for a voluntary submit.
func (e *Episode) finishLocked(ctx context.Context, forced string) (Observation, error) {
	meta := map[string]any{}
	if forced != "" {
		meta["forced_submit"] = forced
	}
	e.transition(Custody, forced)
	man, artifact, viol, err := e.custodyLocked(ctx)
	if err != nil {
		return Observation{}, e.failLocked(ctx, err)
	}
	if viol != nil {
		e.record(Event{Type: "custody", Data: map[string]any{"violation": viol}})
		e.completeLocked(0, meta, viol, nil, "")
		return e.observationLocked(nil), nil
	}
	e.record(Event{Type: "custody", Data: map[string]any{"manifest_sha256": man.SHA256, "files": len(man.Files), "bytes": man.TotalBytes, "artifact": artifact}})

	e.transition(Grade, "")
	v, err := e.gradeLocked(ctx, artifact)
	if err != nil {
		return Observation{}, e.failLocked(ctx, err)
	}
	for k, val := range v.Metadata {
		meta[k] = val
	}
	e.record(Event{Type: "grade", Data: map[string]any{"reward": v.Reward, "metadata": v.Metadata}})
	e.completeLocked(v.Reward, meta, nil, &man, artifact)
	return e.observationLocked(nil), nil
}

func (e *Episode) artifactDir() string {
	return filepath.Join(e.cfg.ArtifactDir, safeName(e.ID), fmt.Sprintf("attempt-%d", e.Attempt))
}

// custodyLocked returns a violation (agent fault) or an error (infra).
func (e *Episode) custodyLocked(ctx context.Context) (custody.Manifest, string, *custody.Violation, error) {
	var none custody.Manifest
	// 1. Kill every student (and listed service) process, verifying none remain.
	uids := []int{rt.Student.UID}
	for _, name := range e.task.KillUsers {
		u, _ := env.UserByName(name)
		uids = append(uids, u.UID)
	}
	for _, uid := range uids {
		if err := e.student.KillUser(ctx, uid); err != nil {
			if rt.IsInfra(err) {
				return none, "", nil, err
			}
			// Processes that cannot be killed mean we cannot take custody
			// safely: fail closed.
			return none, "", e.closeStudentWith(ctx, &custody.Violation{Kind: custody.KindProcesses, Detail: err.Error()}), nil
		}
	}
	e.record(Event{Type: "custody", Data: map[string]any{"killed_uids": uids, "verified_none_remain": true}})

	// 2. Root-side prescan for hardlinks whose twin is outside the
	// submission (tar encodes those as ordinary files). Student processes
	// are dead and the rootfs is read-only, so this find is trustworthy.
	if v := e.prescanLocked(ctx); v != nil {
		return none, "", e.closeStudentWith(ctx, v), nil
	}

	// 3. Copy out through the runtime (tar stream, never following links)
	// into a root-only host directory, enforcing all limits.
	dir := e.artifactDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return none, "", nil, rt.Infra("custody mkdir", err)
	}
	dest := filepath.Join(dir, "submission")
	_ = os.RemoveAll(dest) // a previous crashed run of this attempt
	lim := e.task.EffectiveLimits()
	climits := custody.Limits{MaxFiles: lim.MaxFiles, MaxBytes: int64(lim.MaxBytes), MaxFileBytes: int64(lim.MaxFileBytes), MaxDepth: lim.MaxDepth}
	rc, err := e.student.CopyOut(ctx, e.task.Submission)
	if err != nil {
		if isNotExist(err) {
			return none, "", e.closeStudentWith(ctx, &custody.Violation{Kind: custody.KindMissing, Path: e.task.Submission, Detail: "submission not found"}), nil
		}
		return none, "", nil, rt.Infra("custody copy-out", err)
	}
	man, err := custody.Extract(ctx, rc, dest, climits)
	rc.Close()
	var viol *custody.Violation
	if errors.As(err, &viol) {
		return none, "", e.closeStudentWith(ctx, viol), nil
	}
	if err != nil {
		return none, "", nil, rt.Infra("custody extract", err)
	}

	// 4. Save the copy as an artifact.
	artifact := filepath.Join(dir, "submission.tar.gz")
	if err := writeArtifact(dest, artifact); err != nil {
		return none, "", nil, rt.Infra("custody artifact", err)
	}
	// 5. Destroy the student sandbox: nothing the student did can matter now.
	if err := e.destroyStudentLocked(ctx); err != nil {
		return none, "", nil, err
	}
	return man, dest, nil, nil
}

func (e *Episode) prescanLocked(ctx context.Context) *custody.Violation {
	res, err := e.student.Exec(ctx, rt.ExecRequest{
		Cmd:     []string{"find", e.task.Submission, "-xdev", "-type", "f", "-links", "+1"},
		User:    rt.Root,
		Timeout: 30 * time.Second, MaxOutput: 4096,
	})
	if err != nil || res.ExitCode == 127 {
		// The runtime cannot run find (e.g. the fake); tar-level checks
		// still apply.
		return nil
	}
	if res.TimedOut {
		return &custody.Violation{Kind: custody.KindTooManyFiles, Path: e.task.Submission, Detail: "prescan timed out"}
	}
	if line := strings.TrimSpace(strings.SplitN(string(res.Stdout), "\n", 2)[0]); line != "" {
		return &custody.Violation{Kind: custody.KindHardlink, Path: line, Detail: "regular file with link count > 1"}
	}
	return nil
}

func (e *Episode) closeStudentWith(ctx context.Context, v *custody.Violation) *custody.Violation {
	if err := e.destroyStudentLocked(ctx); err != nil {
		e.log.Warn("destroy student sandbox", "err", err)
	}
	return v
}

func (e *Episode) destroyStudentLocked(ctx context.Context) error {
	if e.student == nil {
		return nil
	}
	err := e.student.Destroy(context.WithoutCancel(ctx))
	e.student = nil
	if err != nil {
		return rt.Infra("destroy student sandbox", err)
	}
	e.record(Event{Type: "custody", Data: map[string]any{"student_sandbox_destroyed": true}})
	return nil
}

// Verdict is the parsed grader output.
type Verdict struct {
	Reward   float64        `json:"reward"`
	Metadata map[string]any `json:"metadata"`
}

func (e *Episode) gradeLocked(ctx context.Context, submissionDir string) (Verdict, error) {
	spec := e.task.Env().GraderSpec()
	spec.Labels = mergeLabels(spec.Labels, map[string]string{"arena.episode": e.ID, "arena.role": "grader"})
	gsb, err := e.cfg.GraderRuntime.Create(ctx, spec)
	if err != nil {
		return Verdict{}, rt.Infra("provision grader sandbox", err)
	}
	e.grader = gsb
	defer func() {
		if e.grader != nil {
			if err := e.grader.Destroy(context.WithoutCancel(ctx)); err != nil {
				e.log.Warn("destroy grader sandbox", "err", err)
			}
			e.grader = nil
		}
	}()
	hidden, err := e.task.HiddenTar()
	if err != nil {
		return Verdict{}, rt.Infra("read hidden files", err)
	}
	if err := gsb.CopyIn(ctx, "/grader", hidden, rt.Root); err != nil {
		return Verdict{}, rt.Infra("copy hidden files", err)
	}
	var packed bytes.Buffer
	if err := custody.Pack(submissionDir, &packed); err != nil {
		return Verdict{}, rt.Infra("pack submission", err)
	}
	if err := gsb.CopyIn(ctx, "/submission", &packed, rt.Root); err != nil {
		return Verdict{}, rt.Infra("copy submission", err)
	}
	// Hidden files are root-only even inside the grader sandbox, so
	// submitted code the grader runs as Untrusted cannot read them.
	for _, c := range [][]string{{"chown", "-R", "0:0", "/grader"}, {"chmod", "-R", "go-rwx", "/grader"}, {"chmod", "-R", "a+rX", "/submission"}} {
		if r, err := gsb.Exec(ctx, rt.ExecRequest{Cmd: c, User: rt.Root, Timeout: 30 * time.Second}); err != nil || r.ExitCode != 0 {
			return Verdict{}, rt.Infra("lock grader files", fmt.Errorf("%v: exit %d %v %s", c, r.ExitCode, err, r.Stderr))
		}
	}
	if err := runSteps(ctx, e.log, gsb, e.task.Grader.Setup, e.cfg.SetupTimeout, "grader setup"); err != nil {
		return Verdict{}, err
	}
	res, err := gsb.Exec(ctx, rt.ExecRequest{
		Cmd: e.task.Grader.Command, User: rt.Root, Dir: "/grader",
		Timeout: time.Duration(e.task.Grader.Timeout), MaxOutput: 1 << 20,
	})
	if err != nil {
		return Verdict{}, rt.Infra("run grader", err)
	}
	if res.TimedOut {
		return Verdict{Reward: 0, Metadata: map[string]any{"grader_error": "timeout"}}, nil
	}
	v, perr := parseVerdict(res.Stdout)
	if perr != nil {
		return Verdict{Reward: 0, Metadata: map[string]any{
			"grader_error": perr.Error(), "grader_exit": res.ExitCode,
			"grader_stderr": truncate(string(res.Stderr), 2048),
		}}, nil
	}
	return v, nil
}

// parseVerdict reads the last JSON object line of grader stdout.
func parseVerdict(out []byte) (Verdict, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var raw struct {
			Reward   *float64       `json:"reward"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(l), &raw); err != nil || raw.Reward == nil || math.IsNaN(*raw.Reward) {
			continue
		}
		r := math.Max(0, math.Min(1, *raw.Reward))
		return Verdict{Reward: r, Metadata: raw.Metadata}, nil
	}
	return Verdict{}, errors.New("grader printed no verdict line ({\"reward\": x})")
}

func (e *Episode) completeLocked(reward float64, meta map[string]any, viol *custody.Violation, man *custody.Manifest, artifactDir string) {
	e.transition(Done, "")
	o := e.baseOutcomeLocked(StatusDone)
	o.Reward = reward
	o.Metadata = meta
	o.Violation = viol
	o.Manifest = man
	if artifactDir != "" {
		o.ArtifactPath = filepath.Join(filepath.Dir(artifactDir), "submission.tar.gz")
	}
	e.outcome = o
	e.record(Event{Type: "outcome", Outcome: o})
	e.log.Info("episode done", "reward", reward, "violation", viol != nil, "steps", e.steps)
}

func (e *Episode) baseOutcomeLocked(st Status) *Outcome {
	phases := make(map[State]time.Duration, len(e.phases))
	for k, v := range e.phases {
		phases[k] = v
	}
	return &Outcome{
		EpisodeID: e.ID, Attempt: e.Attempt, Env: e.task.Env().Name, Task: e.task.ID,
		Status: st, Steps: e.steps, Phases: phases, Started: e.started,
		Finished: e.cfg.Clock(), Transitions: append([]Transition(nil), e.transitions...),
	}
}

// failLocked moves to FAILED, destroys sandboxes and returns err.
func (e *Episode) failLocked(ctx context.Context, err error) error {
	if e.state == Done || e.state == Failed {
		return err
	}
	e.destroyAllLocked(ctx)
	e.transition(Failed, err.Error())
	o := e.baseOutcomeLocked(StatusFailed)
	o.Error = err.Error()
	o.Failure = FailurePolicy
	if rt.IsInfra(err) {
		o.Failure = FailureInfra
	}
	e.outcome = o
	e.record(Event{Type: "outcome", Outcome: o})
	e.log.Warn("episode failed", "err", err, "failure", o.Failure)
	return err
}

// Abort fails the episode (e.g. the policy errored or the server is
// shutting down). It is a no-op once the episode is terminal.
func (e *Episode) Abort(ctx context.Context, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.failLocked(ctx, err)
}

// Close destroys any remaining sandboxes without changing state. Safe to
// call multiple times.
func (e *Episode) Close(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.destroyAllLocked(ctx)
}

func (e *Episode) destroyAllLocked(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for _, sb := range []*rt.Sandbox{&e.student, &e.grader} {
		if *sb != nil {
			if err := (*sb).Destroy(ctx); err != nil {
				e.log.Warn("destroy sandbox", "err", err)
			}
			*sb = nil
		}
	}
}

func writeArtifact(dir, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if err := custody.Pack(dir, zw); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func isNotExist(err error) bool {
	s := strings.ToLower(err.Error())
	return errors.Is(err, os.ErrNotExist) || strings.Contains(s, "no such file") || strings.Contains(s, "not found")
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

func truncate(s string, n int) string {
	out, _ := truncateMarked(s, n)
	return out
}

// truncateMarked cuts s to n bytes and appends a visible marker.
func truncateMarked(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return s[:n] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-n), true
}
