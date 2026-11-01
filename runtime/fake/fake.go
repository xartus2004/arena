// Package fake is an in-memory runtime.Runtime for unit tests.
//
// It executes nothing: commands are dispatched to a pluggable handler. It
// models a filesystem (including symlinks, FIFOs, device nodes, hardlinks and
// sparse files) so custody logic can be tested without Docker, and it models
// a process table so KillUser semantics can be tested. It provides NO
// isolation and must never be used to run untrusted code.
package fake

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rt "github.com/xartus2004/arena/runtime"
)

// Kind is a filesystem node type.
type Kind int

// Node kinds.
const (
	Dir Kind = iota
	File
	Symlink
	FIFO
	CharDevice
	BlockDevice
	Hardlink // a second name for another node; Target is the other path
)

// Node is one filesystem entry.
type Node struct {
	Kind   Kind
	Mode   int64
	UID    int
	GID    int
	Data   []byte
	Size   int64 // apparent size; if > len(Data) the file is sparse
	Target string
	NLink  int
}

// Handler executes a command in a fake sandbox. Returning handled=false
// falls through to the built-in commands.
type Handler func(ctx context.Context, sb *Sandbox, req rt.ExecRequest) (res rt.Result, handled bool, err error)

// Runtime is the fake runtime.
type Runtime struct {
	// Handler, if set, is consulted before built-in commands.
	Handler Handler
	// CreateDelay simulates provisioning latency.
	CreateDelay time.Duration
	// FailCreate makes the next N Create calls fail with an infra error.
	FailCreate atomic.Int32

	mu        sync.Mutex
	seq       int
	sandboxes map[string]*Sandbox
	created   atomic.Int64
}

// New returns an empty fake runtime.
func New() *Runtime { return &Runtime{sandboxes: map[string]*Sandbox{}} }

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "fake" }

// Capabilities implements runtime.Runtime. The fake enforces nothing.
func (r *Runtime) Capabilities() rt.Capabilities { return rt.Capabilities{} }

// Created reports how many sandboxes were successfully created.
func (r *Runtime) Created() int64 { return r.created.Load() }

// Live returns the sandboxes that have not been destroyed.
func (r *Runtime) Live() []*Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Sandbox
	for _, s := range r.sandboxes {
		if !s.Destroyed() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// Create implements runtime.Runtime.
func (r *Runtime) Create(ctx context.Context, spec rt.Spec) (rt.Sandbox, error) {
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("fake: invalid spec: %w", err)
	}
	if r.CreateDelay > 0 {
		select {
		case <-time.After(r.CreateDelay):
		case <-ctx.Done():
			return nil, rt.Infra("create", ctx.Err())
		}
	}
	for {
		n := r.FailCreate.Load()
		if n <= 0 {
			break
		}
		if r.FailCreate.CompareAndSwap(n, n-1) {
			return nil, rt.Infra("create", errors.New("fake: injected create failure"))
		}
	}
	r.mu.Lock()
	r.seq++
	id := fmt.Sprintf("fake-%d", r.seq)
	sb := &Sandbox{
		id:    id,
		rt:    r,
		Spec:  spec,
		nodes: map[string]*Node{"/": {Kind: Dir, Mode: 0o755}, "/tmp": {Kind: Dir, Mode: 0o1777}},
		procs: map[int]int{},
	}
	for _, d := range spec.Workdirs {
		sb.nodes[path.Clean(d)] = &Node{Kind: Dir, Mode: 0o755}
	}
	r.sandboxes[id] = sb
	r.mu.Unlock()
	r.created.Add(1)
	return sb, nil
}

// Sandbox is a fake sandbox. Its exported helpers let tests shape state.
type Sandbox struct {
	id   string
	rt   *Runtime
	Spec rt.Spec

	mu        sync.Mutex
	nodes     map[string]*Node
	procs     map[int]int // pid -> uid
	nextPID   int
	destroyed bool
	execLog   []rt.ExecRequest
	// StubbornUIDs: processes of these uids survive KillUser (to test the
	// "verify none remain" path).
	stubborn map[int]bool
	// FailExec makes Exec return an infra error.
	failExec atomic.Bool
}

// ID implements runtime.Sandbox.
func (s *Sandbox) ID() string { return s.id }

