// Package firecracker is a placeholder for a Firecracker microVM runtime.
//
// A microVM gives each sandbox its own guest kernel (hardware virtualisation
// via KVM), the strongest isolation arena is designed for. It needs /dev/kvm,
// which the development host does not have. Every method returns
// ErrUnsupported.
package firecracker

import (
	"context"
	"fmt"

	rt "github.com/xartus2004/arena/runtime"
)

// Runtime is the Firecracker stub.
type Runtime struct{}

// New returns the stub runtime.
func New() *Runtime { return &Runtime{} }

// Name implements runtime.Runtime.
func (*Runtime) Name() string { return "firecracker" }

// Capabilities implements runtime.Runtime; the stub provides nothing.
func (*Runtime) Capabilities() rt.Capabilities { return rt.Capabilities{} }

// Create implements runtime.Runtime.
func (*Runtime) Create(context.Context, rt.Spec) (rt.Sandbox, error) {
	return nil, fmt.Errorf("firecracker: %w (needs /dev/kvm; see README roadmap)", rt.ErrUnsupported)
}
