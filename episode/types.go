package episode

import (
	"context"
	"time"

	"github.com/xartus2004/arena/custody"
)

// State is a lifecycle phase.
type State string

// Lifecycle states.
const (
	Provision   State = "PROVISION"
	Setup       State = "SETUP"
	StudentTurn State = "STUDENT_TURN"
	Custody     State = "CUSTODY"
	Grade       State = "GRADE"
	Done        State = "DONE"
	Failed      State = "FAILED"
)

// allowed is the transition table. Any other transition is a bug.
var allowed = map[State][]State{
	"":          {Provision},
	Provision:   {Setup, Failed},
	Setup:       {StudentTurn, Failed},
	StudentTurn: {Custody, Failed},
	Custody:     {Grade, Done, Failed}, // Done directly when custody rejects the submission
	Grade:       {Done, Failed},
	Done:        {},
	Failed:      {},
}

func canTransition(from, to State) bool {
	for _, s := range allowed[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition is one recorded state change; At is host time.
type Transition struct {
	From   State     `json:"from"`
	To     State     `json:"to"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// Tool names.
const (
	ToolBash      = "bash"
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
	ToolSubmit    = "submit"
)

// ToolCall is an action chosen by the agent.
type ToolCall struct {
	Tool    string `json:"tool"`
	Command string `json:"command,omitempty"` // bash
	Path    string `json:"path,omitempty"`    // read_file / write_file
	Content string `json:"content,omitempty"` // write_file
	// TimeoutSec optionally lowers the per-call timeout for bash.
	TimeoutSec float64 `json:"timeout_sec,omitempty"`
}

// ToolResult is what the agent sees after a tool call.
type ToolResult struct {
	Output    string        `json:"output"`
	Stderr    string        `json:"stderr,omitempty"`
	ExitCode  int           `json:"exit_code"`
	Error     string        `json:"error,omitempty"` // invalid call (not an infra failure)
	TimedOut  bool          `json:"timed_out,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Duration  time.Duration `json:"duration_ns"`
}

// Observation is returned to the agent after Start and every Step.
type Observation struct {
	EpisodeID    string      `json:"episode_id"`
	Attempt      int         `json:"attempt"`
	State        State       `json:"state"`
	Step         int         `json:"step"`
	Instructions string      `json:"instructions,omitempty"`
	Result       *ToolResult `json:"result,omitempty"`
	Deadline     time.Time   `json:"deadline"`
	Done         bool        `json:"done"`
	Outcome      *Outcome    `json:"outcome,omitempty"`
}

// Status is the terminal status of an episode.
type Status string

// Terminal statuses.
const (
	StatusDone   Status = "done"
	StatusFailed Status = "failed"
)

// Failure classifies a FAILED episode.
type Failure string

// Failure kinds. Agent mistakes are not failures: a wrong answer is a
// DONE episode with reward 0.
const (
	FailureNone   Failure = ""
	FailureInfra  Failure = "infra"  // sandbox/runtime broke: retryable
	FailurePolicy Failure = "policy" // the policy itself errored (e.g. LLM API down)
)

// Outcome is the final result of an episode attempt.
type Outcome struct {
	EpisodeID    string                  `json:"episode_id"`
	Attempt      int                     `json:"attempt"`
	Env          string                  `json:"env"`
	Task         string                  `json:"task"`
	Status       Status                  `json:"status"`
	Failure      Failure                 `json:"failure,omitempty"`
	Error        string                  `json:"error,omitempty"`
	Reward       float64                 `json:"reward"`
	Metadata     map[string]any          `json:"metadata,omitempty"`
	Violation    *custody.Violation      `json:"violation,omitempty"`
	Manifest     *custody.Manifest       `json:"manifest,omitempty"`
	ArtifactPath string                  `json:"artifact_path,omitempty"`
	Steps        int                     `json:"steps"`
	Phases       map[State]time.Duration `json:"phases_ns"`
	Started      time.Time               `json:"started"`
	Finished     time.Time               `json:"finished"`
	Transitions  []Transition            `json:"transitions"`
}

// Duration is the host-measured wall time of the attempt.
func (o *Outcome) Duration() time.Duration { return o.Finished.Sub(o.Started) }

// Event is a trajectory record.
type Event struct {
	Time      time.Time      `json:"time"`
	EpisodeID string         `json:"episode_id"`
	Attempt   int            `json:"attempt"`
	Type      string         `json:"type"` // transition | tool_call | custody | grade | outcome | log
	Step      int            `json:"step,omitempty"`
	From      State          `json:"from,omitempty"`
	To        State          `json:"to,omitempty"`
	Tool      *ToolCall      `json:"tool,omitempty"`
	Result    *ToolResult    `json:"result,omitempty"`
	Outcome   *Outcome       `json:"outcome,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// Recorder receives trajectory events. Implementations must be safe for
// concurrent use and must not block for long.
type Recorder interface {
	Record(Event)
}

// Policy chooses tool calls. Implementations are owned by one episode.
type Policy interface {
	Act(ctx context.Context, obs Observation) (ToolCall, error)
}

type nopRecorder struct{}

func (nopRecorder) Record(Event) {}
