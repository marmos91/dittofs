package openstate

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// The filesystem service maps each of these once to its own neutral errors.
var (
	ErrBadState    = errors.New("openstate: reference names no record")
	ErrStaleState  = errors.New("openstate: state lost with its shard's open state")
	ErrNotYours    = errors.New("openstate: state held by another client or principal")
	ErrOldState    = errors.New("openstate: reference sequence older than the state's")
	ErrStaleClient = errors.New("openstate: client expired, or owes a reclaim it must confirm again for")
	ErrClientInUse = errors.New("openstate: client name held by another principal")
	ErrClientBusy  = errors.New("openstate: client still holds a session or state")
	ErrNoRecall    = errors.New("openstate: acknowledgement of a grant with no recall in flight")
	ErrInvalid     = errors.New("openstate: request out of bounds: a client or owner name past 1,024 bytes, an Adapter record past 4 KiB, a lock list past 64 ranges, delete on close without delete access, or a recall answer claiming caching its grant does not hold or its recall did not offer")
	ErrDelay       = errors.New("openstate: retry later")
	ErrStopped     = errors.New("openstate: table not started, or stopped")

	ErrShareViolation = errors.New("openstate: deny mode conflict")
	ErrDeletePending  = errors.New("openstate: file is delete pending")
	ErrAccess         = errors.New("openstate: the open was not granted the access the request needs")
	ErrLocked         = errors.New("openstate: range locked") // as *LockedError from a lock request, naming the holder
	ErrRangeNotLocked = errors.New("openstate: unlock of a range the owner does not hold")
	ErrBadSeqID       = errors.New("openstate: owner sequence out of order")
	ErrLocksHeld      = errors.New("openstate: the owner or lock state still holds locks")
	ErrRevoked        = errors.New("openstate: state revoked, by a recall past its deadline or an administrator, and not yet freed") // as *RevokedError, naming the cause

	// ErrRecordChanged is the store's own: AddSession hands its refusal on
	// as it is, with the current record.
	ErrRecordChanged = kvstore.ErrRecordChanged
)

// DelayError is a retry-later. errors.Is(err, ErrDelay) holds for it, and its
// Cause says why.
type DelayError struct{ Cause DelayCause }

// DelayCause is why a call is to be retried.
type DelayCause uint8

const (
	DelayRecall    DelayCause = iota + 1 // a caching grant is being recalled
	DelayResources                       // a cap: pending proposals per node or per source, or recalls not yet sent
)

func (e *DelayError) Error() string        { return ErrDelay.Error() }
func (e *DelayError) Is(target error) bool { return target == ErrDelay }

// LockedError is a lock conflict. errors.Is(err, ErrLocked) holds for it, and
// Holder is the conflicting lock, which a protocol's denial reports.
type LockedError struct{ Holder Lock }

func (e *LockedError) Error() string        { return ErrLocked.Error() }
func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// RevokedError names revoked state. errors.Is(err, ErrRevoked) holds for it,
// and Cause is why it was revoked: RevokedByAdmin or RevokedGrant.
type RevokedError struct{ Cause RevokeReason }

func (e *RevokedError) Error() string        { return ErrRevoked.Error() }
func (e *RevokedError) Is(target error) bool { return target == ErrRevoked }