// Destroyed reports whether Destroy has been called.
func (s *Sandbox) Destroyed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.destroyed
}

// SetFailExec makes subsequent Exec calls fail with an infra error.
func (s *Sandbox) SetFailExec(v bool) { s.failExec.Store(v) }

// SetStubborn makes processes of uid survive KillUser.
func (s *Sandbox) SetStubborn(uid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stubborn == nil {
		s.stubborn = map[int]bool{}
	}
	s.stubborn[uid] = true
}

// Spawn adds a fake background process owned by uid and returns its pid.
func (s *Sandbox) Spawn(uid int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextPID++
	s.procs[s.nextPID] = uid
	return s.nextPID
}

// Procs returns the number of live processes owned by uid.
func (s *Sandbox) Procs(uid int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, u := range s.procs {
		if u == uid {
			n++
		}
	}
	return n
}

// ExecLog returns the commands executed so far.
func (s *Sandbox) ExecLog() []rt.ExecRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rt.ExecRequest(nil), s.execLog...)
}

// Put creates or replaces a node at p, creating parent directories.
func (s *Sandbox) Put(p string, n Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(path.Clean(p), n)
}

func (s *Sandbox) putLocked(p string, n Node) {
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		if _, ok := s.nodes[d]; !ok {
			s.nodes[d] = &Node{Kind: Dir, Mode: 0o755, UID: n.UID, GID: n.GID}
		}
	}
	if n.Kind == File && n.Size < int64(len(n.Data)) {
		n.Size = int64(len(n.Data))
	}
	if n.NLink == 0 {
		n.NLink = 1
	}
	if n.Kind == Hardlink {
		if t, ok := s.nodes[path.Clean(n.Target)]; ok {
			t.NLink++
			n.NLink = t.NLink
		}
	}
	cp := n
	s.nodes[p] = &cp
}

// Get returns a copy of the node at p.
func (s *Sandbox) Get(p string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[path.Clean(p)]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// ReadFile returns the contents of a regular file.
func (s *Sandbox) ReadFile(p string) ([]byte, bool) {
	n, ok := s.Get(p)
	if !ok || n.Kind != File {
		return nil, false
	}
	return n.Data, true
}

func (s *Sandbox) checkAlive() error {
	if s.destroyed {
		return rt.ErrDestroyed
	}
	return nil
}

// Exec implements runtime.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req rt.ExecRequest) (rt.Result, error) {
	start := time.Now()
	s.mu.Lock()
	if err := s.checkAlive(); err != nil {
		s.mu.Unlock()
		return rt.Result{}, err
	}
	s.execLog = append(s.execLog, req)
	s.mu.Unlock()
	if s.failExec.Load() {
		return rt.Result{}, rt.Infra("exec", errors.New("fake: injected exec failure"))
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	if s.rt.Handler != nil {
		res, handled, err := s.rt.Handler(ctx, s, req)
		if handled {
			res.Duration = time.Since(start)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.TimedOut = true
				res.ExitCode = 137
				err = nil
			}
			return s.limit(res, req), err
		}
	}
	res := s.builtin(req)
	res.Duration = time.Since(start)
	return s.limit(res, req), nil
}

func (s *Sandbox) limit(res rt.Result, req rt.ExecRequest) rt.Result {
	max := req.MaxOutput
	if max <= 0 {
		max = rt.DefaultMaxOutput
	}
	if len(res.Stdout) > max {
		res.Stdout, res.Truncated = res.Stdout[:max], true
	}
	if len(res.Stderr) > max {
		res.Stderr, res.Truncated = res.Stderr[:max], true
	}
	return res
}

