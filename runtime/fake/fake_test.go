package fake

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	rt "github.com/xartus2004/arena/runtime"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var spec = rt.Spec{Image: "fake", Timeout: time.Minute, Workdirs: []string{"/workspace"}}

func newSB(t *testing.T) *Sandbox {
	t.Helper()
	sb, err := New().Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return sb.(*Sandbox)
}

func TestReadWritePermissions(t *testing.T) {
	ctx := context.Background()
	sb := newSB(t)
	sb.Put("/workspace", Node{Kind: Dir, Mode: 0o755, UID: 1000, GID: 1000})
	sb.Put("/secret", Node{Kind: File, Mode: 0o600, Data: []byte("key")})

	write := func(u rt.User, p, data string) rt.Result {
		r, err := sb.Exec(ctx, rt.ExecRequest{
			Cmd:   []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", p},
			User:  u,
			Stdin: strings.NewReader(data),
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	read := func(u rt.User, p string) rt.Result {
		r, err := sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"cat", "--", p}, User: u})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	tests := []struct {
		name     string
		run      func() rt.Result
		wantCode int
		wantOut  string
	}{
		{"student writes workspace", func() rt.Result { return write(rt.Student, "/workspace/a.txt", "hi") }, 0, ""},
		{"student reads own", func() rt.Result { return read(rt.Student, "/workspace/a.txt") }, 0, "hi"},
		{"student cannot read secret", func() rt.Result { return read(rt.Student, "/secret") }, 1, ""},
		{"root reads secret", func() rt.Result { return read(rt.Root, "/secret") }, 0, "key"},
		{"student cannot write root dir", func() rt.Result { return write(rt.Student, "/etc/passwd", "x") }, 1, ""},
		{"missing file", func() rt.Result { return read(rt.Student, "/nope") }, 1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run()
			if r.ExitCode != tc.wantCode || string(r.Stdout) != tc.wantOut {
				t.Fatalf("got code=%d out=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
			}
		})
	}
}

func TestCopyOutTypesAndSparse(t *testing.T) {
	sb := newSB(t)
	sb.Put("/workspace/sub/a", Node{Kind: File, Mode: 0o644, Data: []byte("x")})
	sb.Put("/workspace/sub/link", Node{Kind: Symlink, Target: "/etc/shadow"})
	sb.Put("/workspace/sub/fifo", Node{Kind: FIFO})
	sb.Put("/workspace/sub/big", Node{Kind: File, Size: 1 << 40})
	rc, err := sb.CopyOut(context.Background(), "/workspace/sub")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	got := map[string]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h.Typeflag
		if h.Name == "sub/big" {
			if h.Size != 1<<40 {
				t.Fatalf("sparse size = %d", h.Size)
			}
			break // do not read a terabyte of zeros
		}
	}
	want := map[string]byte{"sub/": tar.TypeDir, "sub/a": tar.TypeReg, "sub/big": tar.TypeReg}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("entry %s: got type %q want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestKillUser(t *testing.T) {
	ctx := context.Background()
	sb := newSB(t)
	sb.Spawn(1000)
	sb.Spawn(1000)
	sb.Spawn(1001)
	if err := sb.KillUser(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if sb.Procs(1000) != 0 || sb.Procs(1001) != 1 {
		t.Fatalf("procs after kill: student=%d service=%d", sb.Procs(1000), sb.Procs(1001))
	}
	sb.SetStubborn(1001)
	if err := sb.KillUser(ctx, 1001); err == nil {
		t.Fatal("expected survivor error")
	}
}

func TestDestroyAndInjectedFailures(t *testing.T) {
	ctx := context.Background()
	r := New()
	r.FailCreate.Store(1)
	if _, err := r.Create(ctx, spec); !rt.IsInfra(err) {
		t.Fatalf("want infra error, got %v", err)
	}
	sb, err := r.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	fsb := sb.(*Sandbox)
	fsb.SetFailExec(true)
	if _, err := sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"true"}}); !rt.IsInfra(err) {
		t.Fatalf("want infra exec error, got %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatal("destroy must be idempotent")
	}
	if _, err := sb.Exec(ctx, rt.ExecRequest{Cmd: []string{"true"}}); !errors.Is(err, rt.ErrDestroyed) {
		t.Fatalf("want ErrDestroyed, got %v", err)
	}
	if len(r.Live()) != 0 {
		t.Fatal("no live sandboxes expected")
	}
}

func TestCopyInRoundTrip(t *testing.T) {
	ctx := context.Background()
	sb := newSB(t)
	if err := sb.CopyIn(ctx, "/workspace/repo", TarFiles(map[string]string{"main.py": "print(1)", "pkg/x.py": "y"}), rt.Student); err != nil {
		t.Fatal(err)
	}
	b, ok := sb.ReadFile("/workspace/repo/pkg/x.py")
	if !ok || string(b) != "y" {
		t.Fatalf("got %q %v", b, ok)
	}
	n, _ := sb.Get("/workspace/repo/main.py")
	if n.UID != 1000 {
		t.Fatalf("owner = %d", n.UID)
	}
}
