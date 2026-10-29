package openstate

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// The filesystem service maps each of these once to its own neutral errors.
var (
	ErrBadState    = errors.New("openstate: reference names no record")
	ErrNotYours    = errors.New("openstate: state held by another client or principal")
	ErrOldState    = errors.New("openstate: reference sequence older than the state's")
	ErrStaleClient = errors.New("openstate: client expired, or owes a reclaim it must confirm again for")
	ErrClientInUse = errors.New("openstate: client name held by another principal")
	ErrClientBusy  = errors.New("openstate: client still holds a session or state")
	ErrInvalid     = errors.New("openstate: request out of bounds: a client name past 1,024 bytes or an Adapter record past 4 KiB")
	ErrDelay       = errors.New("openstate: retry later")
	ErrStopped     = errors.New("openstate: table not started, or stopped")

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
	DelayResources DelayCause = iota + 1 // a cap: pending proposals per node or per source
)

func (e *DelayError) Error() string        { return ErrDelay.Error() }
func (e *DelayError) Is(target error) bool { return target == ErrDelay }
