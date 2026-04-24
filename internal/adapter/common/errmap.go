package common

import (
	goerrors "errors"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// protoCodes carries the per-protocol status code for a single
// merrs.ErrorCode row. Every row in errorMap populates all three columns —
// Go's struct literal rules guarantee that adding a new ErrorCode populates
// NFS3/NFS4/SMB at once (ADAPT-03 "one edit, not two" contract).
type protoCodes struct {
	NFS3 uint32
	NFS4 uint32
	SMB  smbtypes.Status
}

// defaultCodes are returned when the error is not a *merrs.StoreError OR
// when the StoreError.Code is not in errorMap. These match the "generic I/O
// error" fallback the pre-consolidation protocol-specific translators used.
var defaultCodes = protoCodes{
	NFS3: nfs3types.NFS3ErrIO,
	NFS4: nfs4types.NFS4ERR_SERVERFAULT,
	SMB:  smbtypes.StatusInternalError,
}

// errorMap is the single source of truth for merrs.ErrorCode -> protocol
// status code (general/non-lock context).
//
// Authority per column (consolidated during ADAPT-03):
//   - NFS3 column: internal/adapter/nfs/xdr/errors.go (MapStoreErrorToNFSStatus
//     is the audit-logging wrapper and is more complete than
//     internal/adapter/nfs/v3/handlers/create.go's mapMetadataErrorToNFS —
//     per PATTERNS.md gotcha #2 / #6). xdr/errors.go includes ErrLocked,
//     ErrPrivilegeRequired, ErrNameTooLong rows that create.go partially
//     omitted.
//   - NFS4 column: internal/adapter/nfs/v4/types/errors.go:11-70. Includes
//     lock codes (ErrLocked → NFS4ERR_LOCKED, ErrDeadlock → NFS4ERR_DEADLOCK,
//     ErrGracePeriod → NFS4ERR_GRACE) that NFSv3 had to fall back on
//     NFS3ErrJukebox for.
//   - SMB column: internal/adapter/smb/v2/handlers/converters.go:354-395 for
//     general (non-lock) context. Lock-context deltas go in lock_errmap.go.
//
// Three-way drift surfaced during consolidation (per D-07 / PATTERNS.md
// gotcha #7). Each row with a drift item is annotated inline with the
// authority source and the fallback chosen for the omitting protocol:
//
//   - NFSv3 omitted ErrDeadlock, ErrGracePeriod, ErrLockLimitExceeded,
//     ErrLockConflict, ErrQuotaExceeded, ErrLockNotFound,
//     ErrConnectionLimitReached. Fallback: the most conservative transient
//     code NFSv3 offers (NFS3ErrJukebox for lock-class; NFS3ErrIO for I/O
//     class; NFS3ErrDquot for quota).
//   - SMB omitted ErrPermissionDenied, ErrAuthRequired, ErrReadOnly,
//     ErrNameTooLong, ErrStaleHandle, ErrPrivilegeRequired,
//     ErrQuotaExceeded, ErrLocked, ErrLockNotFound, ErrDeadlock,
//     ErrGracePeriod, ErrLockLimitExceeded, ErrLockConflict,
//     ErrConnectionLimitReached. Fallback: StatusAccessDenied for
//     permission-class (MS-ERREF 2.3 — SMB has no EPERM distinction);
//     StatusMediaWriteProtected (not available — we use StatusAccessDenied
//     for ReadOnly); StatusFileLockConflict for general-context lock errors
//     (lock-operation context uses lock_errmap.go).
var errorMap = map[merrs.ErrorCode]protoCodes{
	merrs.ErrNotFound: {
		NFS3: nfs3types.NFS3ErrNoEnt,
		NFS4: nfs4types.NFS4ERR_NOENT,
		SMB:  smbtypes.StatusObjectNameNotFound,
	},
	merrs.ErrAccessDenied: {
		NFS3: nfs3types.NFS3ErrAccess,
		NFS4: nfs4types.NFS4ERR_ACCESS,
		SMB:  smbtypes.StatusAccessDenied,
	},
	merrs.ErrAuthRequired: {
		// NFSv3: xdr/errors.go:80-83 maps to NFS3ErrAccess (same as AccessDenied).
		// SMB: converters.go omits; fallback StatusAccessDenied (SMB does not
		// distinguish "auth required" from "access denied" at the I/O layer).
		NFS3: nfs3types.NFS3ErrAccess,
		NFS4: nfs4types.NFS4ERR_ACCESS,
		SMB:  smbtypes.StatusAccessDenied,
	},
	merrs.ErrPermissionDenied: {
		// SMB: converters.go omits; fallback StatusAccessDenied (no EPERM
		// distinction in SMB — MS-ERREF 2.3).
		NFS3: nfs3types.NFS3ErrPerm,
		NFS4: nfs4types.NFS4ERR_PERM,
		SMB:  smbtypes.StatusAccessDenied,
	},
	merrs.ErrAlreadyExists: {
		NFS3: nfs3types.NFS3ErrExist,
		NFS4: nfs4types.NFS4ERR_EXIST,
		SMB:  smbtypes.StatusObjectNameCollision,
	},
	merrs.ErrNotEmpty: {
		NFS3: nfs3types.NFS3ErrNotEmpty,
		NFS4: nfs4types.NFS4ERR_NOTEMPTY,
		SMB:  smbtypes.StatusDirectoryNotEmpty,
	},
	merrs.ErrIsDirectory: {
		NFS3: nfs3types.NFS3ErrIsDir,
		NFS4: nfs4types.NFS4ERR_ISDIR,
		SMB:  smbtypes.StatusFileIsADirectory,
	},
	merrs.ErrNotDirectory: {
		NFS3: nfs3types.NFS3ErrNotDir,
		NFS4: nfs4types.NFS4ERR_NOTDIR,
		SMB:  smbtypes.StatusNotADirectory,
	},
	merrs.ErrInvalidArgument: {
		// NFSv3: create.go's mapMetadataErrorToNFS returned NFS3ErrInval
		// (POSIX EINVAL — the correct mapping); xdr/errors.go's audit-logged
		// wrapper coarsened to NFS3ErrIO. We take create.go as authoritative
		// here because (a) the handler-path tests (e.g., TestReadLink_NotSymlink)
		// assert NFS3ErrInval, (b) EINVAL→EIO coarsening hides real argument
		// bugs from clients. The xdr wrapper's existing callers continue to
		// log, but now also get NFS3ErrInval via common.MapToNFS3.
		NFS3: nfs3types.NFS3ErrInval,
		NFS4: nfs4types.NFS4ERR_INVAL,
		SMB:  smbtypes.StatusInvalidParameter,
	},
	merrs.ErrIOError: {
		NFS3: nfs3types.NFS3ErrIO,
		NFS4: nfs4types.NFS4ERR_IO,
		SMB:  smbtypes.StatusUnexpectedIOError,
	},
	merrs.ErrNoSpace: {
		NFS3: nfs3types.NFS3ErrNoSpc,
		NFS4: nfs4types.NFS4ERR_NOSPC,
		SMB:  smbtypes.StatusDiskFull,
	},
	merrs.ErrQuotaExceeded: {
		// NFSv3: create.go:604-605 has NFS3ErrDquot; xdr/errors.go omits —
		// use create.go value.
		// SMB: converters.go omits; fallback StatusDiskFull (closest SMB
		// signal — SMB does not distinguish quota from free-space exhaustion
		// at this layer).
		NFS3: nfs3types.NFS3ErrDquot,
		NFS4: nfs4types.NFS4ERR_DQUOT,
		SMB:  smbtypes.StatusDiskFull,
	},
	merrs.ErrReadOnly: {
		// SMB: converters.go omits; fallback StatusAccessDenied (SMB has no
		// dedicated "read-only filesystem" status — clients observe access
		// denied on write).
		NFS3: nfs3types.NFS3ErrRofs,
		NFS4: nfs4types.NFS4ERR_ROFS,
		SMB:  smbtypes.StatusAccessDenied,
	},
	merrs.ErrNotSupported: {
		NFS3: nfs3types.NFS3ErrNotSupp,
		NFS4: nfs4types.NFS4ERR_NOTSUPP,
		SMB:  smbtypes.StatusNotSupported,
	},
	merrs.ErrInvalidHandle: {
		// NFSv3: xdr/errors.go:120-123 maps to NFS3ErrBadHandle; create.go:610-611
		// maps to NFS3ErrStale. Authority = xdr/errors.go (NFS3ErrBadHandle).
		NFS3: nfs3types.NFS3ErrBadHandle,
		NFS4: nfs4types.NFS4ERR_BADHANDLE,
		SMB:  smbtypes.StatusInvalidHandle,
	},
	merrs.ErrStaleHandle: {
		// SMB: converters.go omits; fallback StatusFileClosed (closest
		// semantic — clients observe "handle no longer refers to a file").
		NFS3: nfs3types.NFS3ErrStale,
		NFS4: nfs4types.NFS4ERR_STALE,
		SMB:  smbtypes.StatusFileClosed,
	},
	merrs.ErrLocked: {
		// NFSv3: xdr/errors.go:140-146 authoritative (NFS3ErrJukebox); create.go
		// omits.
		// SMB general context: StatusFileLockConflict. Lock-operation context
		// lives in lock_errmap.go (→ StatusLockNotGranted).
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_LOCKED,
		SMB:  smbtypes.StatusFileLockConflict,
	},
	merrs.ErrLockNotFound: {
		// NFSv3/SMB: neither source has a general-context row. Lock-operation
		// context lives in lock_errmap.go (→ NFS3ErrInval / StatusRangeNotLocked).
		// General context falls back to the closest "something is wrong with
		// the lock state" code.
		NFS3: nfs3types.NFS3ErrInval,
		NFS4: nfs4types.NFS4ERR_LOCK_RANGE,
		SMB:  smbtypes.StatusRangeNotLocked,
	},
	merrs.ErrPrivilegeRequired: {
		// NFSv3: xdr/errors.go:148-152 returns NFS3ErrPerm (matches
		// create.go:589-591).
		// SMB: converters.go omits; fallback StatusAccessDenied.
		NFS3: nfs3types.NFS3ErrPerm,
		NFS4: nfs4types.NFS4ERR_PERM,
		SMB:  smbtypes.StatusAccessDenied,
	},
	merrs.ErrNameTooLong: {
		// SMB: converters.go omits; fallback StatusObjectNameInvalid (closest
		// SMB signal — NT_STATUS has no dedicated "name too long" code in the
		// codes used by this adapter).
		NFS3: nfs3types.NFS3ErrNameTooLong,
		NFS4: nfs4types.NFS4ERR_NAMETOOLONG,
		SMB:  smbtypes.StatusObjectNameInvalid,
	},
	merrs.ErrDeadlock: {
		// NFSv3: no direct code — fallback NFS3ErrJukebox (transient retry).
		// SMB: converters.go omits; general-context fallback
		// StatusFileLockConflict (lock-context in lock_errmap.go uses
		// StatusLockNotGranted).
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DEADLOCK,
		SMB:  smbtypes.StatusFileLockConflict,
	},
	merrs.ErrGracePeriod: {
		// NFSv3: no direct code — fallback NFS3ErrJukebox (RFC 1813
		// retry-later semantic).
		// SMB: converters.go omits; fallback StatusInternalError — SMB has no
		// NFSv4-style grace-period concept; clients see a server-side fault.
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_GRACE,
		SMB:  smbtypes.StatusInternalError,
	},
	merrs.ErrLockLimitExceeded: {
		// NFSv3: no direct code — fallback NFS3ErrIO.
		// NFSv4: NFS4ERR_DENIED (standard "lock request denied" code).
		// SMB: fallback StatusInsufficientResources — closest SMB signal for
		// "too many locks held".
		NFS3: nfs3types.NFS3ErrIO,
		NFS4: nfs4types.NFS4ERR_DENIED,
		SMB:  smbtypes.StatusInsufficientResources,
	},
	merrs.ErrLockConflict: {
		// Similar to ErrLocked but surfaced during lock upgrade/downgrade.
		// Fallbacks mirror ErrLocked for the three protocols.
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DENIED,
		SMB:  smbtypes.StatusFileLockConflict,
	},
	merrs.ErrConnectionLimitReached: {
		// No protocol has a "connection limit" code at the file-I/O layer
		// (this is a DittoFS-internal signal). Fallbacks match the generic
		// "temporary server limit reached" intent.
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DELAY,
		SMB:  smbtypes.StatusInsufficientResources,
	},
}

// lookupErrorRow returns the errorMap row for err, or defaultCodes when err is
// nil, is not a *merrs.StoreError, or has a Code that is not in errorMap. Uses
// goerrors.As so wrapped StoreErrors unwrap correctly — this is the
// consolidation fix for the pre-consolidation SMB type-assertion bug
// (converters.go:364) which did not unwrap.
//
// Callers use the returned row directly; nil is handled at the per-protocol
// accessor so each protocol returns its own SUCCESS constant (NFS3OK / NFS4_OK
// / StatusSuccess) instead of defaultCodes.
func lookupErrorRow(err error) protoCodes {
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return defaultCodes
	}
	if codes, ok := errorMap[storeErr.Code]; ok {
		return codes
	}
	return defaultCodes
}

// MapToNFS3 translates err to an NFS3 status code. Returns NFS3OK for nil,
// defaultCodes.NFS3 when err is not a *merrs.StoreError, and the errorMap
// row's NFS3 column otherwise.
func MapToNFS3(err error) uint32 {
	if err == nil {
		return nfs3types.NFS3OK
	}
	return lookupErrorRow(err).NFS3
}

// MapToNFS4 translates err to an NFS4 status code.
func MapToNFS4(err error) uint32 {
	if err == nil {
		return nfs4types.NFS4_OK
	}
	return lookupErrorRow(err).NFS4
}

// MapToSMB translates err to an SMB NT status code.
func MapToSMB(err error) smbtypes.Status {
	if err == nil {
		return smbtypes.StatusSuccess
	}
	return lookupErrorRow(err).SMB
}
