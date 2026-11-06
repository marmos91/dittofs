// Package enginetest holds the engine's test components: a fake journal for
// unit tests of the engine's own logic, and the fixture composing the engine
// over the real journal and metadata store for its conformance checks.
package enginetest

import (
	"cmp"
	"context"
	"iter"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// fakeStream is the number of the fake's one append stream, not 0 so that a
// mark that drops its stream shows.
const fakeStream uint32 = 3

// FakeJournal is an in-memory journal answering the engine's data path:
// versions, held and missing extents, asOf, removal markers, Since, the loss
// ring and generation, and the journal's named refusals. Every call it does
// not model answers journal.ErrNotImplemented, as the journal's own stubs
// do.
//
// It stands in for the journal only where the logic under test is the
// engine's: no check of what the journal itself does runs over it. Every
// operation is synced and durable at once, unless the test loses it with
// Lose.
//
// Its records lie on one append stream: each write and removal appends one,
// a write of its length and a removal of none, so a sync's marks name where
// they end. A Sync returns the stream's mark over the newest record of the
// files it names; SyncMarks is the mark of the stream's last sync, over its
// every record. Each mark names the stream, fakeStream.
type FakeJournal struct {
	mu        sync.Mutex
	last      uint64 // the counter of the last version issued
	limit     uint64 // no version is issued at or above it; zero until set
	files     map[journal.FileID]*fakeFile
	held      map[journal.ShareTag]int64 // held bytes per tag
	losses    []journal.LossEvent
	gen       uint64
	closed    bool
	handles   []*FakeHandle
	failing   map[journal.FileID]bool // FailSync's files: the next Sync naming one fails its window
	undurable map[journal.FileID]bool // files a failed window left undurable, until a Sync of theirs succeeds
	seg       uint64                  // the stream's segment, from 1
	end       int64                   // the end of the stream's last record in seg
	seq       uint64                  // the stream's last record's sequence number
	synced    entity.SyncMark         // the mark of the stream's last sync; zero before one
	recorded  [][]entity.SyncMark     // what MarksRecorded was handed, in order
}

// fakeFile is one file: its tag, its held pieces in offset order, and its
// unsettled removal markers and synced losses in version order.
type fakeFile struct {
	tag     journal.ShareTag
	pieces  []piece
	changes []journal.Change // markers and losses
	asOf    journal.Version
	rec     record // its newest record
}

// record is where a record lies on the stream: its segment, its end there,
// its length and its sequence number, zero for none.
type record struct {
	seg uint64
	end int64
	n   int64
	seq uint64
}

// piece is a held extent: its bytes from off, and the version and mtime of
// the write that put them there.
type piece struct {
	off   int64
	b     []byte
	v     journal.Version
	mtime int64
}

func (p piece) end() int64 { return p.off + int64(len(p.b)) }

// NewFakeJournal returns an empty fake journal. It issues no version until
// SetVersionLimit, as the journal does not.
func NewFakeJournal() *FakeJournal {
	return &FakeJournal{files: map[journal.FileID]*fakeFile{}, held: map[journal.ShareTag]int64{}, seg: 1}
}

var _ engine.Journal = (*FakeJournal)(nil)

// FakeHandle is one share's handle on a FakeJournal.
type FakeHandle struct {
	j      *FakeJournal
	tag    journal.ShareTag
	limit  int64
	closed bool
}

var _ engine.Handle = (*FakeHandle)(nil)

// Share attaches tag with limit, raising the version counter above floor.
func (j *FakeJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version) (engine.Handle, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.closed:
		return nil, journal.ErrClosed
	case limit < 0:
		return nil, journal.ErrInvalidSetting
	}
	j.last = max(j.last, floor.Counter)
	h := &FakeHandle{j: j, tag: tag, limit: limit}
	j.handles = append(j.handles, h)
	return h, nil
}

// Draw issues the next version.
func (j *FakeJournal) Draw() (journal.Version, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return journal.Version{}, journal.ErrClosed
	}
	return j.draw()
}

