package custody

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/fake"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var lim = Limits{MaxFiles: 20, MaxBytes: 1000, MaxFileBytes: 500, MaxDepth: 4}

type entry struct {
	name     string
	typ      byte
	body     string
	linkname string
	pax      map[string]string
}

func mkTar(t *testing.T, es ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.linkname, PAXRecords: e.pax}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtractTable(t *testing.T) {
	ok := []entry{{name: "repo/", typ: tar.TypeDir}, {name: "repo/a.py", typ: tar.TypeReg, body: "print(1)"}}
	deep := []entry{{name: "repo/", typ: tar.TypeDir}}
	p := "repo"
	for i := 0; i < 6; i++ {
		p += "/d"
		deep = append(deep, entry{name: p + "/", typ: tar.TypeDir})
	}
	many := []entry{{name: "repo/", typ: tar.TypeDir}}
	for i := 0; i < 25; i++ {
		many = append(many, entry{name: "repo/f" + strings.Repeat("x", i), typ: tar.TypeReg, body: "1"})
	}
	tests := []struct {
		name     string
		entries  []entry
		wantKind Kind // "" = success
		wantN    int
	}{
		{"ok", ok, "", 1},
		{"symlink", append(ok, entry{name: "repo/l", typ: tar.TypeSymlink, linkname: "/etc/shadow"}), KindSymlink, 0},
		{"relative symlink", append(ok, entry{name: "repo/l", typ: tar.TypeSymlink, linkname: "a.py"}), KindSymlink, 0},
		{"fifo", append(ok, entry{name: "repo/p", typ: tar.TypeFifo}), KindSpecial, 0},
		{"char device", append(ok, entry{name: "repo/null", typ: tar.TypeChar}), KindSpecial, 0},
		{"block device", append(ok, entry{name: "repo/sda", typ: tar.TypeBlock}), KindSpecial, 0},
		{"hardlink entry", append(ok, entry{name: "repo/h", typ: tar.TypeLink, linkname: "repo/a.py"}), KindHardlink, 0},
		{"hardlink nlink", append(ok, entry{name: "repo/h", typ: tar.TypeReg, body: "x", pax: map[string]string{"SCHILY.nlink": "2"}}), KindHardlink, 0},
		{"too deep", deep, KindTooDeep, 0},
		{"too many files", many, KindTooManyFiles, 0},
		{"file too large", append(ok, entry{name: "repo/big", typ: tar.TypeReg, body: strings.Repeat("x", 501)}), KindFileTooLarge, 0},
		{"total too large", append(ok,
			entry{name: "repo/b1", typ: tar.TypeReg, body: strings.Repeat("x", 400)},
			entry{name: "repo/b2", typ: tar.TypeReg, body: strings.Repeat("x", 400)},
			entry{name: "repo/b3", typ: tar.TypeReg, body: strings.Repeat("x", 400)}), KindTooLarge, 0},
		{"dotdot", append(ok, entry{name: "repo/../../etc/x", typ: tar.TypeReg, body: "x"}), KindBadPath, 0},
		{"absolute", append(ok, entry{name: "/etc/x", typ: tar.TypeReg, body: "x"}), KindBadPath, 0},
		{"second root", append(ok, entry{name: "other/x", typ: tar.TypeReg, body: "x"}), KindBadPath, 0},
		{"duplicate", append(ok, entry{name: "repo/a.py", typ: tar.TypeReg, body: "y"}), KindBadPath, 0},
		{"single file submission", []entry{{name: "answer.txt", typ: tar.TypeReg, body: "42"}}, "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "custody")
			m, err := Extract(context.Background(), mkTar(t, tc.entries...), dest, lim)
			if tc.wantKind == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(m.Files) != tc.wantN {
					t.Fatalf("files = %d want %d (%+v)", len(m.Files), tc.wantN, m.Files)
				}
				st, err := os.Stat(dest)
				if err != nil || st.Mode().Perm() != 0o700 {
					t.Fatalf("dest must be 0700: %v %v", st.Mode(), err)
				}
				return
			}
			var v *Violation
			if !errors.As(err, &v) || v.Kind != tc.wantKind {
				t.Fatalf("got %v, want violation %s", err, tc.wantKind)
			}
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Fatalf("dest must be removed on violation, stat err = %v", statErr)
			}
		})
	}
}

func TestSparseHeaderRejectedWithoutReading(t *testing.T) {
	// A fake sandbox serving a 1 TiB sparse file: custody must reject it from
	// the header alone, in well under a second.
	ctx := context.Background()
	sb, err := fake.New().Create(ctx, rt.Spec{Image: "x", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f := sb.(*fake.Sandbox)
	f.Put("/workspace/sub/huge", fake.Node{Kind: fake.File, Size: 1 << 40})
	rc, err := sb.CopyOut(ctx, "/workspace/sub")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	start := time.Now()
	_, err = Extract(ctx, rc, filepath.Join(t.TempDir(), "c"), lim)
	var v *Violation
	if !errors.As(err, &v) || v.Kind != KindFileTooLarge {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %v; custody read the sparse data", time.Since(start))
	}
}

func TestLyingHeaderDetected(t *testing.T) {
	// header says 3 bytes, stream is truncated -> treated as a corrupt submission
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644})
	_, _ = tw.Write([]byte("ab"))
	b := buf.Bytes()
	_, err := Extract(context.Background(), bytes.NewReader(b), filepath.Join(t.TempDir(), "c"), lim)
	if err == nil {
		t.Fatal("expected error on truncated stream")
	}
}

func TestManifestAndPack(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "c")
	m, err := Extract(context.Background(), mkTar(t,
		entry{name: "repo/", typ: tar.TypeDir},
		entry{name: "repo/b.txt", typ: tar.TypeReg, body: "bbb"},
		entry{name: "repo/a/x.txt", typ: tar.TypeReg, body: "x"},
	), dest, lim)
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalBytes != 4 || len(m.Files) != 2 || m.Root != "repo" || m.SHA256 == "" {
		t.Fatalf("manifest: %+v", m)
	}
	// The digest must be deterministic.
	dest2 := filepath.Join(t.TempDir(), "c")
	m2, err := Extract(context.Background(), mkTar(t,
		entry{name: "repo/", typ: tar.TypeDir},
		entry{name: "repo/a/x.txt", typ: tar.TypeReg, body: "x"},
		entry{name: "repo/b.txt", typ: tar.TypeReg, body: "bbb"},
	), dest2, lim)
	if err != nil || m2.SHA256 != m.SHA256 {
		t.Fatalf("digest not order-independent: %s vs %s (%v)", m.SHA256, m2.SHA256, err)
	}
	var packed bytes.Buffer
	if err := Pack(dest, &packed); err != nil {
		t.Fatal(err)
	}
	// Re-extracting the packed copy yields the same digest.
	dest3 := filepath.Join(t.TempDir(), "c")
	m3, err := Extract(context.Background(), &packed, dest3, lim)
	if err != nil || m3.SHA256 != m.SHA256 {
		t.Fatalf("pack round trip: %v %s vs %s", err, m3.SHA256, m.SHA256)
	}
}

func TestContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Extract(ctx, mkTar(t, entry{name: "a", typ: tar.TypeReg, body: "x"}), filepath.Join(t.TempDir(), "c"), lim)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
