//go:build integration

package docker

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/fake"
)

const testImage = "python:3.12-alpine"

func newRuntime(t *testing.T) *Runtime { return newRuntimeOpts(t, Options{PullMissing: true}) }

// newRuntimeOpts connects to Docker. ARENA_DOCKER_RUNTIME=runsc runs the
// whole suite under gVisor.
func newRuntimeOpts(t *testing.T, opts Options) *Runtime {
	t.Helper()
	opts.OCIRuntime = os.Getenv("ARENA_DOCKER_RUNTIME")
	r, err := New(context.Background(), opts)
	if err != nil {
		if os.Getenv("ARENA_DOCKER_RUNTIME") != "" {
			t.Fatalf("requested OCI runtime unavailable: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func newSandbox(t *testing.T, r *Runtime, mut func(*rt.Spec)) *Sandbox {
	t.Helper()
	spec := rt.Spec{
		Image: testImage, CPUs: 1, MemoryBytes: 256 << 20, MaxPIDs: 64,
		DiskBytes: 16 << 20, MaxInodes: 2000, Timeout: 2 * time.Minute,
		Network: rt.NetworkPolicy{Mode: rt.NetworkNone}, Workdirs: []string{"/workspace"},
	}
	if mut != nil {
		mut(&spec)
	}
	sb, err := r.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	d := sb.(*Sandbox)
	if _, err := d.Exec(context.Background(), rt.ExecRequest{Cmd: []string{"chown", "1000:1000", "/workspace"}, User: rt.Root}); err != nil {
		t.Fatal(err)
	}
	return d
}

func sh(t *testing.T, sb *Sandbox, u rt.User, script string) rt.Result {
	t.Helper()
	res, err := sb.Exec(context.Background(), rt.ExecRequest{Cmd: []string{"sh", "-c", script}, User: u, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	return res
}

func TestIsolationGuarantees(t *testing.T) {
	r := newRuntime(t)
	sb := newSandbox(t, r, nil)
	tests := []struct {
		name     string
		user     rt.User
		script   string
		wantCode int // -1 = any non-zero
		wantOut  string
	}{
		{"student uid", rt.Student, "id -u", 0, "1000"},
		{"student writes workspace", rt.Student, "echo hi > /workspace/x && cat /workspace/x", 0, "hi"},
		{"rootfs read-only for root", rt.Root, "touch /etc/pwned", -1, ""},
		{"rootfs read-only for student", rt.Student, "touch /usr/pwned", -1, ""},
		{"no network", rt.Student, "python3 -c \"import socket; socket.create_connection(('1.1.1.1', 80), timeout=3)\"", -1, ""},
		{"only loopback", rt.Student, "ls /sys/class/net | tr '\\n' ' '", 0, "lo"},
		// busybox `date -s` exits 0 even when settimeofday fails, so check the
		// clock afterwards rather than trusting the exit code.
		{"no clock change", rt.Root, "date -s '2001-01-01 00:00:00' >/dev/null 2>&1; test $(date +%Y) -gt 2020 && echo unchanged", 0, "unchanged"},
		{"student cannot read shadow", rt.Student, "cat /etc/shadow", -1, ""},
		{"no setuid escalation", rt.Student, "su -c id root </dev/null", -1, ""},
		{"no new privs flag", rt.Student, "grep NoNewPrivs /proc/self/status | tr -d '\\t '", 0, "NoNewPrivs:1"},
		{"student has no caps", rt.Student, "grep CapEff /proc/self/status | tr -d '\\t '", 0, "CapEff:0000000000000000"},
		{"disk quota", rt.Student, "dd if=/dev/zero of=/workspace/big bs=1M count=64", -1, ""},
		{"fsize blocks sparse", rt.Student, "truncate -s 1T /workspace/sparse", -1, ""},
		{"pids limited", rt.Student, "cat /sys/fs/cgroup/pids/pids.max 2>/dev/null || cat /sys/fs/cgroup/pids.max", 0, "64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := sh(t, sb, tc.user, tc.script)
			switch {
			case tc.wantCode == -1 && res.ExitCode == 0:
				t.Fatalf("expected failure, got success: %s", res.Stdout)
			case tc.wantCode >= 0 && res.ExitCode != tc.wantCode:
				t.Fatalf("exit %d, stderr %s", res.ExitCode, res.Stderr)
			}
			if tc.wantOut != "" && strings.TrimSpace(string(res.Stdout)) != tc.wantOut {
				t.Fatalf("stdout %q want %q", res.Stdout, tc.wantOut)
			}
		})
	}
}

func TestExecTimeoutStdinTruncation(t *testing.T) {
	r := newRuntime(t)
	sb := newSandbox(t, r, nil)
	ctx := context.Background()
	start := time.Now()
	res, err := sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"sh", "-c", "sleep 30 & sleep 30"}, User: rt.Student, Timeout: 500 * time.Millisecond})
	if err != nil || !res.TimedOut {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	res, err = sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"cat"}, User: rt.Student, Stdin: strings.NewReader("ping"), Timeout: 10 * time.Second})
	if err != nil || string(res.Stdout) != "ping" {
		t.Fatalf("stdin: %+v %v", res, err)
	}
	res, err = sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"sh", "-c", "yes | head -c 100000"}, User: rt.Student, MaxOutput: 1000, Timeout: 10 * time.Second})
	if err != nil || !res.Truncated || len(res.Stdout) != 1000 {
		t.Fatalf("truncation: len=%d truncated=%v err=%v", len(res.Stdout), res.Truncated, err)
	}
}

