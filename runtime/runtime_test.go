package runtime

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSpecValidate(t *testing.T) {
	ok := Spec{Image: "img", Timeout: time.Minute}
	tests := []struct {
		name    string
		mut     func(*Spec)
		wantErr string
	}{
		{"valid", func(*Spec) {}, ""},
		{"no image", func(s *Spec) { s.Image = "" }, "image is required"},
		{"no timeout", func(s *Spec) { s.Timeout = 0 }, "timeout"},
		{"negative mem", func(s *Spec) { s.MemoryBytes = -1 }, "limits"},
		{"bad net", func(s *Spec) { s.Network.Mode = "bridge" }, "unknown network mode"},
		{"empty allowlist", func(s *Spec) { s.Network.Mode = NetworkAllowlist }, "at least one host"},
		{"allowlist ok", func(s *Spec) {
			s.Network = NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"pypi.org"}}
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.mut(&s)
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSpecKeyIgnoresLabels(t *testing.T) {
	a := Spec{Image: "x", Timeout: time.Second, Labels: map[string]string{"a": "1"}}
	b := Spec{Image: "x", Timeout: time.Second, Labels: map[string]string{"b": "2"}}
	if a.Key() != b.Key() {
		t.Fatalf("keys differ: %q vs %q", a.Key(), b.Key())
	}
	b.MemoryBytes = 1
	if a.Key() == b.Key() {
		t.Fatal("keys should differ when limits differ")
	}
}

func TestInfraError(t *testing.T) {
	base := errors.New("boom")
	err := Infra("create", base)
	if !IsInfra(err) || !errors.Is(err, base) {
		t.Fatalf("Infra wrapping broken: %v", err)
	}
	if !IsInfra(fmt.Errorf("outer: %w", err)) {
		t.Fatal("IsInfra should see through wrapping")
	}
	if Infra("x", nil) != nil {
		t.Fatal("Infra(nil) must be nil")
	}
	if IsInfra(base) {
		t.Fatal("plain error is not infra")
	}
	if Infra("again", err) != err {
		t.Fatal("double wrapping should be a no-op")
	}
}

func TestLimitedBuffer(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		writes    []string
		want      string
		truncated bool
	}{
		{"under", 10, []string{"abc"}, "abc", false},
		{"exact", 3, []string{"abc"}, "abc", false},
		{"over in one", 3, []string{"abcdef"}, "abc", true},
		{"over across", 4, []string{"ab", "cd", "ef"}, "abcd", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewLimitedBuffer(tc.max)
			for _, w := range tc.writes {
				n, err := b.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if got := string(b.Bytes()); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if b.Truncated() != tc.truncated {
				t.Fatalf("truncated = %v", b.Truncated())
			}
		})
	}
}
