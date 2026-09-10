package types

import (
	goerrors "errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// StatusFor translates a metadata error code to an NFSv3 status code.
// The per-adapter contract: every adapter types package exposes StatusFor
// with this same signature over its own protocol's wire codes, so a new
// metadata ErrorCode is mapped for each protocol when its row is added.
// Returns NFS3OK for the zero value is not meaningful — callers pass
// errors, not codes, for success handling (see StatusForErr).
func StatusFor(code merrs.ErrorCode) uint32 {
	switch code {
	case merrs.ErrNotFound:
		return NFS3ErrNoEnt
	case merrs.ErrAccessDenied:
		return NFS3ErrAccess
	case merrs.ErrAuthRequired:
		// Same as ErrAccessDenied — NFSv3 has no distinct "auth required"
		// code at the I/O layer.
		return NFS3ErrAccess
	case merrs.ErrPermissionDenied:
		return NFS3ErrPerm
	case merrs.ErrAlreadyExists:
		return NFS3ErrExist
	case merrs.ErrNotEmpty:
		return NFS3ErrNotEmpty
	case merrs.ErrIsDirectory:
		return NFS3ErrIsDir
	case merrs.ErrNotDirectory:
		return NFS3ErrNotDir
	case merrs.ErrInvalidArgument:
		return NFS3ErrInval
	case merrs.ErrIOError:
		return NFS3ErrIO
	case merrs.ErrNoSpace:
		return NFS3ErrNoSpc
	case merrs.ErrQuotaExceeded:
		return NFS3ErrDquot
	case merrs.ErrReadOnly:
		return NFS3ErrRofs
	case merrs.ErrNotSupported:
		return NFS3ErrNotSupp
	case merrs.ErrInvalidHandle:
		return NFS3ErrBadHandle
	case merrs.ErrStaleHandle:
		return NFS3ErrStale
	case merrs.ErrLocked:
		// Transient retry-later — NFSv3 has no direct "locked" code (the
		// NLM-side share denial surfaces through the same retry class).
		return NFS3ErrJukebox
	case merrs.ErrLockNotFound:
		// General-context fallback: the closest "lock state is wrong" code.
		// NFSv3 has no lock procedure (only NSM), so no direct code exists.
		return NFS3ErrInval
	case merrs.ErrPrivilegeRequired:
		return NFS3ErrPerm
	case merrs.ErrNameTooLong:
		return NFS3ErrNameTooLong
	case merrs.ErrDeadlock:
		// No direct code — transient retry class.
		return NFS3ErrJukebox
	case merrs.ErrGracePeriod:
		// No direct code — RFC 1813 retry-later semantic.
		return NFS3ErrJukebox
	case merrs.ErrLockLimitExceeded:
		// No direct code — transient retry class matching the lock-context
		// table's intent for lock limits.
		return NFS3ErrJukebox
	case merrs.ErrLockConflict:
		// Mirrors ErrLocked — surfaced during lock upgrade/downgrade.
		return NFS3ErrJukebox
	case merrs.ErrConflict:
		// Store-transaction-level race, not client-actionable — retry later.
		return NFS3ErrJukebox
	case merrs.ErrConnectionLimitReached:
		// Temporary server limit — retry class.
		return NFS3ErrJukebox
	default:
		return NFS3ErrIO
	}
}

// nfs3Default is the fallback status for errors that carry no metadata
// error code (a non-StoreError error): the generic I/O failure.
const nfs3Default = NFS3ErrIO

// StatusForErr translates an error to an NFSv3 status code. nil maps to
// NFS3OK; a block store closed under an in-flight op maps to the
// stale-handle status (the share went away mid-transfer); a
// *merrs.StoreError (including wrapped forms) maps through StatusFor; any
// other error falls back to the generic I/O status.
func StatusForErr(err error) uint32 {
	if err == nil {
		return NFS3OK
	}
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return StatusFor(merrs.ErrStaleHandle)
	}
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return nfs3Default
	}
	return StatusFor(storeErr.Code)
}
