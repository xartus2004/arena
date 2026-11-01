// Package runtime defines the sandbox abstraction that every isolation
// backend (Docker, fake, and future gVisor/Firecracker) implements.
//
// A Runtime creates Sandboxes from a Spec. A Sandbox is single-use: it is
// provisioned, used by exactly one episode phase (student turn or grading),
// and destroyed. Sandboxes are never recycled between episodes.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrUnsupported is returned by runtimes (or runtime features) that are not
// available in the current build or on the current host.
var ErrUnsupported = errors.New("runtime: unsupported")

// ErrDestroyed is returned when operating on a sandbox that has been destroyed.
var ErrDestroyed = errors.New("runtime: sandbox destroyed")

// Well-known users inside every sandbox. Numeric IDs are used so that images
// do not need matching /etc/passwd entries.
var (
	// Root is the sandbox administrator. Setup steps, custody and grading
	// harnesses run as Root. It is never handed to the agent.
	Root = User{Name: "root", UID: 0, GID: 0}
	// Student is the unprivileged user the agent's tools run as.
	Student = User{Name: "student", UID: 1000, GID: 1000}
	// Service is an unprivileged user for in-sandbox services (e.g. the
	// mini-crm HTTP server) whose state the student must not touch directly.
	Service = User{Name: "service", UID: 1001, GID: 1001}
	// Untrusted is the user graders run submitted code as inside the grader
	// sandbox, so submitted code can never read hidden files.
	Untrusted = User{Name: "untrusted", UID: 2000, GID: 2000}
)

// User identifies a numeric user/group inside a sandbox.
type User struct {
	Name string `json:"name" yaml:"name"`
	UID  int    `json:"uid" yaml:"uid"`
	GID  int    `json:"gid" yaml:"gid"`
}

// String implements fmt.Stringer.
func (u User) String() string { return fmt.Sprintf("%s(%d:%d)", u.Name, u.UID, u.GID) }

// NetworkMode selects a sandbox network policy.
type NetworkMode string

const (
	// NetworkNone gives the sandbox only a loopback interface.
	NetworkNone NetworkMode = "none"
	// NetworkAllowlist permits egress only to the listed hosts.
	NetworkAllowlist NetworkMode = "allowlist"
)

// NetworkPolicy describes what the sandbox may reach.
type NetworkPolicy struct {
	Mode  NetworkMode `json:"mode" yaml:"mode"`
	Allow []string    `json:"allow,omitempty" yaml:"allow,omitempty"`
}

// Spec describes the sandbox to create.
type Spec struct {
	// Image is the container image (Docker) or rootfs path (VM runtimes).
	Image string `json:"image"`
	// CPUs is the CPU quota in cores (e.g. 0.5, 1, 2). 0 means runtime default.
	CPUs float64 `json:"cpus"`
	// MemoryBytes is the hard memory limit, including tmpfs pages.
	MemoryBytes int64 `json:"memory_bytes"`
	// MaxPIDs bounds the number of processes/threads (fork-bomb protection).
	MaxPIDs int64 `json:"max_pids"`
	// DiskBytes bounds the writable workspace.
	DiskBytes int64 `json:"disk_bytes"`
	// MaxInodes bounds the number of files in the writable workspace.
	// 0 means runtime default.
	MaxInodes int64 `json:"max_inodes"`
	// Network is the egress policy.
	Network NetworkPolicy `json:"network"`
	// Timeout is the maximum wall-clock lifetime of the sandbox, measured on
	// the host. The runtime destroys the sandbox when it expires.
	Timeout time.Duration `json:"timeout"`
	// Workdirs are writable directories created in the sandbox (beyond /tmp).
	// Each is backed by the disk quota.
	Workdirs []string `json:"workdirs"`
	// Labels are attached to the sandbox for bookkeeping/cleanup.
	Labels map[string]string `json:"labels,omitempty"`
}

// Key returns a stable string identifying sandboxes that are interchangeable
// (used by warm pools). Labels are excluded.
func (s Spec) Key() string {
	return fmt.Sprintf("%s|cpu=%g|mem=%d|pids=%d|disk=%d|inodes=%d|net=%s%v|t=%s|wd=%v",
		s.Image, s.CPUs, s.MemoryBytes, s.MaxPIDs, s.DiskBytes, s.MaxInodes,
		s.Network.Mode, s.Network.Allow, s.Timeout, s.Workdirs)
}

