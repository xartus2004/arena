// Package gvisor is a placeholder for a gVisor (runsc) runtime.
//
// gVisor interposes a user-space kernel between the sandbox and the host,
// which closes most kernel attack surface that plain runc containers expose.
// The intended implementation is the Docker runtime with
// HostConfig.Runtime = "runsc"; it was not built because runsc is not
// installed on the development host. Every method returns ErrUnsupported.
package gvisor

import (
	"context"
	"fmt"

	rt "github.com/xartus2004/arena/runtime"
)

// Runtime is the gVisor stub.
type Runtime struct{}

// New returns the stub runtime.
func New() *Runtime { return &Runtime{} }

// Name implements runtime.Runtime.
func (*Runtime) Name() string { return "gvisor" }

// Capabilities implements runtime.Runtime; the stub provides nothing.
func (*Runtime) Capabilities() rt.Capabilities { return rt.Capabilities{} }

// Create implements runtime.Runtime.
func (*Runtime) Create(context.Context, rt.Spec) (rt.Sandbox, error) {
	return nil, fmt.Errorf("gvisor: %w (runsc not available; see README roadmap)", rt.ErrUnsupported)
}
