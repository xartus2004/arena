package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"

	"github.com/xartus2004/arena/episode"
)

// DefaultLLMModel is used unless ARENA_LLM_MODEL overrides it.
const DefaultLLMModel = "claude-opus-5-5"

// LLM is an optional policy backed by the Claude Messages API. It is only
// constructed when ANTHROPIC_API_KEY is set; nothing in arena requires it.
//
// Each Act call returns exactly one tool call. When the model asks for
// several tools in one turn they are queued, and all their results are sent
// back together in a single user message, as the API expects.
type LLM struct {
	client   anthropic.Client
	model    string
	maxTurns int

	messages []anthropic.BetaMessageParam
	pending  []anthropic.BetaToolUseBlock           // tool calls not yet handed out
	results  []anthropic.BetaContentBlockParamUnion // results for the current turn
	waiting  string                                 // tool_use id awaiting a result
	turns    int
}

// NewLLMFromEnv builds the LLM policy from ANTHROPIC_API_KEY (required) and
// ARENA_LLM_MODEL (optional).
func NewLLMFromEnv() (*LLM, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("agent: llm agent needs ANTHROPIC_API_KEY")
	}
	model := os.Getenv("ARENA_LLM_MODEL")
	if model == "" {
		model = DefaultLLMModel
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if base := os.Getenv("ARENA_LLM_BASE_URL"); base != "" { // tests / proxies
		opts = append(opts, option.WithBaseURL(base))
	}
	return &LLM{client: anthropic.NewClient(opts...), model: model, maxTurns: 40}, nil
}

const llmSystem = `You are solving a task inside a sandboxed Linux machine (Alpine, busybox, python3; no network).
Use the tools to inspect and change files, then call submit exactly once when you are done.
Your work is graded by hidden tests after you submit.`

func llmTools() []anthropic.BetaToolUnionParam {
	str := map[string]any{"type": "string"}
	mk := func(name, desc string, props map[string]any, required ...string) anthropic.BetaToolUnionParam {
		return anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        name,
			Description: anthropic.String(desc),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: props, Required: required},
		}}
	}
	return []anthropic.BetaToolUnionParam{
		mk(episode.ToolBash, "Run a shell command (sh -c) as the unprivileged user.", map[string]any{"command": str}, "command"),
		mk(episode.ToolReadFile, "Read a file by absolute path.", map[string]any{"path": str}, "path"),
		mk(episode.ToolWriteFile, "Write content to a file by absolute path (creates parent directories).",
			map[string]any{"path": str, "content": str}, "path", "content"),
		mk(episode.ToolSubmit, "Submit your work for grading. Ends the episode.", map[string]any{}),
	}
}

func formatResult(r *episode.ToolResult) (string, bool) {
	if r == nil {
		return "(no result)", true
	}
	if r.Error != "" {
		return r.Error, true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d", r.ExitCode)
	if r.TimedOut {
		b.WriteString(" (timed out)")
	}
	b.WriteString("\n" + r.Output)
	if r.Stderr != "" {
		b.WriteString("\n[stderr]\n" + r.Stderr)
	}
	return b.String(), false
}

// Act implements episode.Policy.
func (l *LLM) Act(ctx context.Context, obs episode.Observation) (episode.ToolCall, error) {
	if l.messages == nil {
		l.messages = []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(obs.Instructions))}
	}
	if l.waiting != "" {
		text, isErr := formatResult(obs.Result)
		l.results = append(l.results, anthropic.NewBetaToolResultBlock(l.waiting, text, isErr))
		l.waiting = ""
	}
	if len(l.pending) == 0 {
		if len(l.results) > 0 {
			l.messages = append(l.messages, anthropic.NewBetaUserMessage(l.results...))
			l.results = nil
		}
		if err := l.next(ctx); err != nil {
			return episode.ToolCall{}, err
		}
		if len(l.pending) == 0 { // the model stopped without a tool call
			return Submit(), nil
		}
	}
	use := l.pending[0]
	l.pending = l.pending[1:]
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	// A malformed input leaves the fields empty; the episode then returns a
	// validation error to the model as the tool result.
	_ = json.Unmarshal([]byte(use.JSON.Input.Raw()), &in)
	l.waiting = use.ID
	return episode.ToolCall{Tool: use.Name, Command: in.Command, Path: in.Path, Content: in.Content}, nil
}

// next asks the model for its next turn.
func (l *LLM) next(ctx context.Context) error {
	l.turns++
	if l.turns > l.maxTurns {
		return nil
	}
	resp, err := l.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     l.model,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: llmSystem}},
		Messages:  l.messages,
		Tools:     llmTools(),
		// Re-serve safety-classifier refusals on a fallback model in the
		// same call instead of ending the episode.
		// "default" routes by refusal category, so no model list to maintain.
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
	})
	if err != nil {
		return fmt.Errorf("agent: claude api: %w", err)
	}
	l.messages = append(l.messages, resp.ToParam())
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return nil // nothing to run; the episode will submit
	}
	for _, block := range resp.Content {
		if use, ok := block.AsAny().(anthropic.BetaToolUseBlock); ok {
			l.pending = append(l.pending, use)
		}
	}
	return nil
}
