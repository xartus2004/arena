// Package custody takes a submission out of an untrusted sandbox and into a
// root-only directory on the host, refusing anything that could trick or
// crash a grader.
//
// The input is a tar stream produced by runtime.Sandbox.CopyOut, so custody
// never touches the student's filesystem directly and never follows a
// symlink. Every entry is checked against the rules below *from its header,
// before any data is read*, so a 1 TiB sparse file is rejected without
// reading a byte of it:
//
//   - only regular files and directories are accepted (no symlinks, FIFOs,
//     sockets, char/block devices, hardlinks);
//   - regular files whose link count is > 1 are rejected (a hardlink whose
//     twin lives outside the submission);
//   - paths must be relative, free of "..", unique, and under one root;
//   - depth, file count, per-file size and total size (apparent size, so
//     sparse files count at full length) are bounded.
//
// On any violation the partially written copy is deleted.
package custody

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Limits bounds an acceptable submission.
type Limits struct {
	MaxFiles     int   `json:"max_files"`
	MaxBytes     int64 `json:"max_bytes"`
	MaxFileBytes int64 `json:"max_file_bytes"`
	MaxDepth     int   `json:"max_depth"`
}

// Kind classifies a custody violation.
type Kind string

// Violation kinds.
const (
	KindSymlink      Kind = "symlink"
	KindHardlink     Kind = "hardlink"
	KindSpecial      Kind = "special_file" // fifo, socket, device
	KindTooDeep      Kind = "too_deep"
	KindTooManyFiles Kind = "too_many_files"
	KindFileTooLarge Kind = "file_too_large"
	KindTooLarge     Kind = "too_large"
	KindBadPath      Kind = "bad_path"
	KindCorrupt      Kind = "corrupt"
	KindMissing      Kind = "missing"
	KindProcesses    Kind = "processes_survived"
)