// draw issues the next version, refused at the version limit. j.mu is held.
func (j *FakeJournal) draw() (journal.Version, error) {
	if j.last+1 >= j.limit {
		return journal.Version{}, &journal.NoSpaceError{Limit: journal.LimitVersion, Missing: 1, Unit: journal.UnitVersions}
	}
	j.last++
	return journal.Version{Counter: j.last}, nil
}

// SetVersionLimit sets the version limit; a mark with a non-zero epoch is
// refused.
func (j *FakeJournal) SetVersionLimit(mark journal.Version) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if mark.Epoch != 0 {
		return journal.ErrUnknownFormat
	}
	j.limit = mark.Counter
	return nil
}

// Stats reports the loss ring and generation, each tag's held bytes, the
// version limit last set and the next version the counter issues.
func (j *FakeJournal) Stats() journal.Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := journal.Stats{
		Losses:         slices.Clone(j.losses),
		LossGeneration: j.gen,
		Shares:         map[journal.ShareTag]journal.ShareStats{},
		VersionLimit:   journal.Version{Counter: j.limit},
		NextVersion:    journal.Version{Counter: j.last + 1},
	}

	for tag, n := range j.held {
		s.Shares[tag] = journal.ShareStats{HeldBytes: n}
		s.HeldBytes += n
	}
	return s
}

// Close closes the journal and every handle.
func (j *FakeJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return journal.ErrClosed
	}
	j.closed = true
	for _, h := range j.handles {
		h.closed = true
	}
	return nil
}

// Lose drops what file id holds in e as the journal does on a loss: the bytes
// are no longer held, and the loss, at the newest version it dropped, is an
// event in the loss ring and, unless its sync window failed, a synced change
// Since reports; a corrupt loss and a failed window's also raise the loss
// generation, a stale one does not. A loss of nothing held records nothing.
//
// ponytail: a failed window's loss cuts the extent and raises the generation
// once per call, where RFC 1 holds again what the failed writes superseded
// and raises the generation once per window. Model both once a test drops
// several extents in one window or reads what a failed write overwrote.
func (j *FakeJournal) Lose(id journal.FileID, e journal.Extent, why journal.LossReason) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f := j.files[id]
	if f == nil {
		return
	}
	v, ok := j.cut(f, e.Off, e.Off+e.Len)
	if !ok {
		return
	}
	if why != journal.LossFailedWindow { // a failed window's loss was never synced, so Since has nothing of it
		f.changes = append(f.changes, journal.Change{Kind: journal.ChangeLoss, Extent: e, Version: v})
	}
	if why == journal.LossCorrupt || why == journal.LossFailedWindow {
		j.gen++
	}
	j.losses = append(j.losses, journal.LossEvent{SeqNum: j.nextSeq(), Share: f.tag, File: id, Extent: e, Version: v, Reason: why})
}

// Report puts ev in the loss ring as the journal reports a loss whose
// content it already dropped, raising the loss generation: ev's SeqNum, when
// set, skips the events between, as a ring that overflowed would; zero is the
// next one.
func (j *FakeJournal) Report(ev journal.LossEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ev.SeqNum == 0 {
		ev.SeqNum = j.nextSeq()
	}
	j.gen++
	j.losses = append(j.losses, ev)
}

func (j *FakeJournal) nextSeq() uint64 {
	if len(j.losses) == 0 {
		return 1
	}
	return j.losses[len(j.losses)-1].SeqNum + 1
}

// Forget drops tag whole: every file of the tag and the bytes it holds. It is
// refused with ErrHandleOpen while a handle of tag is open, and forgetting a
// tag that holds nothing succeeds.
func (j *FakeJournal) Forget(_ context.Context, tag journal.ShareTag) error {
	j.mu.Lock()

	defer j.mu.Unlock()

	if j.closed {
		return journal.ErrClosed
	}

	for _, h := range j.handles {
		if h.tag == tag && !h.closed {
			return journal.ErrHandleOpen
		}
	}

	for id, f := range j.files {
		if f.tag == tag {
			delete(j.files, id)
		}
	}

	delete(j.held, tag)

	return nil
}

