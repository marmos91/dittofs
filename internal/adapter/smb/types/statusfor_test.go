package types

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// expectedSMB is the full per-protocol general-context expectation table:
// one entry per merrs.ErrorCode, so a new code without a row fails this
// walk loudly instead of silently falling to the internal-error default.
var expectedSMB = map[merrs.ErrorCode]Status{
	merrs.ErrNotFound:               StatusObjectNameNotFound,
	merrs.ErrAccessDenied:           StatusAccessDenied,
	merrs.ErrAuthRequired:           StatusAccessDenied,
	merrs.ErrPermissionDenied:       StatusAccessDenied,
	merrs.ErrAlreadyExists:          StatusObjectNameCollision,
	merrs.ErrNotEmpty:               StatusDirectoryNotEmpty,
	merrs.ErrIsDirectory:            StatusFileIsADirectory,
	merrs.ErrNotDirectory:           StatusNotADirectory,
	merrs.ErrInvalidArgument:        StatusInvalidParameter,
	merrs.ErrIOError:                StatusUnexpectedIOError,
	merrs.ErrNoSpace:                StatusDiskFull,
	merrs.ErrQuotaExceeded:          StatusQuotaExceeded,
	merrs.ErrReadOnly:               StatusAccessDenied,
	merrs.ErrNotSupported:           StatusNotSupported,
	merrs.ErrInvalidHandle:          StatusInvalidHandle,
	merrs.ErrStaleHandle:            StatusFileClosed,
	merrs.ErrLocked:                 StatusFileLockConflict,
	merrs.ErrLockNotFound:           StatusRangeNotLocked,
	merrs.ErrPrivilegeRequired:      StatusPrivilegeNotHeld,
	merrs.ErrNameTooLong:            StatusObjectNameInvalid,
	merrs.ErrDeadlock:               StatusFileLockConflict,
	merrs.ErrGracePeriod:            StatusInternalError,
	merrs.ErrLockLimitExceeded:      StatusInsufficientResources,
	merrs.ErrLockConflict:           StatusFileLockConflict,
	merrs.ErrConflict:               StatusInsufficientResources,
	merrs.ErrCrossShare:             StatusNotSameDevice,
	merrs.ErrConnectionLimitReached: StatusInsufficientResources,
}

// expectedSMBLock is the LOCK-context expectation table. Per MS-SMB2
// 3.3.5.14 a LOCK denial surfaces STATUS_LOCK_NOT_GRANTED while the I/O
// paths surface STATUS_FILE_LOCK_CONFLICT — the lock-vs-general divergence
// this split exists for.
var expectedSMBLock = map[merrs.ErrorCode]Status{
	merrs.ErrLocked:            StatusLockNotGranted,
	merrs.ErrLockNotFound:      StatusRangeNotLocked,
	merrs.ErrLockConflict:      StatusLockNotGranted,
	merrs.ErrDeadlock:          StatusLockNotGranted,
	merrs.ErrGracePeriod:       StatusInternalError,
	merrs.ErrLockLimitExceeded: StatusInsufficientResources,
	merrs.ErrNotFound:          StatusFileClosed,
	merrs.ErrPermissionDenied:  StatusAccessDenied,
	merrs.ErrIsDirectory:       StatusFileIsADirectory,
}

// TestStatusFor_EnumWalk walks every merrs ErrorCode against both context
// tables. Drift is loud: an unmapped code returns the internal-error
// default and fails here.
func TestStatusFor_EnumWalk(t *testing.T) {
	for c := merrs.ErrNotFound; c <= merrs.ErrCrossShare; c++ {
		want, ok := expectedSMB[c]
		if !ok {
			t.Errorf("StatusFor(%v) has no expectation row — add one to expectedSMB", c)
			continue
		}
		if got := StatusFor(c); got != want {
			t.Errorf("StatusFor(%v) = %d, want %d", c, got, want)
		}

		if wantLock, ok := expectedSMBLock[c]; ok {
			if got := StatusForLock(c); got != wantLock {
				t.Errorf("StatusForLock(%v) = %d, want %d", c, got, wantLock)
			}
		}
	}
}

// TestStatusFor_LockVsGeneralDivergence pins the lock-vs-general split the
// LOCK paths depend on: the same metadata code answers differently by
// operation context.
func TestStatusFor_LockVsGeneralDivergence(t *testing.T) {
	if got, want := StatusFor(merrs.ErrLocked), StatusForLock(merrs.ErrLocked); got == want {
		t.Errorf("ErrLocked lock/general contexts must diverge (both %d)", got)
	}
	if got, want := StatusFor(merrs.ErrDeadlock), StatusForLock(merrs.ErrDeadlock); got == want {
		t.Errorf("ErrDeadlock lock/general contexts must diverge (both %d)", got)
	}
}

// TestStatusFor_NilDefaultsAndUnwrap pins the error-level wrapper semantics:
// nil → success, a non-StoreError error → the internal-error default, a
// wrapped StoreError unwraps, and the block-store-closed sentinel lifts to
// the closed-file row.
func TestStatusFor_NilDefaultsAndUnwrap(t *testing.T) {
	if got := StatusForErr(nil); got != StatusSuccess {
		t.Errorf("StatusForErr(nil) = %d, want StatusSuccess", got)
	}
	if got := StatusForErr(fmt.Errorf("boom")); got != StatusInternalError {
		t.Errorf("StatusForErr(non-StoreError) = %d, want StatusInternalError", got)
	}
	wrapped := fmt.Errorf("op failed: %w", &merrs.StoreError{Code: merrs.ErrNoSpace})
	if got := StatusForErr(wrapped); got != StatusDiskFull {
		t.Errorf("StatusForErr(wrapped ErrNoSpace) = %d, want StatusDiskFull", got)
	}
	if got := StatusForErr(engine.ErrStoreClosed); got != StatusFileClosed {
		t.Errorf("StatusForErr(ErrStoreClosed) = %d, want StatusFileClosed", got)
	}
	if got := StatusForLockErr(nil); got != StatusSuccess {
		t.Errorf("StatusForLockErr(nil) = %d, want StatusSuccess", got)
	}
	if got := StatusForLockErr(fmt.Errorf("boom")); got != StatusInternalError {
		t.Errorf("StatusForLockErr(non-StoreError) = %d, want StatusInternalError", got)
	}
	if got := StatusForLockErr(engine.ErrStoreClosed); got != StatusFileClosed {
		t.Errorf("StatusForLockErr(ErrStoreClosed) = %d, want StatusFileClosed", got)
	}
}
