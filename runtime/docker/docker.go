// Package docker implements runtime.Runtime on top of the Docker Engine API
// (runc containers).
//
// Every sandbox is a fresh container with:
//
//   - no network (NetworkMode "none"; allowlists are not implemented and
//     return ErrUnsupported rather than silently allowing egress);
//   - a read-only root filesystem; the only writable paths are size- and
//     inode-limited tmpfs mounts (the spec's Workdirs, /tmp and a small
//     /dev/shm), mounted nosuid,nodev — this is the disk quota;
//   - memory (swap disabled), CPU and PIDs cgroup limits, plus an
//     RLIMIT_FSIZE equal to the disk quota so sparse files cannot exceed it;
//   - all capabilities dropped except the few root needs for setup and
//     custody, and no-new-privileges so setuid binaries cannot raise the
//     unprivileged student;
//   - docker-init as PID 1 so orphans are reaped.
//
// Process termination for custody is done from the host: the container is
// frozen (cgroup freezer), the target uid's processes are SIGKILLed by host
// PID, the container is thawed, and the process list is re-read from the
// host until none remain. Nothing inside the sandbox is trusted for this.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-units"

	rt "github.com/xartus2004/arena/runtime"
)

// ManagedLabel marks containers created by arena (used for cleanup).
const ManagedLabel = "arena.managed"

// Options configures the Docker runtime.
type Options struct {
	// Logger (optional).
	Logger *slog.Logger
	// PullMissing pulls images that are not present locally.
	PullMissing bool
}

// Runtime is the Docker runtime.
type Runtime struct {
	cli  *client.Client
	log  *slog.Logger
	opts Options

	imgMu  sync.Mutex
	images map[string]bool
}

// New connects to the Docker daemon from the environment (DOCKER_HOST etc).
func New(ctx context.Context, opts Options) (*Runtime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: client: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker: daemon unreachable: %w", err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := &Runtime{cli: cli, log: opts.Logger.With("runtime", "docker"), opts: opts, images: map[string]bool{}}
	return r, nil
}

// Close releases the client.
func (r *Runtime) Close() error { return r.cli.Close() }

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "docker" }

// Capabilities implements runtime.Runtime.
func (r *Runtime) Capabilities() rt.Capabilities {
	return rt.Capabilities{
		ProcessIsolation: true, FSIsolation: true, NetworkIsolation: true,
		MemoryLimit: true, PIDLimit: true, DiskQuota: true, ClockIsolation: true,
		NoPrivEscalation: true, RealExecution: true,
	}
}

func (r *Runtime) ensureImage(ctx context.Context, ref string) error {
	r.imgMu.Lock()
	ok := r.images[ref]
	r.imgMu.Unlock()
	if ok {
		return nil
	}
	if _, err := r.cli.ImageInspect(ctx, ref); err != nil {
		if !cerrdefs.IsNotFound(err) || !r.opts.PullMissing {
			return fmt.Errorf("docker: image %s: %w", ref, err)
		}
		rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
		if err != nil {
			return fmt.Errorf("docker: pull %s: %w (local images such as arena-python are built with `make images`)", ref, err)
		}
		_, err = io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("docker: pull %s: %w", ref, err)
		}
	}
	r.imgMu.Lock()
	r.images[ref] = true
	r.imgMu.Unlock()
	return nil
}

func tmpfsOpts(size, inodes int64, mode string) string {
	return fmt.Sprintf("rw,nosuid,nodev,size=%d,nr_inodes=%d,mode=%s", size, inodes, mode)
}