// Violation is a submission rejected by custody. It is the agent's fault,
// not an infrastructure failure: the episode scores 0.
type Violation struct {
	Kind   Kind   `json:"kind"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (v *Violation) Error() string {
	if v.Path == "" {
		return fmt.Sprintf("custody violation %s: %s", v.Kind, v.Detail)
	}
	return fmt.Sprintf("custody violation %s at %q: %s", v.Kind, v.Path, v.Detail)
}

// File is one regular file in the custody copy.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes an accepted submission.
type Manifest struct {
	Root       string   `json:"root"`
	Dirs       []string `json:"dirs"`
	Files      []File   `json:"files"`
	TotalBytes int64    `json:"total_bytes"`
	Entries    int      `json:"entries"`
	// SHA256 is a deterministic digest of every file's path and content.
	SHA256 string `json:"sha256"`
}

// Extract validates a tar stream and writes it under dest, which must not
// exist and is created with mode 0700. It returns a *Violation (wrapped) for
// rejected submissions and other errors for I/O or context failures.
func Extract(ctx context.Context, r io.Reader, dest string, lim Limits) (m Manifest, err error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, fmt.Errorf("custody: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return Manifest{}, fmt.Errorf("custody: mkdir parent: %w", err)
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("custody: create %s: %w", dest, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dest)
		}
	}()
	// Ensure the mode survives a restrictive/permissive umask.
	if err := os.Chmod(dest, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("custody: chmod: %w", err)
	}
	tr := tar.NewReader(r)
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return Manifest{}, fmt.Errorf("custody: %w", err)
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, &Violation{Kind: KindCorrupt, Detail: err.Error()}
		}
		rel, verr := checkName(h.Name)
		if verr != nil {
			return Manifest{}, verr
		}
		root := strings.SplitN(rel, "/", 2)[0]
		if m.Root == "" {
			m.Root = root
		} else if root != m.Root {
			return Manifest{}, &Violation{Kind: KindBadPath, Path: rel, Detail: "entry outside the submission root"}
		}
		if seen[rel] {
			return Manifest{}, &Violation{Kind: KindBadPath, Path: rel, Detail: "duplicate entry"}
		}
		seen[rel] = true
		m.Entries++
		if m.Entries > lim.MaxFiles {
			return Manifest{}, &Violation{Kind: KindTooManyFiles, Path: rel, Detail: fmt.Sprintf("more than %d entries", lim.MaxFiles)}
		}
		// Depth counts components below the root.
		if d := strings.Count(rel, "/"); d > lim.MaxDepth {
			return Manifest{}, &Violation{Kind: KindTooDeep, Path: rel, Detail: fmt.Sprintf("depth %d > %d", d, lim.MaxDepth)}
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdirNoFollow(dest, rel); err != nil {
				return Manifest{}, err
			}
			m.Dirs = append(m.Dirs, rel)
		case tar.TypeReg: // Go's tar reader normalises legacy TypeRegA to TypeReg
			if n, ok := h.PAXRecords["SCHILY.nlink"]; ok {
				if v, _ := strconv.Atoi(n); v > 1 {
					return Manifest{}, &Violation{Kind: KindHardlink, Path: rel, Detail: fmt.Sprintf("link count %d", v)}
				}
			}
			if h.Size < 0 {
				return Manifest{}, &Violation{Kind: KindCorrupt, Path: rel, Detail: "negative size"}
			}
			if h.Size > lim.MaxFileBytes {
				return Manifest{}, &Violation{Kind: KindFileTooLarge, Path: rel, Detail: fmt.Sprintf("%d bytes > %d", h.Size, lim.MaxFileBytes)}
			}
			if m.TotalBytes+h.Size > lim.MaxBytes {
				return Manifest{}, &Violation{Kind: KindTooLarge, Path: rel, Detail: fmt.Sprintf("total would exceed %d bytes", lim.MaxBytes)}
			}
			if err := mkdirNoFollow(dest, path.Dir(rel)); err != nil {
				return Manifest{}, err
			}
			sum, err := writeFile(target, tr, h.Size)
			if err != nil {
				return Manifest{}, err
			}
			m.TotalBytes += h.Size
			m.Files = append(m.Files, File{Path: rel, Size: h.Size, SHA256: sum})
		case tar.TypeSymlink:
			return Manifest{}, &Violation{Kind: KindSymlink, Path: rel, Detail: "symlink to " + h.Linkname}
		case tar.TypeLink:
			return Manifest{}, &Violation{Kind: KindHardlink, Path: rel, Detail: "hardlink to " + h.Linkname}
		case tar.TypeFifo:
			return Manifest{}, &Violation{Kind: KindSpecial, Path: rel, Detail: "fifo"}
		case tar.TypeChar, tar.TypeBlock:
			return Manifest{}, &Violation{Kind: KindSpecial, Path: rel, Detail: "device node"}
		default:
			return Manifest{}, &Violation{Kind: KindSpecial, Path: rel, Detail: fmt.Sprintf("unsupported tar type %q", h.Typeflag)}
		}
	}
	if m.Entries == 0 {
		return Manifest{}, &Violation{Kind: KindMissing, Detail: "empty submission"}
	}
	m.SHA256 = digest(m)
	return m, nil
}

func checkName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", &Violation{Kind: KindBadPath, Path: name, Detail: "absolute or empty path"}
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "./"), "/")
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." || part == "." || part == "" {
			return "", &Violation{Kind: KindBadPath, Path: name, Detail: "path contains '..', '.' or empty components"}
		}
	}
	return trimmed, nil
}

// mkdirNoFollow creates each component of rel under dest, refusing to
// traverse anything that is not a real directory.
func mkdirNoFollow(dest, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cur := dest
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		switch {
		case err == nil && st.IsDir():
			continue
		case err == nil:
			return &Violation{Kind: KindBadPath, Path: rel, Detail: "path component is not a directory"}
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil {
				return fmt.Errorf("custody: mkdir %s: %w", cur, err)
			}
		default:
			return fmt.Errorf("custody: lstat %s: %w", cur, err)
		}
	}
	return nil
}

func writeFile(target string, r io.Reader, size int64) (string, error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("custody: create %s: %w", target, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size))
	cerr := f.Close()
	if err != nil {
		return "", &Violation{Kind: KindCorrupt, Path: target, Detail: err.Error()}
	}
	if n != size {
		return "", &Violation{Kind: KindCorrupt, Path: target, Detail: fmt.Sprintf("short read: %d of %d bytes", n, size)}
	}
	if cerr != nil {
		return "", fmt.Errorf("custody: close %s: %w", target, cerr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digest(m Manifest) string {
	// Only files count: directories are implied by file paths, and empty
	// directories carry no gradeable content.
	var lines []string
	for _, f := range m.Files {
		lines = append(lines, fmt.Sprintf("f %s %d %s", f.Path, f.Size, f.SHA256))
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

// Pack writes the custody copy under dest as a normalised tar stream
// (root-owned, files 0644, dirs 0755, zero mtimes) for the grader sandbox.
// It refuses anything but regular files and directories.
func Pack(dest string, w io.Writer) error {
	tw := tar.NewWriter(w)
	var paths []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dest {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("custody: pack walk: %w", err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		st, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("custody: pack: %w", err)
		}
		rel, _ := filepath.Rel(dest, p)
		rel = filepath.ToSlash(rel)
		switch {
		case st.IsDir():
			if err := tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				return fmt.Errorf("custody: pack: %w", err)
			}
		case st.Mode().IsRegular():
			if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: st.Size()}); err != nil {
				return fmt.Errorf("custody: pack: %w", err)
			}
			f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return fmt.Errorf("custody: pack open: %w", err)
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return fmt.Errorf("custody: pack copy: %w", err)
			}
		default:
			return fmt.Errorf("custody: pack: refusing non-regular file %s", rel)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("custody: pack: %w", err)
	}
	return nil
}
