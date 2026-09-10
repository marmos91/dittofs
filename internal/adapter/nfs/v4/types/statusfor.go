package types

import (
	goerrors "errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// StatusFor translates a metadata error code to an NFSv4 status code.
// The per-adapter contract: every adapter types package exposes StatusFor
// with this same signature over its own protocol's wire codes, so a new
// metadata ErrorCode is mapped for each protocol when its row is added.
func StatusFor(code merrs.ErrorCode) uint32 {
	switch code {
	case merrs.ErrNotFound:
		return NFS4ERR_NOENT
	case merrs.ErrAccessDenied:
		return NFS4ERR_ACCESS
	case merrs.ErrAuthRequired:
		// NFSv4 has no distinct "auth required" code at the I/O layer —
		// ACCESS covers the client-actionable denial.
		return NFS4ERR_ACCESS
	case merrs.ErrPermissionDenied:
		return NFS4ERR_PERM
	case merrs.ErrAlreadyExists:
		return NFS4ERR_EXIST
	case merrs.ErrNotEmpty:
		return NFS4ERR_NOTEMPTY
	case merrs.ErrIsDirectory:
		return NFS4ERR_ISDIR
	case merrs.ErrNotDirectory:
		return NFS4ERR_NOTDIR
	case merrs.ErrInvalidArgument:
		return NFS4ERR_INVAL
	case merrs.ErrIOError:
		return NFS4ERR_IO
	case merrs.ErrNoSpace:
		return NFS4ERR_NOSPC
	case merrs.ErrQuotaExceeded:
		return NFS4ERR_DQUOT
	case merrs.ErrReadOnly:
		return NFS4ERR_ROFS
	case merrs.ErrNotSupported:
		return NFS4ERR_NOTSUPP
	case merrs.ErrInvalidHandle:
		return NFS4ERR_BADHANDLE
	case merrs.ErrStaleHandle:
		return NFS4ERR_STALE
	case merrs.ErrLocked:
		return NFS4ERR_LOCKED
	case merrs.ErrLockNotFound:
		// General-context fallback: the closest "lock range is wrong" code.
		return NFS4ERR_LOCK_RANGE
	case merrs.ErrPrivilegeRequired:
		return NFS4ERR_PERM
	case merrs.ErrNameTooLong:
		return NFS4ERR_NAMETOOLONG
	case merrs.ErrDeadlock:
		return NFS4ERR_DEADLOCK
	case merrs.ErrGracePeriod:
		return NFS4ERR_GRACE
	case merrs.ErrLockLimitExceeded:
		// Standard "lock request denied" code — too many locks held.
		return NFS4ERR_DENIED
	case merrs.ErrLockConflict:
		// Mirrors ErrLocked — surfaced during lock upgrade/downgrade.
		return NFS4ERR_DENIED
	case merrs.ErrConflict:
		// Store-transaction-level race, not client-actionable — retryable
		// delay, no client state change implied.
		return NFS4ERR_DELAY
	case merrs.ErrConnectionLimitReached:
		// Temporary server limit — retryable delay.
		return NFS4ERR_DELAY
	default:
		return NFS4ERR_SERVERFAULT
	}
}

// nfs4Default is the fallback status for errors that carry no metadata
// error code (a non-StoreError error): the generic server fault.
const nfs4Default = NFS4ERR_SERVERFAULT

// StatusForErr translates an error to an NFSv4 status code. nil maps to
// NFS4_OK; a block store closed under an in-flight op maps to the
// stale-handle status (the share went away mid-transfer); a
// *merrs.StoreError (including wrapped forms) maps through StatusFor; any
// other error falls back to the generic server fault.
func StatusForErr(err error) uint32 {
	if err == nil {
		return NFS4_OK
	}
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return StatusFor(merrs.ErrStaleHandle)
	}
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return nfs4Default
	}
	return StatusFor(storeErr.Code)
}
