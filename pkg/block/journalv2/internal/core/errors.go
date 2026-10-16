package core

import (
	"errors"
	"fmt"
)

// decision: the error set is declared here and re-exported by the public
// package, for the reason the value types are: the internal packages return
// these errors, and they cannot import the public package without a cycle.

// The sentinel errors. A typed error below matches its sentinel under
// errors.Is; a caller tests for the sentinel and recovers the type with
// errors.As only when it needs the details.
var (
	// ErrNotImplemented is returned by an operation that is declared but not
	// yet implemented. It is never retried.
	ErrNotImplemented = errors.New("journal: not implemented")

	// ErrClosed is returned by any call that reaches a journal or a handle
	// after its Close began. The journal's state is unchanged; the call is not
	// retried on the same journal or handle.
	ErrClosed = errors.New("journal: closed")

	// ErrHandleOpen is returned by Forget while a handle of the tag is open.
	// Nothing is forgotten; the caller closes the tag's handles and calls
	// Forget again.
	ErrHandleOpen = errors.New("journal: a handle of the tag is open")

	// ErrJournalExists is returned by Init when the directory already holds a
	// format file or a segment. Nothing is written.
	ErrJournalExists = errors.New("journal: directory already holds a journal")

	// ErrNoJournal is returned by Open and Restamp, given a non-zero expected
	// identity, when the directory holds no journal, empty or missing. Neither
	// ever creates one; only Init does.
	ErrNoJournal = errors.New("journal: directory holds no journal")

	// ErrIdentityMismatch is matched by every IdentityError: the journal in
	// the directory is not the one the caller expected, or the caller expected
	// none. It is never retried.
	ErrIdentityMismatch = errors.New("journal: identity does not match")

	// ErrUnknownFormat is returned by Open when the format file names a
	// format version this implementation does not recognise, or a record
	// carries an unrecognised kind or a content version with a non-zero epoch.
	// The journal is not opened, upgraded or repaired. It is never retried.
	ErrUnknownFormat = errors.New("journal: unknown format")

	// ErrCorrupt is returned when a structure the journal must read does not
	// verify against its checksum and nothing can stand in for it: the format
	// file at Open, or a record an offer's reader must verify before it returns
	// its bytes. The structure is not repaired. It is never retried.
	ErrCorrupt = errors.New("journal: checksum does not verify")

	// ErrStaleFill is returned by Fill when the file's change version is newer
	// than the asOf the fill was read at. Nothing is written; the caller reads
	// again and fetches what is still missing.
	ErrStaleFill = errors.New("journal: fill is older than the file")

	// ErrNotOffloaded is matched by every NotOffloadedError: Release named an
	// extent whose offloaded bit is unset. Nothing is released.
	ErrNotOffloaded = errors.New("journal: extent is not offloaded")

	// ErrNotOffered is returned by an offer's report naming an extent the offer
	// did not contain, and by an offer's reader reading outside the offered
	// extents. Nothing is marked or returned.
	ErrNotOffered = errors.New("journal: extent was not offered")

	// ErrOfferEnded is returned by an offer's report, and by its reader, once
	// the offer's callback has returned. Nothing is marked or returned.
	ErrOfferEnded = errors.New("journal: offer has ended")

	// ErrZeroLength is returned by a Deallocate or CloneTarget of a
	// zero-length extent. Nothing is appended and no version is drawn.
	ErrZeroLength = errors.New("journal: zero-length extent")

	// ErrSyncFailed is matched by every SyncError: a sync window failed. The
	// writes it covered are not durable, and a later successful sync never
	// stands in for them.
	ErrSyncFailed = errors.New("journal: sync failed")

	// ErrNoSpace is matched by every NoSpaceError: a reservation was refused
	// by one of the journal's limits.
	ErrNoSpace = errors.New("journal: no space")

	// ErrOtherShare is matched by every OtherShareError: a call through one
	// share's handle named a file that belongs to another share. Nothing of
	// the file is served, appended, changed or offered. It is never retried.
	ErrOtherShare = errors.New("journal: file belongs to another share")
)

// Limit names the limit that refused a reservation.
type Limit uint8

// The limits. The zero Limit names none, so a NoSpaceError whose Limit was
// never set does not read as a refusal by a real limit.
const (
	// LimitShare is the share's limit on the record bytes of its records on
	// disk. Transient while offload, release and repack can drain the share.
	LimitShare Limit = iota + 1

	// LimitJournal is the journal's configured maximum. Transient while
	// offload, release and repack can drain the journal.
	LimitJournal

	// LimitIndex is the bound on placement index entries.
	LimitIndex

	// LimitHeadroom is the reserved headroom for records that carry no bytes.
	LimitHeadroom

	// LimitVersion is the version limit: no version is issued at or above it
	// until the caller raises it.
	LimitVersion

	// LimitSpare is the pool of spare segments a roll takes from, when no
	// spare is ready.
	LimitSpare
)