// killModes covers host-PID signalling (root on the docker host) and the
// in-sandbox fallback that unprivileged hosts such as CI runners use.
var killModes = []struct {
	name  string
	force bool
}{{"host-pid", false}, {"in-sandbox", true}}

func TestKillUserVerifiesNoneRemain(t *testing.T) {
	for _, m := range killModes {
		t.Run(m.name, func(t *testing.T) { testKillUser(t, m.force) })
	}
}

func testKillUser(t *testing.T, force bool) {
	r := newRuntimeOpts(t, Options{PullMissing: true, ForceExecKill: force})
	sb := newSandbox(t, r, nil)
	if force && sb.HostPIDKill() {
		t.Fatal("ForceExecKill ignored")
	}
	ctx := context.Background()
	sh(t, sb, rt.Student, "for i in 1 2 3; do (setsid sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 &); done; nohup sleep 1000 >/dev/null 2>&1 &")
	sh(t, sb, rt.Service, "nohup sleep 1000 >/dev/null 2>&1 &")
	if pids, _ := sb.userPIDs(ctx, rt.Student.UID); len(pids) < 4 {
		t.Fatalf("expected background processes, got %v", pids)
	}
	if err := sb.KillUser(ctx, rt.Student.UID); err != nil {
		t.Fatal(err)
	}
	if pids, _ := sb.userPIDs(ctx, rt.Student.UID); len(pids) != 0 {
		t.Fatalf("survivors: %v", pids)
	}
	if pids, _ := sb.userPIDs(ctx, rt.Service.UID); len(pids) != 1 {
		t.Fatalf("other users must be untouched: %v", pids)
	}
	if err := sb.KillUser(ctx, 0); err == nil {
		t.Fatal("KillUser(0) must be refused")
	}
}

func TestKillUserForkBomb(t *testing.T) {
	for _, m := range killModes {
		t.Run(m.name, func(t *testing.T) { testKillUserForkBomb(t, m.force) })
	}
}

func testKillUserForkBomb(t *testing.T, force bool) {
	r := newRuntimeOpts(t, Options{PullMissing: true, ForceExecKill: force})
	sb := newSandbox(t, r, nil)
	ctx := context.Background()
	_, _ = sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"sh", "-c", "bomb() { bomb | bomb & }; bomb"}, User: rt.Student, Timeout: 2 * time.Second})
	if err := sb.KillUser(ctx, rt.Student.UID); err != nil {
		t.Fatal(err)
	}
	res := sh(t, sb, rt.Student, "echo alive")
	if strings.TrimSpace(string(res.Stdout)) != "alive" {
		t.Fatalf("sandbox unusable after fork bomb: %+v", res)
	}
}

