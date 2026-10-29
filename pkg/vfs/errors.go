package vfs

import (
	"errors"
	"time"
)

// The service's errors, in no protocol's terms: each adapter maps every one
// of them, as Errors lists them, to its own.
var (
	ErrNotFound    = errors.New("vfs: no such entry")
	ErrExist       = errors.New("vfs: entry exists")
	ErrNotEmpty    = errors.New("vfs: directory not empty")
	ErrNotDir      = errors.New("vfs: not a directory")
	ErrIsDir       = errors.New("vfs: is a directory")
	ErrAccess      = errors.New("vfs: access denied") // as *AccessError when the read-only marker refused a delete
	ErrPerm        = errors.New("vfs: not permitted whatever the caller's access")
	ErrStale       = errors.New("vfs: handle names nothing")
	ErrReadOnly    = errors.New("vfs: mutation where only reading is admitted")
	ErrNameTooLong = errors.New("vfs: name too long")
	ErrInvalid     = errors.New("vfs: arguments cannot be honoured as given")
	ErrNoSpace     = errors.New("vfs: no space without operator action")
	ErrQuota       = errors.New("vfs: quota exceeded") // as *QuotaError, naming the scope

	ErrLocked         = errors.New("vfs: range locked by another")
	ErrRangeNotLocked = errors.New("vfs: unlock of a range the owner does not hold")
	ErrShareViolation = errors.New("vfs: deny mode conflict")
	ErrDeletePending  = errors.New("vfs: file is delete pending")
	ErrGrace          = errors.New("vfs: in grace period")
	ErrDelay          = errors.New("vfs: retry shortly") // as *Delay, naming the cause
	ErrBlocked        = errors.New("vfs: lock waits, granted through LockWaitOver")
	ErrWrongSecurity  = errors.New("vfs: flavour not admitted by the share")
	ErrState          = errors.New("vfs: state cannot be used as given")     // as *StateError, naming the kind
	ErrCross          = errors.New("vfs: call spans what cannot be spanned") // as *CrossError, naming the boundary
	ErrIncomplete     = errors.New("vfs: file incomplete in its snapshot")
	ErrInconsistent   = errors.New("vfs: store invariant broken")

	// ponytail: the content errors are declared here, as no errors package
	// shared with the engine exists yet. Move them to that leaf package and
	// alias them here, so the engine returns them without importing this
	// one, once the engine declares its errors.
	ErrLost        = errors.New("vfs: acknowledged writes lost")
	ErrUnavailable = errors.New("vfs: unavailable")
	ErrCorrupt     = errors.New("vfs: content corrupt")
)

// AccessError is an ErrAccess with its cause.
type AccessError struct{ Cause AccessCause }

// AccessCause is why access was refused, where the adapter tells one apart.
type AccessCause uint8

const (
	AccessReadOnly AccessCause = iota + 1 // the read-only marker refused a delete

	accessCauseEnd
)

var accessCauses = [...]string{AccessReadOnly: "read-only"}

func (e *AccessError) Error() string {
	return ErrAccess.Error() + ": " + kindName(accessCauses[:], e.Cause)
}
func (e *AccessError) Is(target error) bool { return target == ErrAccess }

// QuotaError is an ErrQuota naming the first scope exceeded.
type QuotaError struct{ Scope QuotaScope }

func (e *QuotaError) Error() string {
	return ErrQuota.Error() + ": " + kindName(quotaScopes[:], e.Scope)
}
func (e *QuotaError) Is(target error) bool { return target == ErrQuota }

// Delay is an ErrDelay. Its Cause selects how long an adapter holds the
// request and its final status.
type Delay struct {
	Cause DelayCause
	Until time.Time // DelayFrozen: the freeze's bound; zero otherwise
	Moved bool      // DelayFrozen: the share's namespace moved away
}

// DelayCause is why a call is to be retried. Its values are not open
// state's DelayCause values, so one is mapped to the other, never converted.
type DelayCause uint8

const (
	DelayRecall    DelayCause = iota + 1 // a caching grant is being recalled
	DelayClone                           // a running clone or copy chunk holds the extent past the call's hold
	DelayConflict                        // a metadata conflict still unresolved at the call's deadline
	DelayFrozen                          // the share is quiesced, frozen for a move, or moved away
	DelayCapacity                        // the journal's capacity, or a share's journal limit
	DelayCommit                          // a metadata commit is not landing
	DelayResources                       // a node memory pool is spent

	delayCauseEnd
)

var delayCauses = [...]string{
	DelayRecall: "recall", DelayClone: "clone", DelayConflict: "conflict", DelayFrozen: "frozen",
	DelayCapacity: "capacity", DelayCommit: "commit", DelayResources: "resources",
}

func (e *Delay) Error() string {
	if e.Moved {
		return ErrDelay.Error() + ": " + kindName(delayCauses[:], e.Cause) + ", moved away"
	}
	return ErrDelay.Error() + ": " + kindName(delayCauses[:], e.Cause)
}

func (e *Delay) Is(target error) bool { return target == ErrDelay }

