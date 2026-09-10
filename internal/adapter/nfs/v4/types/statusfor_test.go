package types

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// expectedNFS4 is the full per-protocol expectation table: one entry per
// merrs.ErrorCode, so a new code without a row fails this walk loudly
// instead of silently falling to the server-fault default.
var expectedNFS4 = map[merrs.ErrorCode]uint32{
	merrs.ErrNotFound:               NFS4ERR_NOENT,
	merrs.ErrAccessDenied:           NFS4ERR_ACCESS,
	merrs.ErrAuthRequired:           NFS4ERR_ACCESS,
	merrs.ErrPermissionDenied:       NFS4ERR_PERM,
	merrs.ErrAlreadyExists:          NFS4ERR_EXIST,
	merrs.ErrNotEmpty:               NFS4ERR_NOTEMPTY,
	merrs.ErrIsDirectory:            NFS4ERR_ISDIR,
	merrs.ErrNotDirectory:           NFS4ERR_NOTDIR,
	merrs.ErrInvalidArgument:        NFS4ERR_INVAL,
	merrs.ErrIOError:                NFS4ERR_IO,
	merrs.ErrNoSpace:                NFS4ERR_NOSPC,
	merrs.ErrQuotaExceeded:          NFS4ERR_DQUOT,
	merrs.ErrReadOnly:               NFS4ERR_ROFS,
	merrs.ErrNotSupported:           NFS4ERR_NOTSUPP,
	merrs.ErrInvalidHandle:          NFS4ERR_BADHANDLE,
	merrs.ErrStaleHandle:            NFS4ERR_STALE,
	merrs.ErrLocked:                 NFS4ERR_LOCKED,
	merrs.ErrLockNotFound:           NFS4ERR_LOCK_RANGE,
	merrs.ErrPrivilegeRequired:      NFS4ERR_PERM,
	merrs.ErrNameTooLong:            NFS4ERR_NAMETOOLONG,
	merrs.ErrDeadlock:               NFS4ERR_DEADLOCK,
	merrs.ErrGracePeriod:            NFS4ERR_GRACE,
	merrs.ErrLockLimitExceeded:      NFS4ERR_DENIED,
	merrs.ErrLockConflict:           NFS4ERR_DENIED,
	merrs.ErrConflict:               NFS4ERR_DELAY,
	merrs.ErrConnectionLimitReached: NFS4ERR_DELAY,
}

// TestStatusFor_EnumWalk walks every merrs ErrorCode against the full
// expectation table. Drift is loud: an unmapped code returns the
// server-fault default and fails here.
func TestStatusFor_EnumWalk(t *testing.T) {
	for c := merrs.ErrNotFound; c <= merrs.ErrConflict; c++ {
		want, ok := expectedNFS4[c]
		if !ok {
			t.Errorf("StatusFor(%v) has no expectation row — add one to expectedNFS4", c)
			continue
		}
		if got := StatusFor(c); got != want {
			t.Errorf("StatusFor(%v) = %d, want %d", c, got, want)
		}
	}
}

// TestStatusFor_NilDefaultsAndUnwrap pins the error-level wrapper semantics:
// nil → NFS4_OK, a non-StoreError error → the server-fault default, a
// wrapped StoreError unwraps, and the block-store-closed sentinel lifts to
// the stale row.
func TestStatusFor_NilDefaultsAndUnwrap(t *testing.T) {
	if got := StatusForErr(nil); got != NFS4_OK {
		t.Errorf("StatusForErr(nil) = %d, want NFS4_OK (%d)", got, NFS4_OK)
	}
	if got := StatusForErr(fmt.Errorf("boom")); got != NFS4ERR_SERVERFAULT {
		t.Errorf("StatusForErr(non-StoreError) = %d, want NFS4ERR_SERVERFAULT (%d)", got, NFS4ERR_SERVERFAULT)
	}
	wrapped := fmt.Errorf("op failed: %w", &merrs.StoreError{Code: merrs.ErrNoSpace})
	if got := StatusForErr(wrapped); got != NFS4ERR_NOSPC {
		t.Errorf("StatusForErr(wrapped ErrNoSpace) = %d, want NFS4ERR_NOSPC (%d)", got, NFS4ERR_NOSPC)
	}
	if got := StatusForErr(engine.ErrStoreClosed); got != NFS4ERR_STALE {
		t.Errorf("StatusForErr(ErrStoreClosed) = %d, want NFS4ERR_STALE (%d)", got, NFS4ERR_STALE)
	}
}
