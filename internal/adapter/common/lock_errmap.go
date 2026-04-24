package common

import (
	goerrors "errors"

	nfs3types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// Lock-context mapping (D-08 §3).
//
// In a LOCK request (SMB2 LOCK, NLM/NFSv3 NLM_LOCK, NFSv4 LOCK/LOCKU) the
// same merrs.ErrorCode values map to different protocol codes than in the
// general I/O path:
//
//   - merrs.ErrLocked in LOCK context  →  STATUS_LOCK_NOT_GRANTED (SMB) /
//     NFS3ERR_JUKEBOX / NFS4ERR_DENIED.
//   - merrs.ErrLocked in general (READ/WRITE) context  →
//     STATUS_FILE_LOCK_CONFLICT (SMB) — see errorMap in errmap.go.
//
// Sources (consolidated during ADAPT-03):
//   - SMB column: internal/adapter/smb/v2/handlers/lock.go:532-549
//     (lockErrorToStatus — authoritative).
//   - NFS3 column: internal/adapter/nfs/xdr/errors.go:140-146 (ErrLocked
//     only; other lock-context codes did not have NFSv3 entries and fall
//     back to the closest retry-class code).
//   - NFS4 column: internal/adapter/nfs/v4/types/errors.go:59-64 for
//     ErrLocked/Deadlock/Grace, extended here with ErrLockNotFound →
//     NFS4ERR_LOCK_RANGE and ErrLockConflict → NFS4ERR_DENIED per RFC 7530.
var lockErrorMap = map[merrs.ErrorCode]protoCodes{
	merrs.ErrLocked: {
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DENIED,
		SMB:  smbtypes.StatusLockNotGranted,
	},
	merrs.ErrLockNotFound: {
		NFS3: nfs3types.NFS3ErrInval,
		NFS4: nfs4types.NFS4ERR_LOCK_RANGE,
		SMB:  smbtypes.StatusRangeNotLocked,
	},
	merrs.ErrLockConflict: {
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DENIED,
		SMB:  smbtypes.StatusLockNotGranted,
	},
	merrs.ErrDeadlock: {
		// SMB: no direct code — StatusLockNotGranted (closest retry
		// semantic in LOCK context).
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DEADLOCK,
		SMB:  smbtypes.StatusLockNotGranted,
	},
	merrs.ErrGracePeriod: {
		// NFSv3 has no dedicated grace-period code — use JUKEBOX (retry
		// later, matches the NFSv4 semantic).
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_GRACE,
		SMB:  smbtypes.StatusInternalError,
	},
	merrs.ErrLockLimitExceeded: {
		NFS3: nfs3types.NFS3ErrJukebox,
		NFS4: nfs4types.NFS4ERR_DENIED,
		SMB:  smbtypes.StatusInsufficientResources,
	},
	// Lock-context overrides for general errors that SMB's lockErrorToStatus
	// historically handled (lock.go:540-545). These differ from errorMap in
	// the SMB column only — NFS3/NFS4 match errorMap, so callers that fall
	// through to general context get consistent behavior.
	merrs.ErrNotFound: {
		NFS3: nfs3types.NFS3ErrNoEnt,
		NFS4: nfs4types.NFS4ERR_NOENT,
		SMB:  smbtypes.StatusFileClosed,
	},
	merrs.ErrPermissionDenied: {
		NFS3: nfs3types.NFS3ErrPerm,
		NFS4: nfs4types.NFS4ERR_PERM,
		SMB:  smbtypes.StatusAccessDenied,
	},
	merrs.ErrIsDirectory: {
		NFS3: nfs3types.NFS3ErrIsDir,
		NFS4: nfs4types.NFS4ERR_ISDIR,
		SMB:  smbtypes.StatusFileIsADirectory,
	},
}

// lookupLockErrorRow resolves err via the fallback chain
// lockErrorMap → errorMap (general) → defaultCodes. Callers handle the nil
// case separately so each protocol can return its own SUCCESS constant.
func lookupLockErrorRow(err error) protoCodes {
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return defaultCodes
	}
	if codes, ok := lockErrorMap[storeErr.Code]; ok {
		return codes
	}
	if codes, ok := errorMap[storeErr.Code]; ok {
		return codes
	}
	return defaultCodes
}

// MapLockToNFS3 translates a lock-operation error to an NFS3 status code.
// Fallback chain: lockErrorMap → errorMap (general) → defaultCodes.
func MapLockToNFS3(err error) uint32 {
	if err == nil {
		return nfs3types.NFS3OK
	}
	return lookupLockErrorRow(err).NFS3
}

// MapLockToNFS4 translates a lock-operation error to an NFS4 status code.
func MapLockToNFS4(err error) uint32 {
	if err == nil {
		return nfs4types.NFS4_OK
	}
	return lookupLockErrorRow(err).NFS4
}

// MapLockToSMB translates a lock-operation error to an SMB status code.
func MapLockToSMB(err error) smbtypes.Status {
	if err == nil {
		return smbtypes.StatusSuccess
	}
	return lookupLockErrorRow(err).SMB
}
