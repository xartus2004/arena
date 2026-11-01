package env

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rt "github.com/xartus2004/arena/runtime"
)

const validYAML = `
name: demo
description: demo env
sandbox:
  image: python:3.12-alpine
  cpus: 1
  memory: 256Mi
  pids: 128
  disk: 64Mi
  inodes: 5000
limits:
  max_files: 50
  max_bytes: 1Mi
tasks:
  - id: t1
    instructions: fix it
    files: tasks/t1/files
    hidden: tasks/t1/hidden
    submission: /workspace/repo
    setup:
      - run: chown -R 1000:1000 /workspace
    grader:
      command: ["python3", "/grader/grade.py"]
      timeout: 30s
    time_limit: 2m
`

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false},
		{"100", 100, false},
		{"1k", 1000, false},
		{"1Ki", 1024, false},
		{"64Mi", 64 << 20, false},
		{"1.5G", 1_500_000_000, false},
		{"2GiB", 2 << 30, false},
		{"12 mb", 12_000_000, false},
		{"5x", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSize(tc.in)
			if (err != nil) != tc.err || got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, %v", tc.in, got, err)
			}
		})
	}
}

func TestParseValidAndDefaults(t *testing.T) {
	e, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	s := e.StudentSpec()
	if s.MemoryBytes != 256<<20 || s.DiskBytes != 64<<20 || s.MaxPIDs != 128 || s.Network.Mode != rt.NetworkNone {
		t.Fatalf("bad spec: %+v", s)
	}
	if s.Timeout != 30*time.Minute || len(s.Workdirs) != 1 {
		t.Fatalf("defaults not applied: %+v", s)
	}
	g := e.GraderSpec()
	if strings.Join(g.Workdirs, ",") != "/grader,/submission" {
		t.Fatalf("grader workdirs: %v", g.Workdirs)
	}
	task, err := e.Task("t1")
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(task.TimeLimit) != 2*time.Minute || task.MaxSteps != 100 || task.FilesDest != "/workspace" {
		t.Fatalf("task defaults: %+v", task)
	}
	l := task.EffectiveLimits()
	if l.MaxFiles != 50 || l.MaxBytes != 1<<20 || l.MaxFileBytes != 1<<20 || l.MaxDepth != 16 {
		t.Fatalf("limits: %+v", l)
	}
	if _, err := e.Task("nope"); err == nil {
		t.Fatal("expected missing task error")
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(string) string
		wantErr string
	}{
		{"no name", func(s string) string { return strings.Replace(s, "name: demo", "name: ''", 1) }, "name is required"},
		{"unknown field", func(s string) string { return s + "bogus: 1\n" }, "bogus"},
		{"relative submission", func(s string) string {
			return strings.Replace(s, "submission: /workspace/repo", "submission: repo", 1)
		}, "submission must be an absolute"},
		{"root submission", func(s string) string {
			return strings.Replace(s, "submission: /workspace/repo", "submission: /", 1)
		}, "submission must be an absolute"},
		{"no grader", func(s string) string {
			return strings.Replace(s, `command: ["python3", "/grader/grade.py"]`, "command: []", 1)
		}, "grader.command"},
		{"bad user", func(s string) string {
			return strings.Replace(s, "- run: chown -R 1000:1000 /workspace", "- run: x\n        user: mallory", 1)
		}, "unknown user"},
		{"same dirs", func(s string) string {
			return strings.Replace(s, "hidden: tasks/t1/hidden", "hidden: tasks/t1/files", 1)
		}, "files and hidden must differ"},
		{"bad size", func(s string) string { return strings.Replace(s, "256Mi", "lots", 1) }, "size"},
		{"bad net", func(s string) string { return strings.Replace(s, "inodes: 5000", "inodes: 5000\n  network: bridge", 1) }, "network mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mut(validYAML)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadAndTar(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"env.yaml":                    validYAML,
		"tasks/t1/files/repo/main.py": "print('hi')",
		"tasks/t1/hidden/grade.py":    "SECRET",
	})
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := e.Task("t1")
	files := tarNames(t, mustTar(task.FilesTar()))
	if strings.Join(files, ",") != "repo/,repo/main.py" {
		t.Fatalf("files tar: %v", files)
	}
	hidden := tarNames(t, mustTar(task.HiddenTar()))
	if strings.Join(hidden, ",") != "grade.py" {
		t.Fatalf("hidden tar: %v", hidden)
	}

	all, err := LoadAll(filepath.Dir(dir))
	if err != nil || all["demo"] == nil {
		t.Fatalf("LoadAll: %v %v", all, err)
	}
}

func TestTarRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"env.yaml": validYAML, "tasks/t1/files/a": "x", "tasks/t1/hidden/h": "y"})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "tasks/t1/files/evil")); err != nil {
		t.Fatal(err)
	}
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := e.Task("t1")
	if _, err := task.FilesTar(); err == nil || !strings.Contains(err.Error(), "only regular files") {
		t.Fatalf("want symlink rejection, got %v", err)
	}
}

func TestLoadMissingDir(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"env.yaml": validYAML})
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for missing task dirs")
	}
}

func mustTar(r io.Reader, err error) io.Reader {
	if err != nil {
		panic(err)
	}
	return r
}

func tarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func TestUserByName(t *testing.T) {
	for name, uid := range map[string]int{"": 0, "root": 0, "student": 1000, "service": 1001, "untrusted": 2000} {
		u, err := UserByName(name)
		if err != nil || u.UID != uid {
			t.Fatalf("%q -> %v %v", name, u, err)
		}
	}
}
