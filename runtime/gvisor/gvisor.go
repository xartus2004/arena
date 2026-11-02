// Package gvisor provides a gVisor runtime: the Docker runtime with the
// "runsc" OCI runtime, so every sandbox runs on gVisor's user-space kernel
// (the Sentry) instead of talking to the host kernel directly.
//
// It needs runsc installed and registered with dockerd, e.g. in
// /etc/docker/daemon.json:
//
//	{"runtimes": {"runsc": {"path": "/usr/local/bin/runsc"}}}
//
// Differences from runc that arena relies on: processes inside a gVisor
// sandbox are not host processes, so custody never signals host PIDs and
// reads the process table from the sandbox kernel instead (see
// runtime/docker). All other guarantees use the same Docker settings.
package gvisor

import (
	"context"
	"log/slog"

	"github.com/xartus2004/arena/runtime/docker"
)

// OCIRuntime is the name runsc is registered under in dockerd.
const OCIRuntime = "runsc"

// New connects to Docker and checks that runsc is registered. When it is
// not, the error wraps runtime.ErrUnsupported.
func New(ctx context.Context, log *slog.Logger) (*docker.Runtime, error) {
	return docker.New(ctx, docker.Options{PullMissing: true, Logger: log, OCIRuntime: OCIRuntime})
}
