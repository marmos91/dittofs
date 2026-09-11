package types

import (
	goerrors "errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// StatusFor translates a metadata error code to an SMB NT status code for
// the general (non-LOCK) I/O context. The per-adapter contract: every
// adapter types package exposes StatusFor with this same signature over its
// own protocol's wire codes, so a new metadata ErrorCode is mapped for each
// protocol when its row is added.
//
// SMB needs a second lock-context mapper (StatusForLock) because per
// MS-SMB2 3.3.5.14 a LOCK denial surfaces STATUS_LOCK_NOT_GRANTED while an
// I/O sharing violation surfaces STATUS_FILE_LOCK_CONFLICT — the same
// metadata lock errors legitimately have two answers depending on the
// operation that produced them.
func StatusFor(code merrs.ErrorCode) Status {
	switch code {
	case merrs.ErrNotFound:
		return StatusObjectNameNotFound
	case merrs.ErrAccessDenied:
		return StatusAccessDenied
	case merrs.ErrAuthRequired:
		// SMB has no distinct "auth required" code at the I/O layer —
		// clients observe access denied.
		return StatusAccessDenied
	case merrs.ErrPermissionDenied:
		// No EPERM distinction in SMB (MS-ERREF 2.3) — access denied.
		return StatusAccessDenied
	case merrs.ErrAlreadyExists:
		return StatusObjectNameCollision
	case merrs.ErrNotEmpty:
		return StatusDirectoryNotEmpty
	case merrs.ErrIsDirectory:
		return StatusFileIsADirectory
	case merrs.ErrNotDirectory:
		return StatusNotADirectory
	case merrs.ErrInvalidArgument:
		return StatusInvalidParameter
	case merrs.ErrCrossShare:
		return StatusNotSameDevice
	case merrs.ErrIOError:
		return StatusUnexpectedIOError
	case merrs.ErrNoSpace:
		return StatusDiskFull
	case merrs.ErrQuotaExceeded:
		// Distinct from StatusDiskFull so Windows clients tell a full volume
		// from a per-user/per-group quota breach.
		return StatusQuotaExceeded
	case merrs.ErrReadOnly:
		// No dedicated "read-only filesystem" status in the codes this
		// adapter uses — clients observe access denied on write.
		return StatusAccessDenied
	case merrs.ErrNotSupported:
		return StatusNotSupported
	case merrs.ErrInvalidHandle:
		return StatusInvalidHandle
	case merrs.ErrStaleHandle:
		// Clients observe "handle no longer refers to a file".
		return StatusFileClosed
	case merrs.ErrLocked:
		// General (READ/WRITE) context: an I/O operation blocked by a range
		// lock is a sharing violation, not a LOCK denial.
		return StatusFileLockConflict
	case merrs.ErrLockNotFound:
		return StatusRangeNotLocked
	case merrs.ErrPrivilegeRequired:
		// Required by smbtorture smb2.maximum_allowed.maximum_allowed when
		// the request asks for SEC_FLAG_SYSTEM_SECURITY without
		// SeSecurityPrivilege.
		return StatusPrivilegeNotHeld
	case merrs.ErrNameTooLong:
		// No dedicated "name too long" code in the codes this adapter uses —
		// the closest malformed-name signal.
		return StatusObjectNameInvalid
	case merrs.ErrDeadlock:
		// General-context fallback: I/O blocked by lock state.
		return StatusFileLockConflict
	case merrs.ErrGracePeriod:
		// SMB has no NFSv4-style grace-period concept — server-side fault.
		return StatusInternalError
	case merrs.ErrLockLimitExceeded:
		// Closest signal for "too many locks held".
		return StatusInsufficientResources
	case merrs.ErrLockConflict:
		// Mirrors ErrLocked in general context — I/O sharing violation.
		return StatusFileLockConflict
	case merrs.ErrConflict:
		// Store-transaction-level race, not client-actionable — transient
		// server-side condition.
		return StatusInsufficientResources
	case merrs.ErrConnectionLimitReached:
		// Temporary server limit.
		return StatusInsufficientResources
	default:
		return StatusInternalError
	}
}

// StatusForLock translates a metadata error code to an SMB NT status code
// for the LOCK-operation context. Per MS-SMB2 3.3.5.14, a LOCK denial
// surfaces STATUS_LOCK_NOT_GRANTED (STATUS_FILE_LOCK_CONFLICT is reserved
// for the I/O paths), so the lock-class codes and the general errors the
// lock path historically handled diverge from StatusFor here. The NFS
// answers for the same codes need no split (NFS4ERR_DENIED in both
// contexts; NFSv3 has no lock procedure), which is why only SMB carries
// this second mapper.
func StatusForLock(code merrs.ErrorCode) Status {
	switch code {
	case merrs.ErrLocked:
		return StatusLockNotGranted
	case merrs.ErrLockNotFound:
		return StatusRangeNotLocked
	case merrs.ErrLockConflict:
		return StatusLockNotGranted
	case merrs.ErrDeadlock:
		// No direct SMB code — the closest retry semantic in LOCK context.
		return StatusLockNotGranted
	case merrs.ErrGracePeriod:
		// SMB has no grace-period concept — server-side fault (same as
		// general context).
		return StatusInternalError
	case merrs.ErrLockLimitExceeded:
		// Same as general context — "too many locks held".
		return StatusInsufficientResources
	// Lock-context overrides for general errors the lock path historically
	// handled: these differ from StatusFor so callers stay consistent with
	// the operation they dispatched.
	case merrs.ErrNotFound:
		return StatusFileClosed
	case merrs.ErrPermissionDenied:
		return StatusAccessDenied
	case merrs.ErrIsDirectory:
		return StatusFileIsADirectory
	default:
		return StatusFor(code)
	}
}

// smbDefault is the fallback status for errors that carry no metadata
// error code (a non-StoreError error): the generic internal error.
const smbDefault = StatusInternalError

// StatusForErr translates an error to an SMB NT status code in the general
// (non-LOCK) context. nil maps to StatusSuccess; a block store closed under
// an in-flight op maps to the closed-file status (the share went away
// mid-transfer); a *merrs.StoreError (including wrapped forms) maps through
// StatusFor; any other error falls back to StatusInternalError.
func StatusForErr(err error) Status {
	if err == nil {
		return StatusSuccess
	}
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return StatusFor(merrs.ErrStaleHandle)
	}
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return smbDefault
	}
	return StatusFor(storeErr.Code)
}

// StatusForLockErr is the LOCK-context twin of StatusForErr: nil maps to
// StatusSuccess, a closed block store to the closed-file status, a
// *merrs.StoreError through StatusForLock, anything else to the lock
// context's default (the generic internal error — the same fallback both
// contexts use).
func StatusForLockErr(err error) Status {
	if err == nil {
		return StatusSuccess
	}
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return StatusForLock(merrs.ErrStaleHandle)
	}
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return smbDefault
	}
	return StatusForLock(storeErr.Code)
}