func (j *FakeJournal) Strand(journal.ShareTag) error { return journal.ErrNotImplemented }
func (j *FakeJournal) Repack(context.Context, int64, journal.RepackScope) (int64, error) {
	return 0, journal.ErrNotImplemented
}

// cut stops holding [from, to) of f, narrowing or splitting the pieces it
// crosses, and answers the newest version it dropped, ok false when it
// dropped nothing. j.mu is held.
func (j *FakeJournal) cut(f *fakeFile, from, to int64) (newest journal.Version, ok bool) {
	var kept []piece
	for _, p := range f.pieces {
		if p.end() <= from || p.off >= to {
			kept = append(kept, p)
			continue
		}
		if p.off < from {
			kept = append(kept, piece{p.off, p.b[:from-p.off], p.v, p.mtime})
		}
		if p.end() > to {
			kept = append(kept, piece{to, p.b[to-p.off:], p.v, p.mtime})
		}
		j.held[f.tag] -= min(p.end(), to) - max(p.off, from)
		if !ok || p.v.Compare(newest) > 0 {
			newest, ok = p.v, true
		}
	}
	f.pieces = kept
	return newest, ok
}

// file returns id's file for h's tag, or refuses a file of another tag. A
// file with no records belongs to no tag: claim gives it to h's tag, as a
// write or a removal does by leaving a record; without claim it is answered
// empty and stays unclaimed. j.mu is held.
func (h *FakeHandle) file(id journal.FileID, claim bool) (*fakeFile, error) {
	if h.closed {
		return nil, journal.ErrClosed
	}
	f := h.j.files[id]
	if f == nil {
		f = &fakeFile{tag: h.tag}
		if claim {
			h.j.files[id] = f
		}
	}
	if f.tag != h.tag {
		return nil, &journal.OtherShareError{File: id, Tag: h.tag, Owner: f.tag}
	}
	return f, nil
}

// WriteAt holds p at off under a new version, refused by the share's limit
// on held bytes or the version limit.
func (h *FakeHandle) WriteAt(_ context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	j := h.j
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := h.file(id, true)
	if err != nil {
		return journal.Version{}, err
	}
	if len(p) == 0 {
		return journal.Version{}, journal.ErrZeroLength
	}
	// ponytail: the share's limit counts held bytes, not record bytes, so an
	// overwrite counts its new bytes only once the old are cut. A unit test of
	// the engine's refusal mapping needs only that a refusal names its limit.
	if h.limit > 0 && j.held[h.tag]+int64(len(p)) > h.limit {
		return journal.Version{}, &journal.NoSpaceError{Limit: journal.LimitShare, Missing: j.held[h.tag] + int64(len(p)) - h.limit, Unit: journal.UnitBytes}
	}
	v, err := j.draw()
	if err != nil {
		return journal.Version{}, err
	}
	j.cut(f, off, off+int64(len(p)))
	j.appendRecord(f, int64(len(p)))
	f.pieces = append(f.pieces, piece{off, slices.Clone(p), v, mtime})
	slices.SortFunc(f.pieces, func(a, b piece) int { return cmp.Compare(a.off, b.off) })
	j.held[h.tag] += int64(len(p))
	f.asOf = v
	return v, nil
}

// ReadAt fills p from the pieces held over [off, off+len(p)), reports every
// part not held in missing, leaving those bytes of p untouched, and names the
// version of each part served.
func (h *FakeHandle) ReadAt(_ context.Context, id journal.FileID, off int64, p []byte) (n int, missing []journal.Extent, held []journal.Held, asOf journal.Version, err error) {
	j := h.j
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := h.file(id, false)
	if err != nil {
		return 0, nil, nil, journal.Version{}, err
	}
	end, at := off+int64(len(p)), off
	for _, pc := range f.pieces {
		lo, hi := max(pc.off, off), min(pc.end(), end)
		if lo >= hi {
			continue
		}
		if lo > at {
			missing = append(missing, journal.Extent{Off: at, Len: lo - at})
		}
		n += copy(p[lo-off:hi-off], pc.b[lo-pc.off:])
		held = append(held, journal.Held{Extent: journal.Extent{Off: lo, Len: hi - lo}, Version: pc.v})
		at = hi
	}
	if at < end {
		missing = append(missing, journal.Extent{Off: at, Len: end - at})
	}
	return n, missing, held, f.asOf, nil
}

