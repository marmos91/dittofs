package journalv2

import (
	"context"
	"iter"
	"time"
)

// Handle is one share's view of the journal. Every call through it that names
// a file is refused with an OtherShareError, and serves, appends, changes and
// offers nothing of that file, when the file belongs to another share's tag;
// a call that appends a record of a file with no tag claims the file for the
// handle's tag. After Close, every call fails with ErrClosed.
//
// The lock domains named below are taken in this order and no other: file
// append, capacity, file index, segment append, storage guard. A handle's
// methods are safe for concurrent use.
type Handle struct{}

// CloneSpec describes the range a clone copies into its destination: Len bytes
// of Src, in share SrcShare, from SrcOff, as of Src's change version AsOf,
// placed at DstOff of the destination.
type CloneSpec struct {
	SrcShare       ShareID
	Src            FileID
	SrcOff, DstOff int64
	Len            int64
	AsOf           Version
}

// Change is one thing Since reports about a file: a held extent, an unsettled
// removal marker, or a synced loss.
type Change struct {
	Kind    ChangeKind
	Extent  Extent  // the held extent, or the extent a removal marker covers
	Version Version // the content version of the change
	Mtime   int64   // a write's modification time as WriteAt was given it; 0 for every other kind
	// Clone is the clone target's spec; nil for every other kind.
	Clone *CloneSpec
}

// ChangeKind is what a Change reports. The zero ChangeKind names none.
type ChangeKind uint8

const (
	// ChangeWrite is a held extent, written or filled.
	ChangeWrite ChangeKind = iota + 1
	// ChangeTruncate is the marker of a Truncate.
	ChangeTruncate
	// ChangeDeallocate is the marker of a Deallocate.
	ChangeDeallocate
	// ChangeDelete is the marker of a Delete.
	ChangeDelete
	// ChangeCloneTarget is the marker of a CloneTarget; Change.Clone is its
	// spec.
	ChangeCloneTarget
	// ChangeLoss is a synced write the journal lost: the range reads as lost
	// rather than as the file was before the write.
	ChangeLoss
)

// Close ends the handle. New calls through it fail with ErrClosed, and Close
// returns once every call already in flight on it has returned. The handle's
// extents stay held.
func (h *Handle) Close() error {
	return ErrNotImplemented
}

// SetSyncPolicy changes the share's sync policy. Every record the handle
// writes after SetSyncPolicy returns follows the new policy; a record written
// before keeps the bound it was written under.
func (h *Handle) SetSyncPolicy(policy SyncPolicy) error {
	return ErrNotImplemented
}

// Reserve takes, at once, everything needed by the given number of writes
// carrying n record bytes in total, so an operation that must not fail
// part-way finds every refusal before it changes anything: the bytes against
// the share's limit and the journal's maximum, index entries against the
// extent limit, one version per write below the version limit, and the spare
// segments its bytes roll into. The reservation
// holds them until its Close.
//
// A refusal is a NoSpaceError naming LimitShare, LimitJournal, LimitIndex,
// LimitVersion or LimitSpare, returned at once, without waiting and without
// I/O. Reserve takes the capacity domain. A reservation is kept in memory: a
// restart drops it.
func (h *Handle) Reserve(n int64, writes int) (*Reservation, error) {
	return nil, ErrNotImplemented
}

// WriteAt stages p at offset off of file id, with mtime, the modification time
// the write sets in nanoseconds since the Unix epoch, or 0 when it is
// suspended. On success the extent is held and dirty, and every later read
// serves it wherever its version is newer than what was held. It is written
// before WriteAt returns but durable only once a Sync names the file, or,
// under the Bounded policy, within SyncBound.
//
// WriteAt draws a new content version and returns it. It reserves its record
// bytes before accepting them; a refusal is a NoSpaceError naming LimitShare,
// LimitJournal, LimitIndex, LimitVersion or LimitSpare, returned at once with
// no byte written. It does not draw on the headroom, but once less than half
// the headroom remains it is refused with a NoSpaceError naming
// LimitHeadroom, so what remains serves the records that return space. A
// write through a Reservation is exempt.
//
// It takes the file append domain, in which its version is assigned, then
// capacity, the file index and the segment's append domain.
func (h *Handle) WriteAt(ctx context.Context, id FileID, off int64, p []byte, mtime int64) (Version, error) {
	return Version{}, ErrNotImplemented
}

// Sync returns once every operation on the named files that returned before
// the call is durable. A failed sync window returns a SyncError naming the
// files whose writes it lost; a later successful sync never stands in for
// them. Sync holds no lock across the sync itself.
func (h *Handle) Sync(ctx context.Context, ids ...FileID) error {
	return ErrNotImplemented
}

// ReadAt fills p from the extents of file id the journal holds, starting at
// off. Every part of (off, len(p)) the journal does not hold is reported in
// missing, exactly, and the matching bytes of p are left untouched: they are
// never zeroed, and a missing extent never shows as a short read. The journal
// does not say why an extent is missing. held names the content version of
// each extent served, and asOf is the file's change version at the read,
// which a Fill of a missing extent passes back.
//
// ReadAt draws no version and appends nothing. It takes the file index domain
// to resolve the extents and releases it before it reads from storage.
func (h *Handle) ReadAt(ctx context.Context, id FileID, off int64, p []byte) (n int, missing []Extent, held []Held, asOf Version, err error) {
	return 0, nil, nil, Version{}, ErrNotImplemented
}