// Create implements runtime.Runtime.
func (r *Runtime) Create(ctx context.Context, spec rt.Spec) (rt.Sandbox, error) {
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("docker: invalid spec: %w", err)
	}
	if spec.Network.Mode == rt.NetworkAllowlist {
		return nil, fmt.Errorf("docker: network allowlist: %w (needs an egress proxy; see README roadmap)", rt.ErrUnsupported)
	}
	if err := r.ensureImage(ctx, spec.Image); err != nil {
		return nil, rt.Infra("docker create", err)
	}
	disk := spec.DiskBytes
	if disk <= 0 {
		disk = 256 << 20
	}
	inodes := spec.MaxInodes
	if inodes <= 0 {
		inodes = 20000
	}
	tmp := min(disk, 64<<20)
	tmpfs := map[string]string{"/tmp": tmpfsOpts(tmp, min(inodes, 4096), "1777")}
	for _, d := range spec.Workdirs {
		tmpfs[path.Clean(d)] = tmpfsOpts(disk, inodes, "0755")
	}
	labels := map[string]string{ManagedLabel: "true"}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	pids := spec.MaxPIDs
	if pids <= 0 {
		pids = 256
	}
	init := true
	hc := &container.HostConfig{
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		Tmpfs:          tmpfs,
		ShmSize:        4 << 20,
		CapDrop:        []string{"ALL"},
		// Root inside the sandbox needs these for setup/custody; the
		// student (uid 1000) never holds them: non-root execs start with an
		// empty effective set and no-new-privileges blocks setuid gains.
		CapAdd:      []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "KILL", "SETUID", "SETGID"},
		SecurityOpt: []string{"no-new-privileges"},
		Init:        &init,
		IpcMode:     "private",
		Resources: container.Resources{
			Memory:     spec.MemoryBytes,
			MemorySwap: spec.MemoryBytes, // no swap
			NanoCPUs:   int64(spec.CPUs * 1e9),
			PidsLimit:  &pids,
			Ulimits: []*units.Ulimit{
				{Name: "fsize", Soft: disk, Hard: disk},
				{Name: "nofile", Soft: 1024, Hard: 1024},
				{Name: "core", Soft: 0, Hard: 0},
			},
		},
	}
	if spec.MemoryBytes == 0 {
		hc.Resources.MemorySwap = 0
	}
	cfg := &container.Config{
		Image:           spec.Image,
		Cmd:             []string{"sleep", "2147483647"},
		User:            "0:0",
		Labels:          labels,
		WorkingDir:      "/",
		NetworkDisabled: true,
		Env:             []string{"PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	created, err := r.cli.ContainerCreate(ctx, cfg, hc, nil, nil, "")
	if err != nil {
		return nil, rt.Infra("docker create", err)
	}
	sb := &Sandbox{id: created.ID, r: r, spec: spec, log: r.log.With("sandbox", created.ID[:12])}
	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = sb.Destroy(context.WithoutCancel(ctx))
		return nil, rt.Infra("docker start", err)
	}
	insp, err := r.cli.ContainerInspect(ctx, created.ID)
	if err == nil && insp.State != nil {
		sb.initPID = insp.State.Pid
		sb.hostPIDs = sharesPIDNamespace(insp.State.Pid, created.ID)
	}
	sb.timer = time.AfterFunc(spec.Timeout, func() {
		sb.log.Warn("sandbox lifetime exceeded; destroying", "timeout", spec.Timeout)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sb.Destroy(ctx)
	})
	return sb, nil
}

// sharesPIDNamespace reports whether host PID pid is visible to us and
// belongs to the container (so we can signal its processes directly).
func sharesPIDNamespace(pid int, id string) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	return err == nil && strings.Contains(string(b), id)
}

// Cleanup removes every arena-managed container (e.g. after a crash).
func (r *Runtime) Cleanup(ctx context.Context) (int, error) {
	list, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", ManagedLabel+"=true"))})
	if err != nil {
		return 0, fmt.Errorf("docker: list: %w", err)
	}
	n := 0
	for _, c := range list {
		if err := r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err == nil {
			n++
		}
	}
	return n, nil
}

// Sandbox is a Docker container.
type Sandbox struct {
	id       string
	r        *Runtime
	spec     rt.Spec
	log      *slog.Logger
	initPID  int
	hostPIDs bool
	timer    *time.Timer

	mu        sync.Mutex
	destroyed bool
}

// ID implements runtime.Sandbox.
func (s *Sandbox) ID() string { return s.id }

// HostPIDKill reports whether KillUser can signal processes from the host.
func (s *Sandbox) HostPIDKill() bool { return s.hostPIDs }

func (s *Sandbox) alive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return rt.ErrDestroyed
	}
	return nil
}

// Exec implements runtime.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req rt.ExecRequest) (rt.Result, error) {
	if err := s.alive(); err != nil {
		return rt.Result{}, err
	}
	if len(req.Cmd) == 0 {
		return rt.Result{}, errors.New("docker: empty command")
	}
	start := time.Now()
	stdout := rt.NewLimitedBuffer(req.MaxOutput)
	stderr := rt.NewLimitedBuffer(req.MaxOutput)
	code, timedOut, err := s.exec(ctx, req, stdout, stderr)
	if err != nil {
		return rt.Result{}, err
	}
	return rt.Result{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code,
		Duration: time.Since(start), TimedOut: timedOut,
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}, nil
}

