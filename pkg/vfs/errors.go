package vfs

import "github.com/marmos91/dittofs/pkg/vfs/vfserr"

// The service's errors, in no protocol's terms, declared in vfserr so the
// engine returns them without importing this package: each adapter maps
// every one of them, as Errors lists them, to its own.
var (
	ErrNotFound    = vfserr.ErrNotFound
	ErrExist       = vfserr.ErrExist
	ErrNotEmpty    = vfserr.ErrNotEmpty
	ErrNotDir      = vfserr.ErrNotDir
	ErrIsDir       = vfserr.ErrIsDir
	ErrAccess      = vfserr.ErrAccess
	ErrPerm        = vfserr.ErrPerm
	ErrStale       = vfserr.ErrStale
	ErrReadOnly    = vfserr.ErrReadOnly
	ErrNameTooLong = vfserr.ErrNameTooLong
	ErrInvalid     = vfserr.ErrInvalid
	ErrNoSpace     = vfserr.ErrNoSpace
	ErrQuota       = vfserr.ErrQuota

	ErrLocked         = vfserr.ErrLocked
	ErrRangeNotLocked = vfserr.ErrRangeNotLocked
	ErrShareViolation = vfserr.ErrShareViolation
	ErrDeletePending  = vfserr.ErrDeletePending
	ErrGrace          = vfserr.ErrGrace
	ErrDelay          = vfserr.ErrDelay
	ErrBlocked        = vfserr.ErrBlocked
	ErrWrongSecurity  = vfserr.ErrWrongSecurity
	ErrState          = vfserr.ErrState
	ErrCross          = vfserr.ErrCross
	ErrIncomplete     = vfserr.ErrIncomplete
	ErrInconsistent   = vfserr.ErrInconsistent

	ErrLost        = vfserr.ErrLost
	ErrUnavailable = vfserr.ErrUnavailable
	ErrCorrupt     = vfserr.ErrCorrupt
)

// The typed errors and their kinds, and the write verifier and copy extent
// the engine shares with the service.
type (
	AccessError = vfserr.AccessError
	AccessCause = vfserr.AccessCause
	QuotaError  = vfserr.QuotaError
	QuotaScope  = vfserr.QuotaScope
	Delay       = vfserr.Delay
	DelayCause  = vfserr.DelayCause
	StateError  = vfserr.StateError
	StateKind   = vfserr.StateKind
	CrossError  = vfserr.CrossError
	CrossKind   = vfserr.CrossKind
	Verifier    = vfserr.Verifier
	Extent      = vfserr.Extent
)

const AccessReadOnly = vfserr.AccessReadOnly

const (
	QuotaShare   = vfserr.QuotaShare
	QuotaProject = vfserr.QuotaProject
	QuotaUser    = vfserr.QuotaUser
	QuotaGroup   = vfserr.QuotaGroup
)

const (
	DelayRecall    = vfserr.DelayRecall
	DelayClone     = vfserr.DelayClone
	DelayConflict  = vfserr.DelayConflict
	DelayFrozen    = vfserr.DelayFrozen
	DelayCapacity  = vfserr.DelayCapacity
	DelayCommit    = vfserr.DelayCommit
	DelayResources = vfserr.DelayResources
)

const (
	StateClientInUse     = vfserr.StateClientInUse
	StateClientBusy      = vfserr.StateClientBusy
	StateRecordChanged   = vfserr.StateRecordChanged
	StateAdapterVersion  = vfserr.StateAdapterVersion
	StateStaleClient     = vfserr.StateStaleClient
	StateStaleState      = vfserr.StateStaleState
	StateOldState        = vfserr.StateOldState
	StateBadState        = vfserr.StateBadState
	StateNoReclaim       = vfserr.StateNoReclaim
	StateReclaimConflict = vfserr.StateReclaimConflict
	StateLocksHeld       = vfserr.StateLocksHeld
	StateBadSeqID        = vfserr.StateBadSeqID
	StateNoRecall        = vfserr.StateNoRecall
	StateCreateInUse     = vfserr.StateCreateInUse
	StateNotYours        = vfserr.StateNotYours
	StateRevoked         = vfserr.StateRevoked
)

const (
	CrossNamespace = vfserr.CrossNamespace
	CrossShare     = vfserr.CrossShare
	CrossProject   = vfserr.CrossProject
	OtherShard     = vfserr.OtherShard
)

// Errors is every sentinel, and one value of every kind of each typed error,
// so an adapter's error table can be checked total.
func Errors() []error {
	errs := []error{
		ErrNotFound, ErrExist, ErrNotEmpty, ErrNotDir, ErrIsDir, ErrAccess, ErrPerm, ErrStale,
		ErrReadOnly, ErrNameTooLong, ErrInvalid, ErrNoSpace, ErrQuota, ErrLocked, ErrRangeNotLocked,
		ErrShareViolation, ErrDeletePending, ErrGrace, ErrDelay, ErrBlocked, ErrWrongSecurity, ErrState,
		ErrCross, ErrIncomplete, ErrInconsistent, ErrLost, ErrUnavailable, ErrCorrupt,
	}
	for c := AccessCause(1); c.Valid(); c++ {
		errs = append(errs, &AccessError{Cause: c})
	}
	for s := QuotaScope(1); s.Valid(); s++ {
		errs = append(errs, &QuotaError{Scope: s})
	}
	for c := DelayCause(1); c.Valid(); c++ {
		errs = append(errs, &Delay{Cause: c})
	}
	errs = append(errs, &Delay{Cause: DelayFrozen, Moved: true})
	for k := StateKind(1); k.Valid(); k++ {
		errs = append(errs, &StateError{Kind: k})
	}
	errs = append(errs, &StateError{Kind: StateRevoked, Admin: true})
	for k := CrossKind(1); k.Valid(); k++ {
		errs = append(errs, &CrossError{Kind: k})
	}
	return errs
}
