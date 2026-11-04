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

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

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
type FakeJournal struct {
	mu      sync.Mutex
	last    uint64 // the counter of the last version issued
	limit   uint64 // no version is issued at or above it; zero until set
	files   map[journal.FileID]*fakeFile
	held    map[journal.ShareTag]int64 // held bytes per tag
	losses  []journal.LossEvent
	gen     uint64
	closed  bool
	handles []*FakeHandle
}

// fakeFile is one file: its tag, its held pieces in offset order, and its
// unsettled removal markers and synced losses in version order.
type fakeFile struct {
	tag     journal.ShareTag
	pieces  []piece
	changes []journal.Change // markers and losses
	asOf    journal.Version
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
	return &FakeJournal{files: map[journal.FileID]*fakeFile{}, held: map[journal.ShareTag]int64{}}
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
func (j *FakeJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version, _ journal.SyncPolicy) (engine.Handle, error) {
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

// Stats reports the loss ring and generation, and each tag's held bytes.
func (j *FakeJournal) Stats() journal.Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := journal.Stats{Losses: slices.Clone(j.losses), LossGeneration: j.gen, Shares: map[journal.ShareTag]journal.ShareStats{}}
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
	j.losses = append(j.losses, journal.LossEvent{SeqNum: uint64(len(j.losses)) + 1, Share: f.tag, File: id, Extent: e, Version: v, Reason: why})
}

func (j *FakeJournal) Forget(context.Context, journal.ShareTag) error {
	return journal.ErrNotImplemented
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

// Sync answers at once: every operation of the fake is durable on return.
func (h *FakeHandle) Sync(context.Context, ...journal.FileID) error {
	h.j.mu.Lock()
	defer h.j.mu.Unlock()
	if h.closed {
		return journal.ErrClosed
	}
	return nil
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