// appendRecord appends a record of n bytes of f to the stream. j.mu is held.
func (j *FakeJournal) appendRecord(f *fakeFile, n int64) {
	j.seq++
	j.end += n
	f.rec = record{j.seg, j.end, n, j.seq}
}

// Sync answers at once: every operation of the fake is durable on return,
// with the stream's mark over the newest record of the files named, none
// when none holds a record. The first Sync naming a file FailSync armed
// answers a SyncError naming those of its files.
func (h *FakeHandle) Sync(_ context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	h.j.mu.Lock()
	defer h.j.mu.Unlock()
	if h.closed {
		return nil, journal.ErrClosed
	}
	var failed []journal.FileID
	for _, id := range ids {
		if h.j.failing[id] {
			failed = append(failed, id)
			delete(h.j.failing, id)
		}
	}
	if len(failed) > 0 {
		return nil, &journal.SyncError{Files: failed, Err: journal.ErrSyncFailed}
	}
	j := h.j
	var newest record
	for _, id := range ids {
		delete(j.undurable, id)
		if f := j.files[id]; f != nil && f.rec.seq > newest.seq {
			newest = f.rec
		}
	}
	if j.seq > 0 {
		j.synced = entity.SyncMark{Stream: fakeStream, Segment: j.seg, Through: j.end, Offset: j.end, Sequence: j.seq}
	}
	if newest.seq == 0 {
		return nil, nil
	}
	return []entity.SyncMark{{Stream: fakeStream, Segment: newest.seg, Through: j.end, Offset: newest.end, Sequence: newest.seq}}, nil
}

// EndSegment ends the stream's segment, as a failed sync does: each file's
// newest record is appended again, keeping its number, to a new segment and
// synced there, so the stream's mark names the new segment.
func (j *FakeJournal) EndSegment() {
	j.mu.Lock()
	defer j.mu.Unlock()
	var fs []*fakeFile
	for _, f := range j.files {
		if f.rec.seq != 0 {
			fs = append(fs, f)
		}
	}
	slices.SortFunc(fs, func(a, b *fakeFile) int { return cmp.Compare(a.rec.seq, b.rec.seq) })
	j.seg, j.end = j.seg+1, 0
	for _, f := range fs {
		j.end += f.rec.n
		f.rec.seg, f.rec.end = j.seg, j.end
	}
	j.synced = entity.SyncMark{Stream: fakeStream, Segment: j.seg, Through: j.end, Offset: j.end, Sequence: j.seq}
}

// SyncMarks is the mark of the stream's last sync, none before one.
func (j *FakeJournal) SyncMarks() []entity.SyncMark {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.synced.Segment == 0 {
		return nil
	}
	return []entity.SyncMark{j.synced}
}

// MarksRecorded logs marks.
func (j *FakeJournal) MarksRecorded(marks []entity.SyncMark) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recorded = append(j.recorded, slices.Clone(marks))
}

// Recorded is what MarksRecorded was handed, call by call.
func (j *FakeJournal) Recorded() [][]entity.SyncMark {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.recorded)
}

// Durable reports whether file id's writes are durable: false from a
// FailSync naming it until a Sync of it succeeds.
func (j *FakeJournal) Durable(id journal.FileID) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.undurable[id]
}

// FailSync fails the sync window of file id's extent e: the extent is lost
// as a failed window's, and the next Sync naming id answers a SyncError
// naming it, as the journal reports a failed window to the next Sync of each
// file that had a write in it. Each of also, and id, is left undurable until
// a Sync of it succeeds, as a failed window promises nothing durable.
func (j *FakeJournal) FailSync(id journal.FileID, e journal.Extent, also ...journal.FileID) {
	j.Lose(id, e, journal.LossFailedWindow)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failing == nil {
		j.failing = map[journal.FileID]bool{}
		j.undurable = map[journal.FileID]bool{}
	}
	j.failing[id] = true
	j.undurable[id] = true
	for _, a := range also {
		j.undurable[a] = true
	}
}