// builtin implements the handful of commands the platform itself issues
// (read_file / write_file tools and simple probes).
func (s *Sandbox) builtin(req rt.ExecRequest) rt.Result {
	fail := func(code int, msg string) rt.Result {
		return rt.Result{ExitCode: code, Stderr: []byte(msg + "\n")}
	}
	c := req.Cmd
	switch {
	case len(c) == 0:
		return fail(127, "fake: empty command")
	case c[0] == "true":
		return rt.Result{}
	case c[0] == "echo":
		return rt.Result{Stdout: []byte(strings.Join(c[1:], " ") + "\n")}
	case len(c) == 3 && c[0] == "cat" && c[1] == "--":
		s.mu.Lock()
		defer s.mu.Unlock()
		n, ok := s.resolveLocked(c[2])
		if !ok {
			return fail(1, "cat: "+c[2]+": No such file or directory")
		}
		if n.Kind != File {
			return fail(1, "cat: "+c[2]+": not a regular file")
		}
		if !canRead(n, req.User) {
			return fail(1, "cat: "+c[2]+": Permission denied")
		}
		return rt.Result{Stdout: append([]byte(nil), n.Data...)}
	case len(c) == 5 && c[0] == "sh" && c[1] == "-c" && strings.HasPrefix(c[2], "mkdir -p") && strings.Contains(c[2], "cat >"):
		// write_file: sh -c 'mkdir -p "$(dirname "$1")" && cat > "$1"' sh PATH
		p := path.Clean(c[4])
		var data []byte
		if req.Stdin != nil {
			data, _ = io.ReadAll(req.Stdin)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if existing, ok := s.nodes[p]; ok && existing.Kind != File {
			return fail(1, "sh: can't create "+p+": not a regular file")
		}
		if !s.canWriteDirLocked(path.Dir(p), req.User) {
			return fail(1, "sh: can't create "+p+": Permission denied")
		}
		s.putLocked(p, Node{Kind: File, Mode: 0o644, UID: req.User.UID, GID: req.User.GID, Data: data})
		return rt.Result{}
	}
	return fail(127, fmt.Sprintf("fake: command not supported: %q", c))
}

func canRead(n *Node, u rt.User) bool {
	if u.UID == 0 || n.UID == u.UID {
		return true
	}
	return n.Mode&0o004 != 0
}

func (s *Sandbox) canWriteDirLocked(dir string, u rt.User) bool {
	if u.UID == 0 {
		return true
	}
	for d := path.Clean(dir); ; d = path.Dir(d) {
		n, ok := s.nodes[d]
		if ok {
			return n.Kind == Dir && (n.UID == u.UID || n.Mode&0o002 != 0)
		}
		if d == "/" {
			return false
		}
	}
}

func (s *Sandbox) resolveLocked(p string) (*Node, bool) {
	p = path.Clean(p)
	for i := 0; i < 16; i++ {
		n, ok := s.nodes[p]
		if !ok {
			return nil, false
		}
		switch n.Kind {
		case Symlink:
			t := n.Target
			if !path.IsAbs(t) {
				t = path.Join(path.Dir(p), t)
			}
			p = path.Clean(t)
		case Hardlink:
			p = path.Clean(n.Target)
		default:
			return n, true
		}
	}
	return nil, false
}

// CopyIn implements runtime.Sandbox.
func (s *Sandbox) CopyIn(ctx context.Context, dir string, tarStream io.Reader, owner rt.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkAlive(); err != nil {
		return err
	}
	dir = path.Clean(dir)
	if _, ok := s.nodes[dir]; !ok {
		s.putLocked(dir, Node{Kind: Dir, Mode: 0o755, UID: owner.UID, GID: owner.GID})
	}
	tr := tar.NewReader(tarStream)
	for {
		if err := ctx.Err(); err != nil {
			return rt.Infra("copyin", err)
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("fake: copyin: %w", err)
		}
		name := path.Clean("/" + h.Name)
		p := path.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			s.putLocked(p, Node{Kind: Dir, Mode: h.Mode & 0o7777, UID: owner.UID, GID: owner.GID})
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("fake: copyin read %s: %w", h.Name, err)
			}
			s.putLocked(p, Node{Kind: File, Mode: h.Mode & 0o7777, UID: owner.UID, GID: owner.GID, Data: data})
		case tar.TypeSymlink:
			s.putLocked(p, Node{Kind: Symlink, Mode: 0o777, UID: owner.UID, GID: owner.GID, Target: h.Linkname})
		default:
			return fmt.Errorf("fake: copyin: unsupported entry type %q for %s", h.Typeflag, h.Name)
		}
	}
}