// Fill places bytes fetched from the remote tier into the parts of
// (off, len(p)) of file id the journal does not hold, and never overwrites a
// held extent. The filled extents are marked offloaded, since the bytes came
// from the remote tier, so the caller passes only bytes it fetched for exactly
// that extent of that file. v is the content version of the ref the bytes were
// fetched from; Fill draws no version.
//
// Fill is not a write path, and it is a separate method from WriteAt so that
// neither can be taken for the other: a write is newer than what is held and
// owes an offload, a fill is older and is offloaded already.
//
// Fill is refused with ErrStaleFill, writing nothing, when the file changed
// after asOf, the change version ReadAt returned. It reserves its record bytes
// as WriteAt does, refused by a NoSpaceError naming LimitShare, LimitJournal,
// LimitIndex or LimitSpare, and, like WriteAt, by one naming LimitHeadroom
// once less than half the headroom remains. Its absence check and its write are one step in
// the file append domain, atomic with respect to WriteAt on the same file.
func (h *Handle) Fill(ctx context.Context, id FileID, off int64, p []byte, asOf Version, v Version) error {
	return ErrNotImplemented
}

// Truncate stops holding every extent of file id at or beyond size, and
// narrows an extent straddling it. It draws a new version, leaves a removal
// marker until the caller settles it, and is durable on return.
//
// Its record draws on the reserved headroom and is never refused by the
// share's limit or the journal's maximum. It is refused by a NoSpaceError
// naming LimitHeadroom, when the headroom is exhausted and the removal frees
// nothing held, or LimitVersion. It takes the file append domain.
func (h *Handle) Truncate(ctx context.Context, id FileID, size int64) (Version, error) {
	return Version{}, ErrNotImplemented
}

// Deallocate stops holding extent e of file id, whatever its offloaded bit. It
// draws a new version, leaves a removal marker until the caller settles it,
// and is durable on return. A zero-length e is refused with ErrZeroLength and
// appends nothing.
//
// It draws on the headroom as Truncate does, and is refused as Truncate is or
// by a NoSpaceError naming LimitIndex when splitting a held extent would pass
// the index's cap on what removals add. It takes the file append domain.
func (h *Handle) Deallocate(ctx context.Context, id FileID, e Extent) (Version, error) {
	return Version{}, ErrNotImplemented
}

// Delete stops holding every extent of file id. It draws a new version, leaves
// a removal marker until the caller settles it, and is durable on return. It
// draws on the headroom and is refused as Truncate is. It takes the file
// append domain.
func (h *Handle) Delete(ctx context.Context, id FileID) (Version, error) {
	return Version{}, ErrNotImplemented
}

// CloneTarget stops holding (spec.DstOff, spec.Len) of file id as Deallocate
// does, and its record and marker carry spec, so a restart that finds the
// marker resumes the clone rather than leaving the range reading as a hole. A
// zero-length spec is refused with ErrZeroLength and appends nothing.
//
// It draws a new version and is durable on return. Its record frees nothing,
// so besides Deallocate's refusals it is refused with a NoSpaceError naming
// LimitHeadroom once less than half the headroom remains. It takes the file
// append domain.
func (h *Handle) CloneTarget(ctx context.Context, id FileID, spec CloneSpec) (Version, error) {
	return Version{}, ErrNotImplemented
}

// Since yields every held extent of file id, every unsettled removal marker
// and every synced loss whose version is strictly above from, in version
// order. It reads a consistent view of the file and blocks writes to it no
// longer than a read does. An unsynced loss is not yielded, and neither is a
// Forget.
//
// decision: the stub yields one ErrNotImplemented rather than nothing. An
// empty Since reads as "nothing changed since from", a silent wrong answer to
// the caller rebuilding what metadata has not recorded.
func (h *Handle) Since(ctx context.Context, id FileID, from Version) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		yield(Change{}, ErrNotImplemented)
	}
}

// Settle states that metadata has recorded every removal of file id at or
// below v, and drops those removal markers. It appends nothing and is not
// persisted: recovery rebuilds the markers, and the caller settles again.
// Settling before the removal is recorded is the caller's error.
func (h *Handle) Settle(id FileID, v Version) {}

// Extents returns the extents of file id the journal holds, in offset order.
// It says what the journal holds, not what exists: a caller answering where
// data or holes lie combines it with metadata, or it reports evicted content
// as a hole.
func (h *Handle) Extents(id FileID) ([]Extent, error) {
	return nil, ErrNotImplemented
}

// Files yields the share's files that hold an extent, an unsettled removal
// marker or an unsettled synced loss.
//
// decision: the stub yields nothing because the signature has no error to
// carry ErrNotImplemented. An empty result reads as "no files", so no caller
// may depend on it until the index lands.
func (h *Handle) Files() iter.Seq[FileID] {
	return func(func(FileID) bool) {}
}

// DirtyFiles yields the share's files holding an extent whose offloaded bit is
// unset, each with the write time of its oldest dirty byte. It is served from
// a list the journal maintains, costs O(dirty files), and neither walks the
// placement index nor reads a record.
//
// decision: the stub yields nothing because the signature has no error to
// carry ErrNotImplemented. An empty result reads as "no dirty files", so no
// caller may depend on it until the index lands.
func (h *Handle) DirtyFiles() iter.Seq2[FileID, time.Time] {
	return func(func(FileID, time.Time) bool) {}
}
