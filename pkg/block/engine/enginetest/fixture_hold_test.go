package enginetest

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs/vfserr"
)

// Open state's answers to a clone's hold cross in the engine's types: a
// shard not yet served and a stopped table are a frozen delay, and any other
// error is itself.
func TestTheHoldsErrorsCrossInTheEnginesTypes(t *testing.T) {
	for _, err := range []error{&openstate.DelayError{Cause: openstate.DelayFrozen}, openstate.ErrStopped} {
		var d *vfserr.Delay
		if got := holdErr(err); !errors.As(got, &d) || d.Cause != vfserr.DelayFrozen {
			t.Errorf("holdErr(%v) = %v, want ErrDelay(DelayFrozen)", err, got)
		}
	}
	if got := holdErr(openstate.ErrBadState); !errors.Is(got, openstate.ErrBadState) {
		t.Errorf("holdErr(ErrBadState) = %v, want it as itself", got)
	}
}
