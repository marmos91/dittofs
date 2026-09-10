package common

import (
	goerrors "errors"

	smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// Lock-context mapping for the SMB LOCK path.
//
// In a LOCK request the same merrs.ErrorCode values map to different SMB
// codes than in the general I/O path:
//
//   - merrs.ErrLocked in LOCK context  →  STATUS_LOCK_NOT_GRANTED.
//   - merrs.ErrLocked in general (READ/WRITE) context  →
//     STATUS_FILE_LOCK_CONFLICT — see errorMap in errmap.go.
//
// The NFSv3/NFSv4 lock answers live in errorMap (errmap.go); its lock-class
// rows already carry the retry-class codes the lock path needs
// (ErrLocked → NFS4ERR_LOCKED/NFS3ErrJukebox, ErrDeadlock →
// NFS4ERR_DEADLOCK, ErrLockNotFound → NFS4ERR_LOCK_RANGE). This table holds
// only the deltas where the SMB lock answer diverges from the SMB general
// answer.
//
// Source: internal/adapter/smb/handlers/lock.go (lockErrorToStatus —
// authoritative).
var lockErrorMap = map[merrs.ErrorCode]protoCodes{
	merrs.ErrLocked: {
		SMB: smbtypes.StatusLockNotGranted,
	},
	merrs.ErrLockNotFound: {
		SMB: smbtypes.StatusRangeNotLocked,
	},
	merrs.ErrLockConflict: {
		SMB: smbtypes.StatusLockNotGranted,
	},
	merrs.ErrDeadlock: {
		// No direct SMB code — StatusLockNotGranted (closest retry
		// semantic in LOCK context).
		SMB: smbtypes.StatusLockNotGranted,
	},
	merrs.ErrGracePeriod: {
		// SMB has no NFSv4-style grace-period concept; clients see a
		// server-side fault.
		SMB: smbtypes.StatusInternalError,
	},
	merrs.ErrLockLimitExceeded: {
		SMB: smbtypes.StatusInsufficientResources,
	},
	// Lock-context overrides for general errors that SMB's lockErrorToStatus
	// historically handled (lock.go:540-545). These differ from errorMap in
	// the SMB column only, so callers that fall through to general context
	// get consistent behavior.
	merrs.ErrNotFound: {
		SMB: smbtypes.StatusFileClosed,
	},
	merrs.ErrPermissionDenied: {
		SMB: smbtypes.StatusAccessDenied,
	},
	merrs.ErrIsDirectory: {
		SMB: smbtypes.StatusFileIsADirectory,
	},
}

// lookupLockErrorRow resolves err via the fallback chain
// lockErrorMap → errorMap (general) → defaultCodes. Callers handle the nil
// case separately so each protocol can return its own SUCCESS constant.
func lookupLockErrorRow(err error) protoCodes {
	// A store removed mid-lock-operation is not a *merrs.StoreError; map it
	// to the same stale-handle row the general and content mappers use so
	// the client observes the share going away rather than a server fault.
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return errorMap[merrs.ErrStaleHandle]
	}
	var storeErr *merrs.StoreError
	if !goerrors.As(err, &storeErr) {
		return defaultCodes
	}
	if lock, ok := lockErrorMap[storeErr.Code]; ok {
		// Start from the general row and overlay the lock-context SMB
		// delta, so the NFS columns stay populated for any future
		// lock-context caller.
		codes := errorMap[storeErr.Code]
		codes.SMB = lock.SMB
		return codes
	}
	if codes, ok := errorMap[storeErr.Code]; ok {
		return codes
	}
	return defaultCodes
}

// MapLockToSMB translates a lock-operation error to an SMB status code.
func MapLockToSMB(err error) smbtypes.Status {
	if err == nil {
		return smbtypes.StatusSuccess
	}
	return lookupLockErrorRow(err).SMB
}
