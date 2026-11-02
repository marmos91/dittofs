package vfs

import (
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// storeCodes is the service's error for each store error code a call can
// meet.
//
// decision: the store's ErrNotSupported and ErrDeadlock are not mapped, so
// either answers ErrUnavailable, a retry, though each is a final answer: the
// service has no error that means either, and no call meets one yet. Map
// each to its own error once the service has one and a call can meet it.
var storeCodes = map[merrs.ErrorCode]error{
	merrs.ErrNotFound:          vfs.ErrNotFound,
	merrs.ErrAccessDenied:      vfs.ErrAccess,
	merrs.ErrAuthRequired:      vfs.ErrAccess,
	merrs.ErrPermissionDenied:  vfs.ErrPerm,
	merrs.ErrPrivilegeRequired: vfs.ErrPerm,
	merrs.ErrAlreadyExists:     vfs.ErrExist,
	merrs.ErrNotEmpty:          vfs.ErrNotEmpty,
	merrs.ErrIsDirectory:       vfs.ErrIsDir,
	merrs.ErrNotDirectory:      vfs.ErrNotDir,
	merrs.ErrInvalidArgument:   vfs.ErrInvalid,
	merrs.ErrNameTooLong:       vfs.ErrNameTooLong,
	merrs.ErrNoSpace:           vfs.ErrNoSpace,
	merrs.ErrQuotaExceeded:     vfs.ErrQuota,
	merrs.ErrReadOnly:          vfs.ErrReadOnly,
	merrs.ErrInvalidHandle:     vfs.ErrStale,
	merrs.ErrStaleHandle:       vfs.ErrStale,
	merrs.ErrLocked:            vfs.ErrLocked,
	merrs.ErrLockConflict:      vfs.ErrLocked,
	merrs.ErrGracePeriod:       vfs.ErrGrace,
	merrs.ErrConflict:          &vfs.Delay{Cause: vfs.DelayConflict},
	merrs.ErrCrossShare:        &vfs.CrossError{Kind: vfs.CrossShare},
}

// storeErr is the service's error for an error a call met below it, the
// cause kept in its text. One of the service's own passes unchanged, and
// anything the service does not name is ErrUnavailable: the store failed,
// not the call.
//
// decision: the store's ErrDelay is always DelayFrozen, though the store
// also answers it for an extent a running clone still writes, which is
// DelayClone: both are the same bare sentinel, so nothing here tells them
// apart. Both are retried, so only the cause a client logs is wrong.
// Overturned by the store answering the clone's refusal with its own error.
func storeErr(err error) error {
	var se *merrs.StoreError
	switch {
	case err == nil || neutral(err):
		return err
	case errors.Is(err, entity.ErrStale), errors.Is(err, entity.ErrWrongShare):
		return fmt.Errorf("%w: %v", vfs.ErrStale, err)
	case errors.Is(err, entity.ErrDelay):
		return &vfs.Delay{Cause: vfs.DelayFrozen}
	case errors.Is(err, entity.ErrInconsistent), errors.Is(err, kvstore.ErrInconsistent):
		return fmt.Errorf("%w: %v", vfs.ErrInconsistent, err)
	case errors.As(err, &se) && storeCodes[se.Code] != nil:
		return fmt.Errorf("%w: %v", storeCodes[se.Code], err)
	}
	return fmt.Errorf("%w: %v", vfs.ErrUnavailable, err)
}

// neutral reports whether err is, or wraps, one of the service's errors.
func neutral(err error) bool {
	for _, e := range vfs.Errors() {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// openCodes is the service's error for each of open state's sentinels.
var openCodes = []struct {
	is   error
	then error
}{
	{openstate.ErrBadState, &vfs.StateError{Kind: vfs.StateBadState}},
	{openstate.ErrStaleState, &vfs.StateError{Kind: vfs.StateStaleState}},
	{openstate.ErrNotYours, &vfs.StateError{Kind: vfs.StateNotYours}},
	{openstate.ErrOldState, &vfs.StateError{Kind: vfs.StateOldState}},
	{openstate.ErrStaleClient, &vfs.StateError{Kind: vfs.StateStaleClient}},
	{openstate.ErrClientInUse, &vfs.StateError{Kind: vfs.StateClientInUse}},
	{openstate.ErrClientBusy, &vfs.StateError{Kind: vfs.StateClientBusy}},
	{openstate.ErrNoRecall, &vfs.StateError{Kind: vfs.StateNoRecall}},
	{openstate.ErrNoReclaim, &vfs.StateError{Kind: vfs.StateNoReclaim}},
	{openstate.ErrReclaimConflict, &vfs.StateError{Kind: vfs.StateReclaimConflict}},
	{openstate.ErrBadSeqID, &vfs.StateError{Kind: vfs.StateBadSeqID}},
	{openstate.ErrLocksHeld, &vfs.StateError{Kind: vfs.StateLocksHeld}},
	{openstate.ErrCreateInUse, &vfs.StateError{Kind: vfs.StateCreateInUse}},
	{openstate.ErrRecordChanged, &vfs.StateError{Kind: vfs.StateRecordChanged}},
	{openstate.ErrInvalid, vfs.ErrInvalid},
	{openstate.ErrGrace, vfs.ErrGrace},
	{openstate.ErrShareViolation, vfs.ErrShareViolation},
	{openstate.ErrDeletePending, vfs.ErrDeletePending},
	{openstate.ErrAccess, vfs.ErrAccess},
	{openstate.ErrLocked, vfs.ErrLocked},
	{openstate.ErrRangeNotLocked, vfs.ErrRangeNotLocked},
	{openstate.ErrLost, vfs.ErrLost},
	{openstate.ErrStopped, vfs.ErrUnavailable},
}

// openDelays is the service's cause for each of open state's delays.
var openDelays = map[openstate.DelayCause]vfs.DelayCause{
	openstate.DelayRecall:    vfs.DelayRecall,
	openstate.DelayResources: vfs.DelayResources,
	openstate.DelayFrozen:    vfs.DelayFrozen,
	openstate.DelayCommit:    vfs.DelayCommit,
	openstate.DelayConflict:  vfs.DelayConflict,
}

// openErr is the service's error for one open state answered; anything open
// state does not name goes on to storeErr, as open state hands the store's
// failures on.
//
// decision: ErrStopped is ErrUnavailable, the answer a draining filesystem
// gives: open state stops only after the filesystem has drained, so a call
// meeting it is one the node is shutting down under. Overturned by an answer
// of its own for it in the service's error set.
func openErr(err error) error {
	var delay *openstate.DelayError
	var revoked *openstate.RevokedError
	switch {
	case err == nil || neutral(err):
		return err
	case errors.As(err, &delay):
		return &vfs.Delay{Cause: openDelays[delay.Cause]}
	case errors.As(err, &revoked):
		return &vfs.StateError{Kind: vfs.StateRevoked, Admin: revoked.Cause == openstate.RevokedByAdmin}
	case errors.Is(err, openstate.ErrRevoked):
		return &vfs.StateError{Kind: vfs.StateRevoked}
	}
	for _, c := range openCodes {
		if errors.Is(err, c.is) {
			return c.then
		}
	}
	return storeErr(err)
}
