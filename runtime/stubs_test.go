package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	rt "github.com/xartus2004/arena/runtime"
	"github.com/xartus2004/arena/runtime/firecracker"
	"github.com/xartus2004/arena/runtime/gvisor"
)

func TestStubsReturnUnsupported(t *testing.T) {
	for _, r := range []rt.Runtime{gvisor.New(), firecracker.New()} {
		t.Run(r.Name(), func(t *testing.T) {
			_, err := r.Create(context.Background(), rt.Spec{Image: "x", Timeout: time.Second})
			if !errors.Is(err, rt.ErrUnsupported) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
