// Package testenv provides a tiny environment backed by the fake runtime
// for unit tests of packages above episode (agent API, scheduler, store).
package testenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xartus2004/arena/env"
	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/fake"
)

// Secret is the hidden answer of the toy task.
const Secret = "THE-ANSWER-IS-42"

// AnswerPath is where the toy task expects the answer.
const AnswerPath = "/workspace/out/answer.txt"

const envYAML = `
name: toy
sandbox:
  image: fake
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
    max_steps: 20
`

// New writes the toy environment to a temp dir and returns it with a fake
// runtime whose handler implements the grader.
func New(t testing.TB) (*env.Environment, *fake.Runtime) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"env.yaml": envYAML, "files/README": "hello", "hidden/answer.txt": Secret,
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
	r := fake.New()
	r.Handler = Grader
	return e, r
}

// Grader implements the toy grader inside the fake runtime.
func Grader(ctx context.Context, sb *fake.Sandbox, req rt.ExecRequest) (rt.Result, bool, error) {
	if len(req.Cmd) >= 2 && req.Cmd[0] == "python3" && req.Cmd[1] == "/grader/grade.py" {
		want, _ := sb.ReadFile("/grader/answer.txt")
		got, ok := sb.ReadFile("/submission/out/answer.txt")
		reward := 0.0
		if ok && strings.TrimSpace(string(got)) == strings.TrimSpace(string(want)) {
			reward = 1
		}
		return rt.Result{Stdout: []byte(fmt.Sprintf("{\"reward\": %g}\n", reward))}, true, nil
	}
	return rt.Result{}, false, nil
}