// CopyOut implements runtime.Sandbox. Names in the stream are relative to
// the parent of p (like `tar -C dirname(p) -c basename(p)`).
func (s *Sandbox) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkAlive(); err != nil {
		return nil, err
	}
	p = path.Clean(p)
	root, ok := s.nodes[p]
	if !ok {
		return nil, fmt.Errorf("fake: copyout %s: no such file or directory", p)
	}
	var paths []string
	if root.Kind == Dir {
		for k := range s.nodes {
			if k == p || strings.HasPrefix(k, p+"/") {
				paths = append(paths, k)
			}
		}
	} else {
		paths = []string{p}
	}
	sort.Strings(paths)
	parent := path.Dir(p)
	// Large sparse files are streamed lazily through a pipe so that tests
	// can verify custody aborts without materialising the data.
	pr, pw := io.Pipe()
	type item struct {
		h    tar.Header
		node Node
	}
	var items []item
	for _, k := range paths {
		n := *s.nodes[k]
		rel := strings.TrimPrefix(strings.TrimPrefix(k, parent), "/")
		h := tar.Header{Name: rel, Mode: n.Mode, Uid: n.UID, Gid: n.GID, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		switch n.Kind {
		case Dir:
			h.Typeflag, h.Name = tar.TypeDir, rel+"/"
		case File:
			h.Typeflag, h.Size = tar.TypeReg, n.Size
		case Symlink:
			h.Typeflag, h.Linkname = tar.TypeSymlink, n.Target
		case FIFO:
			h.Typeflag = tar.TypeFifo
		case CharDevice:
			h.Typeflag = tar.TypeChar
		case BlockDevice:
			h.Typeflag = tar.TypeBlock
		case Hardlink:
			t := path.Clean(n.Target)
			if t == p || strings.HasPrefix(t, p+"/") {
				h.Typeflag, h.Linkname = tar.TypeLink, strings.TrimPrefix(strings.TrimPrefix(t, parent), "/")
			} else if tn, ok := s.nodes[t]; ok && tn.Kind == File {
				// Like real tar: a hardlink whose twin is outside the
				// archive is emitted as a regular file. Record nlink.
				h.Typeflag, h.Size = tar.TypeReg, tn.Size
				n.Data = tn.Data
				h.PAXRecords = map[string]string{"SCHILY.nlink": fmt.Sprint(n.NLink)}
			}
		}
		if n.Kind == File && n.NLink > 1 {
			h.PAXRecords = map[string]string{"SCHILY.nlink": fmt.Sprint(n.NLink)}
		}
		items = append(items, item{h: h, node: n})
	}
	go func() {
		tw := tar.NewWriter(pw)
		for _, it := range items {
			if err := tw.WriteHeader(&it.h); err != nil {
				pw.CloseWithError(err)
				return
			}
			if it.h.Typeflag == tar.TypeReg && it.h.Size > 0 {
				if _, err := tw.Write(it.node.Data); err != nil {
					pw.CloseWithError(err)
					return
				}
				if pad := it.h.Size - int64(len(it.node.Data)); pad > 0 {
					if _, err := io.CopyN(tw, zeroReader{}, pad); err != nil {
						pw.CloseWithError(err)
						return
					}
				}
			}
		}
		pw.CloseWithError(tw.Close())
	}()
	return pr, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// KillUser implements runtime.Sandbox.
func (s *Sandbox) KillUser(ctx context.Context, uid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkAlive(); err != nil {
		return err
	}
	if !s.stubborn[uid] {
		for pid, u := range s.procs {
			if u == uid {
				delete(s.procs, pid)
			}
		}
	}
	for pid, u := range s.procs {
		if u == uid {
			return fmt.Errorf("fake: process %d of uid %d survived kill", pid, uid)
		}
	}
	return nil
}

// Stats implements runtime.Sandbox.
func (s *Sandbox) Stats(ctx context.Context) (rt.Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkAlive(); err != nil {
		return rt.Stats{}, err
	}
	var mem int64
	for _, n := range s.nodes {
		mem += int64(len(n.Data))
	}
	return rt.Stats{MemoryBytes: mem, PIDs: int64(len(s.procs))}, nil
}

// Destroy implements runtime.Sandbox.
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed = true
	s.procs = map[int]int{}
	return nil
}

// TarFiles builds an in-memory tar stream from name→content (helper for
// tests and CopyIn callers).
func TarFiles(files map[string]string) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(files[n]))
	}
	_ = tw.Close()
	return &buf
}
