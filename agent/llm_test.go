package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xartus2004/arena/episode"
)

// fakeClaude replays canned Messages API responses and records requests.
type fakeClaude struct {
	mu        sync.Mutex
	responses []string
	requests  []map[string]any
}

func (f *fakeClaude) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	resp := f.responses[0]
	f.responses = f.responses[1:]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, resp)
}

func msg(stop string, content ...string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"m","stop_reason":"` + stop +
		`","usage":{"input_tokens":1,"output_tokens":1},"content":[` + strings.Join(content, ",") + `]}`
}

func toolUse(id, name, input string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}`
}

func TestLLMPolicyToolLoop(t *testing.T) {
	f := &fakeClaude{responses: []string{
		msg("tool_use", `{"type":"text","text":"looking"}`, toolUse("t1", "bash", `{"command":"ls"}`), toolUse("t2", "read_file", `{"path":"/a"}`)),
		msg("tool_use", toolUse("t3", "submit", `{}`)),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ARENA_LLM_BASE_URL", srv.URL)
	p, err := NewLLMFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c1, err := p.Act(ctx, episode.Observation{Instructions: "do it"})
	if err != nil || c1.Tool != "bash" || c1.Command != "ls" {
		t.Fatalf("first call %+v %v", c1, err)
	}
	c2, err := p.Act(ctx, episode.Observation{Result: &episode.ToolResult{Output: "a b"}})
	if err != nil || c2.Tool != "read_file" || c2.Path != "/a" {
		t.Fatalf("second call %+v %v", c2, err)
	}
	if len(f.requests) != 1 {
		t.Fatal("queued tool calls must not trigger a new API request")
	}
	c3, err := p.Act(ctx, episode.Observation{Result: &episode.ToolResult{Output: "x", ExitCode: 1}})
	if err != nil || c3.Tool != "submit" {
		t.Fatalf("third call %+v %v", c3, err)
	}
	// Both tool results were returned together in one user message.
	msgs := f.requests[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 || blocks[0].(map[string]any)["tool_use_id"] != "t1" {
		t.Fatalf("tool results not batched: %+v", last)
	}
	if f.requests[0]["model"] != DefaultLLMModel {
		t.Fatalf("model = %v", f.requests[0]["model"])
	}
}

func TestLLMNeedsKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := NewLLMFromEnv(); err == nil {
		t.Fatal("expected error without API key")
	}
}