// Validate checks the spec for obviously broken values.
func (s Spec) Validate() error {
	var errs []error
	if s.Image == "" {
		errs = append(errs, errors.New("image is required"))
	}
	if s.CPUs < 0 {
		errs = append(errs, errors.New("cpus must be >= 0"))
	}
	if s.MemoryBytes < 0 || s.MaxPIDs < 0 || s.DiskBytes < 0 || s.MaxInodes < 0 {
		errs = append(errs, errors.New("limits must be >= 0"))
	}
	if s.Timeout <= 0 {
		errs = append(errs, errors.New("timeout must be > 0"))
	}
	switch s.Network.Mode {
	case NetworkNone, "":
	case NetworkAllowlist:
		if len(s.Network.Allow) == 0 {
			errs = append(errs, errors.New("allowlist network needs at least one host"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown network mode %q", s.Network.Mode))
	}
	return errors.Join(errs...)
}

// ExecRequest is a command to run inside a sandbox.
type ExecRequest struct {
	// Cmd is the argv; it is not interpreted by a shell.
	Cmd []string
	// User runs the command.
	User User
	// Stdin, if non-nil, is streamed to the process.
	Stdin io.Reader
	// Env holds extra KEY=VALUE pairs.
	Env []string
	// Dir is the working directory ("" = runtime default).
	Dir string
	// Timeout bounds the command's wall-clock time (host-measured).
	// 0 means only the context deadline applies.
	Timeout time.Duration
	// MaxOutput caps captured bytes per stream; extra output is discarded and
	// Result.Truncated is set. 0 means DefaultMaxOutput.
	MaxOutput int
}

// DefaultMaxOutput caps captured stdout/stderr per stream.
const DefaultMaxOutput = 1 << 20

// Result is the outcome of an Exec.
type Result struct {
	Stdout    []byte        `json:"stdout"`
	Stderr    []byte        `json:"stderr"`
	ExitCode  int           `json:"exit_code"`
	Duration  time.Duration `json:"duration"`
	TimedOut  bool          `json:"timed_out"`
	Truncated bool          `json:"truncated"`
}

// Stats is a point-in-time resource snapshot.
type Stats struct {
	MemoryBytes int64 `json:"memory_bytes"`
	PIDs        int64 `json:"pids"`
	CPUNanos    int64 `json:"cpu_nanos"`
}

// Sandbox is a single isolated execution environment.
type Sandbox interface {
	// ID is a runtime-unique identifier.
	ID() string
	// Exec runs a command and waits for it. A non-zero exit status is not an
	// error; errors indicate infrastructure problems.
	Exec(ctx context.Context, req ExecRequest) (Result, error)
	// CopyIn extracts a tar stream into dir (which is created if missing).
	// Entries are owned by owner.
	CopyIn(ctx context.Context, dir string, tarStream io.Reader, owner User) error
	// CopyOut returns a tar stream of path. The stream is produced without
	// following symlinks and without opening special files.
	CopyOut(ctx context.Context, path string) (io.ReadCloser, error)
	// KillUser kills every process owned by uid and verifies none remain.
	KillUser(ctx context.Context, uid int) error
	// Stats reports current resource usage.
	Stats(ctx context.Context) (Stats, error)
	// Destroy releases all resources. It is idempotent.
	Destroy(ctx context.Context) error
}

// Runtime creates sandboxes.
type Runtime interface {
	// Name identifies the runtime ("docker", "fake", ...).
	Name() string
	// Create provisions a new, started sandbox.
	Create(ctx context.Context, spec Spec) (Sandbox, error)
	// Capabilities reports which isolation guarantees this runtime provides,
	// so tests and the red-team report can be honest about them.
	Capabilities() Capabilities
}

// Capabilities lists the isolation guarantees a runtime actually enforces.
type Capabilities struct {
	ProcessIsolation bool // separate PID namespace / process tree
	FSIsolation      bool // separate root filesystem
	NetworkIsolation bool // NetworkNone is enforced
	MemoryLimit      bool
	PIDLimit         bool
	DiskQuota        bool
	ClockIsolation   bool // sandbox cannot change the host clock
	NoPrivEscalation bool // setuid/caps cannot lift the student to root
	RealExecution    bool // commands are actually executed
}

// InfraError marks a failure of the platform rather than of the agent.
// Episodes that fail with an InfraError may be retried.
type InfraError struct {
	Op  string
	Err error
}

func (e *InfraError) Error() string { return fmt.Sprintf("infra: %s: %v", e.Op, e.Err) }
func (e *InfraError) Unwrap() error { return e.Err }

// Infra wraps err as an InfraError (nil stays nil).
func Infra(op string, err error) error {
	if err == nil {
		return nil
	}
	var ie *InfraError
	if errors.As(err, &ie) {
		return err
	}
	return &InfraError{Op: op, Err: err}
}

// IsInfra reports whether err is (or wraps) an InfraError.
func IsInfra(err error) bool {
	var ie *InfraError
	return errors.As(err, &ie)
}