func TestCopyInOutSpecialFiles(t *testing.T) {
	r := newRuntime(t)
	sb := newSandbox(t, r, nil)
	ctx := context.Background()
	if err := sb.CopyIn(ctx, "/workspace/repo", fake.TarFiles(map[string]string{"a.txt": "A", "d/b.txt": "B"}), rt.Student); err != nil {
		t.Fatal(err)
	}
	if out := sh(t, sb, rt.Student, "stat -c %u /workspace/repo/d/b.txt && cat /workspace/repo/d/b.txt"); !strings.Contains(string(out.Stdout), "1000") {
		t.Fatalf("owner/content: %s %s", out.Stdout, out.Stderr)
	}
	sh(t, sb, rt.Student, "cd /workspace/repo && ln -s /etc/shadow link && mkfifo pipe && ln a.txt hard")
	rc, err := sb.CopyOut(ctx, "/workspace/repo")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	types := map[string]byte{}
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types[h.Name] = h.Typeflag
	}
	want := map[string]byte{"repo/link": tar.TypeSymlink, "repo/pipe": tar.TypeFifo, "repo/d/b.txt": tar.TypeReg}
	for k, v := range want {
		if types[k] != v {
			t.Fatalf("%s: type %q want %q (all %v)", k, types[k], v, types)
		}
	}
	if types["repo/hard"] != tar.TypeLink && types["repo/a.txt"] != tar.TypeLink {
		t.Fatalf("hardlink not encoded as link: %v", types)
	}
	if _, err := sb.CopyOut(ctx, "/workspace/nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
}

func TestDestroyIdempotentAndLifetime(t *testing.T) {
	r := newRuntime(t)
	sb := newSandbox(t, r, func(s *rt.Spec) { s.Timeout = 2 * time.Second })
	ctx := context.Background()
	if _, err := sb.Stats(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if _, err := sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"true"}, User: rt.Root}); !errors.Is(err, rt.ErrDestroyed) {
		t.Fatalf("lifetime not enforced: %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAllowlistUnsupported(t *testing.T) {
	r := newRuntime(t)
	_, err := r.Create(context.Background(), rt.Spec{Image: testImage, Timeout: time.Minute, Network: rt.NetworkPolicy{Mode: rt.NetworkAllowlist, Allow: []string{"x"}}})
	if !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

// A process that holds all of the sandbox's memory must not stop custody
// from killing it (the in-sandbox path needs headroom to start `kill`).
func TestKillUserMemoryHog(t *testing.T) {
	for _, m := range killModes {
		t.Run(m.name, func(t *testing.T) {
			r := newRuntimeOpts(t, Options{PullMissing: true, ForceExecKill: m.force})
			sb := newSandbox(t, r, func(s *rt.Spec) { s.MemoryBytes = 96 << 20 })
			ctx := context.Background()
			// Allocate in chunks until ~90% of the limit, then hold it.
			sh(t, sb, rt.Student, `nohup python3 -c "
import time
x=[]
try:
    while len(x) < 86: x.append(bytearray(1<<20))
except MemoryError:
    pass
time.sleep(600)" >/dev/null 2>&1 &
sleep 2`)
			if pids, _ := sb.userPIDs(ctx, rt.Student.UID); len(pids) == 0 {
				t.Fatal("memory hog not running")
			}
			if err := sb.KillUser(ctx, rt.Student.UID); err != nil {
				t.Fatal(err)
			}
			if pids, _ := sb.userPIDs(ctx, rt.Student.UID); len(pids) != 0 {
				t.Fatalf("survivors: %v", pids)
			}
		})
	}
}