// StateError is an ErrState naming why: a client-held name of state cannot
// be used as given. Admin tells, for StateRevoked, an administrator's revoke
// from a recall missed.
type StateError struct {
	Kind  StateKind
	Admin bool
}

// StateKind is why a name of state was refused.
type StateKind uint8

const (
	StateClientInUse     StateKind = iota + 1 // a client name another principal holds
	StateClientBusy                           // Destroy with state left
	StateRecordChanged                        // the record's adapter bytes changed since read: reread and decide again
	StateAdapterVersion                       // adapter bytes of a version this node cannot read
	StateStaleClient                          // a client that expired, or must propose and confirm again after a shard's loss
	StateStaleState                           // state that may have been lost with its shard's open state
	StateOldState                             // a stateid seqid older than the state's
	StateBadState                             // a reference whose record is missing: expired, freed, a removed or moved share's, never issued
	StateNoReclaim                            // a reclaim in a shard the client's record does not list
	StateReclaimConflict                      // a reclaim conflicting with one already reclaimed in this grace
	StateLocksHeld                            // an owner released, or a lock state freed, still holding locks
	StateBadSeqID                             // an owner sequence out of order
	StateNoRecall                             // an acknowledgement of a grant with no recall in flight
	StateCreateInUse                          // an SMB create naming a create ID another open of its client holds
	StateNotYours                             // state another client or principal holds
	StateRevoked                              // state in its client's revoked set, not yet freed

	stateKindEnd
)

var stateKinds = [...]string{
	StateClientInUse: "client in use", StateClientBusy: "client busy", StateRecordChanged: "record changed",
	StateAdapterVersion: "adapter version", StateStaleClient: "stale client", StateStaleState: "stale state",
	StateOldState: "old state", StateBadState: "bad state", StateNoReclaim: "no reclaim",
	StateReclaimConflict: "reclaim conflict", StateLocksHeld: "locks held", StateBadSeqID: "bad seqid",
	StateNoRecall: "no recall", StateCreateInUse: "create in use", StateNotYours: "not yours", StateRevoked: "revoked",
}

func (e *StateError) Error() string {
	if e.Admin {
		return ErrState.Error() + ": " + kindName(stateKinds[:], e.Kind) + " by an administrator"
	}
	return ErrState.Error() + ": " + kindName(stateKinds[:], e.Kind)
}

func (e *StateError) Is(target error) bool { return target == ErrState }

// CrossError is an ErrCross naming the boundary.
type CrossError struct{ Kind CrossKind }

// CrossKind is the boundary a call spans. The first three are boundaries a
// client can see; OtherShard is placement, which it cannot, so an adapter
// falls back rather than report it.
type CrossKind uint8

const (
	CrossNamespace CrossKind = iota + 1 // a copy or clone between namespaces
	CrossShare                          // a rename or link between shares
	CrossProject                        // a rename or link across tree quotas
	OtherShard                          // a clone whose source and destination lie in different shards

	crossKindEnd
)

var crossKinds = [...]string{CrossNamespace: "namespace", CrossShare: "share", CrossProject: "project", OtherShard: "other shard"}

func (e *CrossError) Error() string        { return ErrCross.Error() + ": " + kindName(crossKinds[:], e.Kind) }
func (e *CrossError) Is(target error) bool { return target == ErrCross }

// Errors is every sentinel, and one value of every kind of each typed error,
// so an adapter's error table can be checked total.
func Errors() []error {
	errs := []error{
		ErrNotFound, ErrExist, ErrNotEmpty, ErrNotDir, ErrIsDir, ErrAccess, ErrPerm, ErrStale,
		ErrReadOnly, ErrNameTooLong, ErrInvalid, ErrNoSpace, ErrQuota, ErrLocked, ErrRangeNotLocked,
		ErrShareViolation, ErrDeletePending, ErrGrace, ErrDelay, ErrBlocked, ErrWrongSecurity, ErrState,
		ErrCross, ErrIncomplete, ErrInconsistent, ErrLost, ErrUnavailable, ErrCorrupt,
	}
	for c := AccessCause(1); c < accessCauseEnd; c++ {
		errs = append(errs, &AccessError{Cause: c})
	}
	for s := QuotaScope(1); s < quotaScopeEnd; s++ {
		errs = append(errs, &QuotaError{Scope: s})
	}
	for c := DelayCause(1); c < delayCauseEnd; c++ {
		errs = append(errs, &Delay{Cause: c})
	}
	errs = append(errs, &Delay{Cause: DelayFrozen, Moved: true})
	for k := StateKind(1); k < stateKindEnd; k++ {
		errs = append(errs, &StateError{Kind: k})
	}
	errs = append(errs, &StateError{Kind: StateRevoked, Admin: true})
	for k := CrossKind(1); k < crossKindEnd; k++ {
		errs = append(errs, &CrossError{Kind: k})
	}
	return errs
}

// kindName is the name of kind k, or "unknown" for a kind no name is
// declared for, the zero value of a kind numbered from one included.
func kindName[K ~uint8](names []string, k K) string {
	if int(k) < len(names) && names[k] != "" {
		return names[k]
	}
	return "unknown"
}