// String returns the limit's name: share, journal, index, headroom, version or
// spare.
func (l Limit) String() string {
	switch l {
	case LimitShare:
		return "share"
	case LimitJournal:
		return "journal"
	case LimitIndex:
		return "index"
	case LimitHeadroom:
		return "headroom"
	case LimitVersion:
		return "version"
	case LimitSpare:
		return "spare"
	}
	return fmt.Sprintf("Limit(%d)", uint8(l))
}

// Unit is what a NoSpaceError's Missing counts.
type Unit uint8

// The units. The zero Unit names none.
const (
	UnitBytes Unit = iota + 1
	UnitEntries
	UnitVersions
	UnitSegments
)

// String returns the unit's name: bytes, entries, versions or segments.
func (u Unit) String() string {
	switch u {
	case UnitBytes:
		return "bytes"
	case UnitEntries:
		return "entries"
	case UnitVersions:
		return "versions"
	case UnitSegments:
		return "segments"
	}
	return fmt.Sprintf("Unit(%d)", uint8(u))
}

// NoSpaceError is a reservation refused by one of the journal's limits. It is
// returned at once, performs no I/O and never waits: space is freed only by
// the caller's own offload, release and repack, so a journal that waited would
// wait on its caller. Missing counts, in Unit, how much the reservation lacked.
//
// A refusal by LimitShare or LimitJournal is transient while offload, release
// and repack can drain it. LimitVersion lasts until the caller raises the
// version limit, and LimitSpare until a spare is ready.
//
// decision: a full filesystem underneath is never a NoSpaceError. It surfaces
// as the platform's *fs.PathError wrapping syscall.ENOSPC, so for any one
// error errors.Is(err, ErrNoSpace) and errors.Is(err, syscall.ENOSPC) are
// never both true. The journal's limits are the caller's to drain; a full
// device is the operator's, and the two must not be answered alike.
type NoSpaceError struct {
	Limit   Limit
	Missing int64
	Unit    Unit
}

func (e *NoSpaceError) Error() string {
	return fmt.Sprintf("journal: no space: refused by the %s limit, %d %s missing",
		e.Limit, e.Missing, e.Unit)
}

// Is reports whether target is ErrNoSpace.
func (e *NoSpaceError) Is(target error) bool {
	return target == ErrNoSpace
}

// OtherShareError is a call through the handle of share Tag that named File,
// which belongs to share Owner. It is also returned, through either tag, for a
// file the journal rebuilt under two tags at open, until one is forgotten.
type OtherShareError struct {
	File  FileID
	Tag   ShareTag
	Owner ShareTag
}

func (e *OtherShareError) Error() string {
	return fmt.Sprintf("journal: file %x belongs to share tag %d, not %d",
		e.File.Bytes(), e.Owner, e.Tag)
}

// Is reports whether target is ErrOtherShare.
func (e *OtherShareError) Is(target error) bool {
	return target == ErrOtherShare
}

// IdentityError is an Open or Restamp refused because the journal in the
// directory is not the one expected. Found and Installation are the identity
// and installation ID the directory's format file holds, and Segments says
// whether the directory holds any segment, so a caller that expected no
// journal can tell an interrupted Init, which leaves none, from a journal in
// use.
type IdentityError struct {
	Found        JournalID
	Installation InstallationID
	Segments     bool
}

func (e *IdentityError) Error() string {
	return fmt.Sprintf("journal: identity %x of installation %x does not match (segments present: %t)",
		e.Found, e.Installation, e.Segments)
}

// Is reports whether target is ErrIdentityMismatch.
func (e *IdentityError) Is(target error) bool {
	return target == ErrIdentityMismatch
}

// SyncError is a failed sync window: the sync of Segment failed with Err, and
// the writes of Files it covered are not durable.
type SyncError struct {
	Segment SegmentID
	Files   []FileID
	Err     error
}

func (e *SyncError) Error() string {
	return fmt.Sprintf("journal: sync of segment %d failed for %d files: %v",
		e.Segment, len(e.Files), e.Err)
}

// Is reports whether target is ErrSyncFailed.
func (e *SyncError) Is(target error) bool {
	return target == ErrSyncFailed
}

// Unwrap returns the cause of the failed sync.
func (e *SyncError) Unwrap() error {
	return e.Err
}

// NotOffloadedError is a Release of File refused because Extents, the part of
// what it named whose offloaded bit is unset, are not offloaded. A refused
// Release releases nothing.
type NotOffloadedError struct {
	File    FileID
	Extents []Extent
}

func (e *NotOffloadedError) Error() string {
	return fmt.Sprintf("journal: release of file %x refused: %d extents not offloaded",
		e.File.Bytes(), len(e.Extents))
}

// Is reports whether target is ErrNotOffloaded.
func (e *NotOffloadedError) Is(target error) bool {
	return target == ErrNotOffloaded
}