// exec runs req, streaming output into stdout/stderr.
func (s *Sandbox) exec(ctx context.Context, req rt.ExecRequest, stdout, stderr io.Writer) (code int, timedOut bool, err error) {
	opts := container.ExecOptions{
		User:         fmt.Sprintf("%d:%d", req.User.UID, req.User.GID),
		Cmd:          req.Cmd,
		Env:          req.Env,
		WorkingDir:   req.Dir,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  req.Stdin != nil,
	}
	created, err := s.r.cli.ContainerExecCreate(ctx, s.id, opts)
	if err != nil {
		return 0, false, rt.Infra("docker exec create", err)
	}
	resp, err := s.r.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, false, rt.Infra("docker exec attach", err)
	}
	defer resp.Close()

	var wg sync.WaitGroup
	if req.Stdin != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(resp.Conn, req.Stdin)
			_ = resp.CloseWrite()
		}()
	}
	copied := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := stdcopy.StdCopy(stdout, stderr, resp.Reader)
		copied <- err
	}()
	var timeout <-chan time.Time
	if req.Timeout > 0 {
		t := time.NewTimer(req.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case cerr := <-copied:
		if cerr != nil && !errors.Is(cerr, io.EOF) {
			resp.Close()
			wg.Wait()
			return 0, false, rt.Infra("docker exec stream", cerr)
		}
	case <-timeout:
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}
	if timedOut {
		s.killExec(context.WithoutCancel(ctx), created.ID, req.User)
		resp.Close()
		wg.Wait()
		if ctx.Err() != nil && req.Timeout == 0 {
			return 0, false, rt.Infra("docker exec", ctx.Err())
		}
		return 137, true, nil
	}
	wg.Wait()
	// The stream can close a moment before the exit code is recorded.
	ictx := context.WithoutCancel(ctx)
	for i := 0; i < 100; i++ {
		insp, err := s.r.cli.ContainerExecInspect(ictx, created.ID)
		if err != nil {
			return 0, false, rt.Infra("docker exec inspect", err)
		}
		if !insp.Running {
			return insp.ExitCode, false, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, false, rt.Infra("docker exec inspect", errors.New("exec still running after stream closed"))
}

// killExec terminates a timed-out exec and all its descendants.
func (s *Sandbox) killExec(ctx context.Context, execID string, u rt.User) {
	insp, err := s.r.cli.ContainerExecInspect(ctx, execID)
	if err == nil && insp.Pid > 0 && s.hostPIDs {
		killTree(insp.Pid)
		return
	}
	if u.UID != 0 {
		_ = s.KillUser(ctx, u.UID)
	}
}

// killTree SIGKILLs pid and its descendants (host PIDs), children first
// being frozen by stopping the parent first.
func killTree(pid int) {
	_ = syscall.Kill(pid, syscall.SIGSTOP)
	for _, c := range children(pid) {
		killTree(c)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func children(pid int) []int {
	var out []int
	tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	for _, t := range tasks {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, t.Name()))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// CopyIn implements runtime.Sandbox. Extraction is done by the image's tar
// running as root inside the sandbox, so it also works on tmpfs mounts
// (which `docker cp` cannot write to).
func (s *Sandbox) CopyIn(ctx context.Context, dir string, tarStream io.Reader, owner rt.User) error {
	if err := s.alive(); err != nil {
		return err
	}
	script := `mkdir -p "$1" && tar -x -o -f - -C "$1"`
	if owner.UID != 0 {
		script += fmt.Sprintf(` && chown -R %d:%d "$1"`, owner.UID, owner.GID)
	}
	res, err := s.Exec(ctx, rt.ExecRequest{Cmd: []string{"sh", "-c", script, "sh", dir}, User: rt.Root, Stdin: tarStream, Timeout: 5 * time.Minute})
	if err != nil {
		return rt.Infra("docker copyin", err)
	}
	if res.ExitCode != 0 {
		return rt.Infra("docker copyin", fmt.Errorf("tar exit %d: %s", res.ExitCode, res.Stderr))
	}
	return nil
}

// CopyOut implements runtime.Sandbox. The stream comes from `tar -c` (which
// does not follow symlinks and stores special files as headers only), run
// as root after the caller has stopped all untrusted processes.
func (s *Sandbox) CopyOut(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	p = path.Clean(p)
	chk, err := s.Exec(ctx, rt.ExecRequest{Cmd: []string{"sh", "-c", `test -e "$1" || test -L "$1"`, "sh", p}, User: rt.Root, Timeout: 30 * time.Second})
	if err != nil {
		return nil, rt.Infra("docker copyout", err)
	}
	if chk.ExitCode != 0 {
		return nil, fmt.Errorf("docker copyout %s: %w", p, os.ErrNotExist)
	}
	pr, pw := io.Pipe()
	cctx, cancel := context.WithCancel(ctx)
	stderr := rt.NewLimitedBuffer(4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, _, err := s.exec(cctx, rt.ExecRequest{
			Cmd:  []string{"tar", "-c", "-f", "-", "-C", path.Dir(p), path.Base(p)},
			User: rt.Root,
		}, pw, stderr)
		switch {
		case err != nil:
			pw.CloseWithError(err)
		case code != 0:
			pw.CloseWithError(fmt.Errorf("docker copyout: tar exit %d: %s", code, stderr.Bytes()))
		default:
			pw.Close()
		}
	}()
	return &pipeCloser{PipeReader: pr, cancel: cancel, done: done}, nil
}

type pipeCloser struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *pipeCloser) Close() error {
	p.cancel()
	err := p.PipeReader.Close()
	<-p.done
	return err
}

// KillUser implements runtime.Sandbox.
func (s *Sandbox) KillUser(ctx context.Context, uid int) error {
	if err := s.alive(); err != nil {
		return err
	}
	if uid == 0 {
		return errors.New("docker: refusing to KillUser(0)")
	}
	for attempt := 0; attempt < 50; attempt++ {
		pids, err := s.userPIDs(ctx, uid)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		if s.hostPIDs {
			// Freeze so nothing can fork while we kill, signal by host PID,
			// then thaw so the SIGKILLs are delivered. The cgroup v1
			// freezer can fail to converge while a fork bomb is spawning
			// ("unable to freeze"); then kill unfrozen and let the loop
			// repeat. Freezing only speeds this up: the guarantee is the
			// host-side verification that no process remains.
			paused := s.r.cli.ContainerPause(ctx, s.id) == nil
			if !paused {
				s.log.Debug("freeze failed; killing without freeze", "attempt", attempt)
			}
			targets, err := s.userPIDs(ctx, uid)
			if err == nil {
				for _, p := range targets {
					_ = syscall.Kill(p, syscall.SIGKILL)
				}
			}
			if paused {
				if uerr := s.r.cli.ContainerUnpause(context.WithoutCancel(ctx), s.id); uerr != nil {
					return rt.Infra("docker unpause", uerr)
				}
			}
			if err != nil {
				return err
			}
		} else {
			// Fallback: signal from inside as the same uid. kill(-1) as a
			// non-root user reaches every process of that uid.
			_, err := s.Exec(ctx, rt.ExecRequest{Cmd: []string{"kill", "-9", "-1"}, User: rt.User{UID: uid, GID: uid}, Timeout: 10 * time.Second})
			if err != nil && !rt.IsInfra(err) {
				return err
			}
		}
		time.Sleep(time.Duration(min(attempt*attempt, 100)) * time.Millisecond)
	}
	pids, _ := s.userPIDs(ctx, uid)
	return fmt.Errorf("docker: %d processes of uid %d survived KillUser", len(pids), uid)
}

// userPIDs lists live (non-zombie) host PIDs owned by uid, read from the
// host via `docker top`, never from inside the sandbox.
func (s *Sandbox) userPIDs(ctx context.Context, uid int) ([]int, error) {
	top, err := s.r.cli.ContainerTop(ctx, s.id, []string{"-eo", "pid,uid,stat"})
	if err != nil {
		return nil, rt.Infra("docker top", err)
	}
	idx := map[string]int{}
	for i, t := range top.Titles {
		idx[strings.ToUpper(t)] = i
	}
	pi, ok1 := idx["PID"]
	ui, ok2 := idx["UID"]
	si, ok3 := idx["STAT"]
	if !ok1 || !ok2 || !ok3 {
		return nil, rt.Infra("docker top", fmt.Errorf("unexpected columns %v", top.Titles))
	}
	var out []int
	for _, row := range top.Processes {
		if len(row) <= max(pi, ui, si) || strings.HasPrefix(row[si], "Z") {
			continue
		}
		if u, err := strconv.Atoi(row[ui]); err == nil && u == uid {
			if p, err := strconv.Atoi(row[pi]); err == nil {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Stats implements runtime.Sandbox.
func (s *Sandbox) Stats(ctx context.Context) (rt.Stats, error) {
	if err := s.alive(); err != nil {
		return rt.Stats{}, err
	}
	resp, err := s.r.cli.ContainerStatsOneShot(ctx, s.id)
	if err != nil {
		return rt.Stats{}, rt.Infra("docker stats", err)
	}
	defer resp.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(bufio.NewReader(resp.Body)).Decode(&st); err != nil {
		return rt.Stats{}, rt.Infra("docker stats decode", err)
	}
	return rt.Stats{
		MemoryBytes: int64(st.MemoryStats.Usage),
		PIDs:        int64(st.PidsStats.Current),
		CPUNanos:    int64(st.CPUStats.CPUUsage.TotalUsage),
	}, nil
}

// Destroy implements runtime.Sandbox.
func (s *Sandbox) Destroy(ctx context.Context) error {
	s.mu.Lock()
	if s.destroyed {
		s.mu.Unlock()
		return nil
	}
	s.destroyed = true
	s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	err := s.r.cli.ContainerRemove(ctx, s.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return rt.Infra("docker remove", err)
	}
	return nil
}