// remove runs a removal of kind k over [from, to) under a new version and
// leaves its marker.
func (h *FakeHandle) remove(id journal.FileID, k journal.ChangeKind, from, to int64) (journal.Version, error) {
	j := h.j
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := h.file(id, true)
	if err != nil {
		return journal.Version{}, err
	}
	v, err := j.draw()
	if err != nil {
		return journal.Version{}, err
	}
	j.cut(f, from, to)
	j.appendRecord(f, 0)
	f.changes = append(f.changes, journal.Change{Kind: k, Extent: journal.Extent{Off: from, Len: to - from}, Version: v})
	f.asOf = v
	return v, nil
}

// endOfFile is the end offset a truncate or delete marker covers to.
const endOfFile = 1<<63 - 1

// Truncate stops holding every byte at or beyond size.
func (h *FakeHandle) Truncate(_ context.Context, id journal.FileID, size int64) (journal.Version, error) {
	return h.remove(id, journal.ChangeTruncate, size, endOfFile)
}

// Deallocate stops holding e; a zero-length e is refused.
func (h *FakeHandle) Deallocate(_ context.Context, id journal.FileID, e journal.Extent) (journal.Version, error) {
	if e.Len == 0 {
		return journal.Version{}, journal.ErrZeroLength
	}
	return h.remove(id, journal.ChangeDeallocate, e.Off, e.Off+e.Len)
}

// Delete stops holding every byte of the file.
func (h *FakeHandle) Delete(_ context.Context, id journal.FileID) (journal.Version, error) {
	return h.remove(id, journal.ChangeDelete, 0, endOfFile)
}

// Since yields every held piece, unsettled marker and synced loss of the
// file above from, in version order.
func (h *FakeHandle) Since(_ context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error] {
	j := h.j
	j.mu.Lock()
	f, err := h.file(id, false)
	var out []journal.Change
	if err == nil {
		for _, p := range f.pieces {
			out = append(out, journal.Change{Kind: journal.ChangeWrite, Extent: journal.Extent{Off: p.off, Len: int64(len(p.b))}, Version: p.v, Mtime: p.mtime})
		}
		out = append(out, f.changes...)
	}
	j.mu.Unlock()
	out = slices.DeleteFunc(out, func(c journal.Change) bool { return c.Version.Compare(from) <= 0 })
	slices.SortStableFunc(out, func(a, b journal.Change) int { return a.Version.Compare(b.Version) })
	return func(yield func(journal.Change, error) bool) {
		if err != nil {
			yield(journal.Change{}, err)
			return
		}
		for _, c := range out {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// Settle drops the file's markers and losses at or below v.
func (h *FakeHandle) Settle(id journal.FileID, v journal.Version) {
	h.j.mu.Lock()
	defer h.j.mu.Unlock()
	if f := h.j.files[id]; f != nil && f.tag == h.tag {
		f.changes = slices.DeleteFunc(f.changes, func(c journal.Change) bool { return c.Version.Compare(v) <= 0 })
	}
}

// Files yields the share's files holding a piece, a marker or a loss.
func (h *FakeHandle) Files() iter.Seq[journal.FileID] {
	h.j.mu.Lock()
	var ids []journal.FileID
	for id, f := range h.j.files {
		if f.tag == h.tag && len(f.pieces)+len(f.changes) > 0 {
			ids = append(ids, id)
		}
	}
	h.j.mu.Unlock()
	return slices.Values(ids)
}

// Close closes the handle; its pieces stay held.
func (h *FakeHandle) Close() error {
	h.j.mu.Lock()
	defer h.j.mu.Unlock()
	if h.closed {
		return journal.ErrClosed
	}
	h.closed = true
	return nil
}

func (h *FakeHandle) CloneTarget(context.Context, journal.FileID, journal.CloneSpec) (journal.Version, error) {
	return journal.Version{}, journal.ErrNotImplemented
}

func (h *FakeHandle) Reserve(int64, int) (engine.Reservation, error) {
	return nil, journal.ErrNotImplemented
}
