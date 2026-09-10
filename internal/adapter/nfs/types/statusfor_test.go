package types

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// expectedNFS3 is the full per-protocol expectation table: one entry per
// merrs.ErrorCode, so a new code without a row fails this walk loudly
// instead of silently falling to the I/O default.
var expectedNFS3 = map[merrs.ErrorCode]uint32{
	merrs.ErrNotFound:               NFS3ErrNoEnt,
	merrs.ErrAccessDenied:           NFS3ErrAccess,
	merrs.ErrAuthRequired:           NFS3ErrAccess,
	merrs.ErrPermissionDenied:       NFS3ErrPerm,
	merrs.ErrAlreadyExists:          NFS3ErrExist,
	merrs.ErrNotEmpty:               NFS3ErrNotEmpty,
	merrs.ErrIsDirectory:            NFS3ErrIsDir,
	merrs.ErrNotDirectory:           NFS3ErrNotDir,
	merrs.ErrInvalidArgument:        NFS3ErrInval,
	merrs.ErrIOError:                NFS3ErrIO,
	merrs.ErrNoSpace:                NFS3ErrNoSpc,
	merrs.ErrQuotaExceeded:          NFS3ErrDquot,
	merrs.ErrReadOnly:               NFS3ErrRofs,
	merrs.ErrNotSupported:           NFS3ErrNotSupp,
	merrs.ErrInvalidHandle:          NFS3ErrBadHandle,
	merrs.ErrStaleHandle:            NFS3ErrStale,
	merrs.ErrLocked:                 NFS3ErrJukebox,
	merrs.ErrLockNotFound:           NFS3ErrInval,
	merrs.ErrPrivilegeRequired:      NFS3ErrPerm,
	merrs.ErrNameTooLong:            NFS3ErrNameTooLong,
	merrs.ErrDeadlock:               NFS3ErrJukebox,
	merrs.ErrGracePeriod:            NFS3ErrJukebox,
	merrs.ErrLockLimitExceeded:      NFS3ErrJukebox,
	merrs.ErrLockConflict:           NFS3ErrJukebox,
	merrs.ErrConflict:               NFS3ErrJukebox,
	merrs.ErrConnectionLimitReached: NFS3ErrJukebox,
}

// TestStatusFor_EnumWalk walks every merrs ErrorCode against the full
// expectation table. Drift is loud: an unmapped code returns the I/O
// default and fails here.
func TestStatusFor_EnumWalk(t *testing.T) {
	for c := merrs.ErrNotFound; c <= merrs.ErrConflict; c++ {
		want, ok := expectedNFS3[c]
		if !ok {
			t.Errorf("StatusFor(%v) has no expectation row — add one to expectedNFS3", c)
			continue
		}
		if got := StatusFor(c); got != want {
			t.Errorf("StatusFor(%v) = %d, want %d", c, got, want)
		}
	}
}

// TestStatusFor_NilDefaultsAndUnwrap pins the error-level wrapper semantics:
// nil → success, a non-StoreError error → the I/O default, a wrapped
// StoreError unwraps, and the block-store-closed sentinel lifts to the
// stale row.
func TestStatusFor_NilDefaultsAndUnwrap(t *testing.T) {
	if got := StatusForErr(nil); got != NFS3OK {
		t.Errorf("StatusForErr(nil) = %d, want NFS3OK (%d)", got, NFS3OK)
	}
	if got := StatusForErr(fmt.Errorf("boom")); got != NFS3ErrIO {
		t.Errorf("StatusForErr(non-StoreError) = %d, want NFS3ErrIO (%d)", got, NFS3ErrIO)
	}
	wrapped := fmt.Errorf("op failed: %w", &merrs.StoreError{Code: merrs.ErrNoSpace})
	if got := StatusForErr(wrapped); got != NFS3ErrNoSpc {
		t.Errorf("StatusForErr(wrapped ErrNoSpace) = %d, want NFS3ErrNoSpc (%d)", got, NFS3ErrNoSpc)
	}
	if got := StatusForErr(engine.ErrStoreClosed); got != NFS3ErrStale {
		t.Errorf("StatusForErr(ErrStoreClosed) = %d, want NFS3ErrStale (%d)", got, NFS3ErrStale)
	}
}
