package journalv2

import "github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"

// The journal's errors, declared in internal/core so the internal packages can
// return them. Each one's contract is documented at its declaration.
var (
	ErrNotImplemented   = core.ErrNotImplemented
	ErrClosed           = core.ErrClosed
	ErrHandleOpen       = core.ErrHandleOpen
	ErrJournalExists    = core.ErrJournalExists
	ErrNoJournal        = core.ErrNoJournal
	ErrIdentityMismatch = core.ErrIdentityMismatch
	ErrUnknownFormat    = core.ErrUnknownFormat
	ErrCorrupt          = core.ErrCorrupt
	ErrStaleFill        = core.ErrStaleFill
	ErrNotOffloaded     = core.ErrNotOffloaded
	ErrNotOffered       = core.ErrNotOffered
	ErrOfferEnded       = core.ErrOfferEnded
	ErrZeroLength       = core.ErrZeroLength
	ErrSyncFailed       = core.ErrSyncFailed
	ErrNoSpace          = core.ErrNoSpace
	ErrOtherShare       = core.ErrOtherShare
)

type (
	Limit             = core.Limit
	Unit              = core.Unit
	NoSpaceError      = core.NoSpaceError
	OtherShareError   = core.OtherShareError
	IdentityError     = core.IdentityError
	SyncError         = core.SyncError
	NotOffloadedError = core.NotOffloadedError
)

// The limits a NoSpaceError names; see Limit.
const (
	LimitShare    = core.LimitShare
	LimitJournal  = core.LimitJournal
	LimitIndex    = core.LimitIndex
	LimitHeadroom = core.LimitHeadroom
	LimitVersion  = core.LimitVersion
	LimitSpare    = core.LimitSpare
)

// The units a NoSpaceError counts in; see Unit.
const (
	UnitBytes    = core.UnitBytes
	UnitEntries  = core.UnitEntries
	UnitVersions = core.UnitVersions
	UnitSegments = core.UnitSegments
)
