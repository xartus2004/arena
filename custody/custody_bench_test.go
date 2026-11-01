package custody

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkExtract measures validating + writing a 200-file, 2 MiB submission.
func BenchmarkExtract(b *testing.B) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "repo/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := bytes.Repeat([]byte("x"), 10<<10)
	for i := 0; i < 200; i++ {
		_ = tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("repo/d%d/f%d.py", i%10, i), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	data := buf.Bytes()
	lim := Limits{MaxFiles: 1000, MaxBytes: 16 << 20, MaxFileBytes: 1 << 20, MaxDepth: 8}
	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Extract(context.Background(), bytes.NewReader(data), filepath.Join(dir, fmt.Sprint(i)), lim); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRejectSparseHeader measures rejecting a 1 TiB file from its header.
func BenchmarkRejectSparseHeader(b *testing.B) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "repo/huge", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1 << 40})
	data := buf.Bytes() // header only: the body is never read
	lim := Limits{MaxFiles: 10, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxDepth: 4}
	dir := b.TempDir()
	for i := 0; i < b.N; i++ {
		if _, err := Extract(context.Background(), bytes.NewReader(data), filepath.Join(dir, fmt.Sprint(i)), lim); err == nil {
			b.Fatal("expected violation")
		}
	}
}
