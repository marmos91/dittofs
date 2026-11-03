// Package enginev2test holds the engine's test components: a fake journal for
// unit tests of the engine's own logic, and the fixture composing the engine
// over the real journal and metadata store for its conformance checks.
package enginev2test

import (
	"cmp"
	"context"
	"iter"
	"slices"
	"sync"

	"github.com/marmos91/dittofs/pkg/block/enginev2"
	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// FakeJournal is an in-memory journal answering the engine's data path:
// versions, held and missing extents, asOf, removal markers, Since, the loss
// ring and generation, and the journal's named refusals. Every call it does
// not model answers journalv2.ErrNotImplemented, as the journal's own stubs
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
	files   map[journalv2.FileID]*fakeFile
	held    map[journalv2.ShareTag]int64 // held bytes per tag
	losses  []journalv2.LossEvent
	gen     uint64
	closed  bool
	handles []*FakeHandle
}

// fakeFile is one file: its tag, its held pieces in offset order, and its
// unsettled removal markers and synced losses in version order.
type fakeFile struct {
	tag     journalv2.ShareTag
	pieces  []piece
	changes []journalv2.Change // markers and losses
	asOf    journalv2.Version
}

// piece is a held extent: its bytes from off, and the version and mtime of
// the write that put them there.
type piece struct {
	off   int64
	b     []byte
	v     journalv2.Version
	mtime int64
}

func (p piece) end() int64 { return p.off + int64(len(p.b)) }

// NewFakeJournal returns an empty fake journal. It issues no version until
// SetVersionLimit, as the journal does not.
func NewFakeJournal() *FakeJournal {
	return &FakeJournal{files: map[journalv2.FileID]*fakeFile{}, held: map[journalv2.ShareTag]int64{}}
}

var _ enginev2.Journal = (*FakeJournal)(nil)

// FakeHandle is one share's handle on a FakeJournal.
type FakeHandle struct {
	j      *FakeJournal
	tag    journalv2.ShareTag
	limit  int64
	closed bool
}

var _ enginev2.Handle = (*FakeHandle)(nil)

// Share attaches tag with limit, raising the version counter above floor.
func (j *FakeJournal) Share(tag journalv2.ShareTag, limit int64, floor journalv2.Version, _ journalv2.SyncPolicy) (enginev2.Handle, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.closed:
		return nil, journalv2.ErrClosed
	case limit < 0:
		return nil, journalv2.ErrInvalidSetting
	}
	j.last = max(j.last, floor.Counter)
	h := &FakeHandle{j: j, tag: tag, limit: limit}
	j.handles = append(j.handles, h)
	return h, nil
}

// Draw issues the next version.
func (j *FakeJournal) Draw() (journalv2.Version, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return journalv2.Version{}, journalv2.ErrClosed
	}
	return j.draw()
}

// draw issues the next version, refused at the version limit. j.mu is held.
func (j *FakeJournal) draw() (journalv2.Version, error) {
	if j.last+1 >= j.limit {
		return journalv2.Version{}, &journalv2.NoSpaceError{Limit: journalv2.LimitVersion, Missing: 1, Unit: journalv2.UnitVersions}
	}
	j.last++
	return journalv2.Version{Counter: j.last}, nil
}

// SetVersionLimit sets the version limit; a mark with a non-zero epoch is
// refused.
func (j *FakeJournal) SetVersionLimit(mark journalv2.Version) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if mark.Epoch != 0 {
		return journalv2.ErrUnknownFormat
	}
	j.limit = mark.Counter
	return nil
}

// Stats reports the loss ring and generation, and each tag's held bytes.
func (j *FakeJournal) Stats() journalv2.Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := journalv2.Stats{Losses: slices.Clone(j.losses), LossGeneration: j.gen, Shares: map[journalv2.ShareTag]journalv2.ShareStats{}}
	for tag, n := range j.held {
		s.Shares[tag] = journalv2.ShareStats{HeldBytes: n}
		s.HeldBytes += n
	}
	return s
}

// Close closes the journal and every handle.
func (j *FakeJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return journalv2.ErrClosed
	}
	j.closed = true
	for _, h := range j.handles {
		h.closed = true
	}
	return nil
}

// Lose drops what file id holds in e as the journal does on a loss: the bytes
// are no longer held, and the loss, at the newest version it dropped, is a
// synced change Since reports and an event in the loss ring; a corrupt loss
// also raises the loss generation, a stale one does not. A loss of nothing
// held records nothing.
func (j *FakeJournal) Lose(id journalv2.FileID, e journalv2.Extent, why journalv2.LossReason) {
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
	f.changes = append(f.changes, journalv2.Change{Kind: journalv2.ChangeLoss, Extent: e, Version: v})
	if why == journalv2.LossCorrupt {
		j.gen++
	}
	j.losses = append(j.losses, journalv2.LossEvent{SeqNum: uint64(len(j.losses)) + 1, Share: f.tag, File: id, Extent: e, Version: v, Reason: why})
}

func (j *FakeJournal) Forget(context.Context, journalv2.ShareTag) error {
	return journalv2.ErrNotImplemented
}
func (j *FakeJournal) Strand(journalv2.ShareTag) error { return journalv2.ErrNotImplemented }
func (j *FakeJournal) Repack(context.Context, int64, journalv2.RepackScope) (int64, error) {
	return 0, journalv2.ErrNotImplemented
}

// cut stops holding [from, to) of f, narrowing or splitting the pieces it
// crosses, and answers the newest version it dropped, ok false when it
// dropped nothing. j.mu is held.
func (j *FakeJournal) cut(f *fakeFile, from, to int64) (newest journalv2.Version, ok bool) {
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
func (h *FakeHandle) file(id journalv2.FileID, claim bool) (*fakeFile, error) {
	if h.closed {
		return nil, journalv2.ErrClosed
	}
	f := h.j.files[id]
	if f == nil {
		f = &fakeFile{tag: h.tag}
		if claim {
			h.j.files[id] = f
		}
	}
	if f.tag != h.tag {
		return nil, &journalv2.OtherShareError{File: id, Tag: h.tag, Owner: f.tag}
	}
	return f, nil
}

// WriteAt holds p at off under a new version, refused by the share's limit
// on held bytes or the version limit.
func (h *FakeHandle) WriteAt(_ context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error) {
	j := h.j
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := h.file(id, true)
	if err != nil {
		return journalv2.Version{}, err
	}
	if len(p) == 0 {
		return journalv2.Version{}, journalv2.ErrZeroLength
	}
	// ponytail: the share's limit counts held bytes, not record bytes, so an
	// overwrite counts its new bytes only once the old are cut. A unit test of
	// the engine's refusal mapping needs only that a refusal names its limit.
	if h.limit > 0 && j.held[h.tag]+int64(len(p)) > h.limit {
		return journalv2.Version{}, &journalv2.NoSpaceError{Limit: journalv2.LimitShare, Missing: j.held[h.tag] + int64(len(p)) - h.limit, Unit: journalv2.UnitBytes}
	}
	v, err := j.draw()
	if err != nil {
		return journalv2.Version{}, err
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
func (h *FakeHandle) ReadAt(_ context.Context, id journalv2.FileID, off int64, p []byte) (n int, missing []journalv2.Extent, held []journalv2.Held, asOf journalv2.Version, err error) {
	j := h.j
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := h.file(id, false)
	if err != nil {
		return 0, nil, nil, journalv2.Version{}, err
	}
	end, at := off+int64(len(p)), off
	for _, pc := range f.pieces {
		lo, hi := max(pc.off, off), min(pc.end(), end)
		if lo >= hi {
			continue
		}
		if lo > at {
			missing = append(missing, journalv2.Extent{Off: at, Len: lo - at})
		}
		n += copy(p[lo-off:hi-off], pc.b[lo-pc.off:])
		held = append(held, journalv2.Held{Extent: journalv2.Extent{Off: lo, Len: hi - lo}, Version: pc.v})
		at = hi
	}
	if at < end {
		missing = append(missing, journalv2.Extent{Off: at, Len: end - at})
	}
	return n, missing, held, f.asOf, nil
}

// Sync answers at once: every operation of the fake is durable on return.
func (h *FakeHandle) Sync(context.Context, ...journalv2.FileID) error {
	h.j.mu.Lock()
	defer h.j.mu.Unlock()
	if h.closed {
		return journalv2.ErrClosed
	}
	return nil
}

// remove runs a removal of kind k over [from, to) under a new version and
// leaves its marker.
func (h *FakeHandle) remove(id journalv2.FileID, k journalv2.ChangeKind, from, to int64) (journalv2.Version, error) {
	j := h.j
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := h.file(id, true)
	if err != nil {
		return journalv2.Version{}, err
	}
	v, err := j.draw()
	if err != nil {
		return journalv2.Version{}, err
	}
	j.cut(f, from, to)
	f.changes = append(f.changes, journalv2.Change{Kind: k, Extent: journalv2.Extent{Off: from, Len: to - from}, Version: v})
	f.asOf = v
	return v, nil
}

// endOfFile is the end offset a truncate or delete marker covers to.
const endOfFile = 1<<63 - 1

// Truncate stops holding every byte at or beyond size.
func (h *FakeHandle) Truncate(_ context.Context, id journalv2.FileID, size int64) (journalv2.Version, error) {
	return h.remove(id, journalv2.ChangeTruncate, size, endOfFile)
}

// Deallocate stops holding e; a zero-length e is refused.
func (h *FakeHandle) Deallocate(_ context.Context, id journalv2.FileID, e journalv2.Extent) (journalv2.Version, error) {
	if e.Len == 0 {
		return journalv2.Version{}, journalv2.ErrZeroLength
	}
	return h.remove(id, journalv2.ChangeDeallocate, e.Off, e.Off+e.Len)
}

// Delete stops holding every byte of the file.
func (h *FakeHandle) Delete(_ context.Context, id journalv2.FileID) (journalv2.Version, error) {
	return h.remove(id, journalv2.ChangeDelete, 0, endOfFile)
}

// Since yields every held piece, unsettled marker and synced loss of the
// file above from, in version order.
func (h *FakeHandle) Since(_ context.Context, id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
	j := h.j
	j.mu.Lock()
	f, err := h.file(id, false)
	var out []journalv2.Change
	if err == nil {
		for _, p := range f.pieces {
			out = append(out, journalv2.Change{Kind: journalv2.ChangeWrite, Extent: journalv2.Extent{Off: p.off, Len: int64(len(p.b))}, Version: p.v, Mtime: p.mtime})
		}
		out = append(out, f.changes...)
	}
	j.mu.Unlock()
	out = slices.DeleteFunc(out, func(c journalv2.Change) bool { return c.Version.Compare(from) <= 0 })
	slices.SortStableFunc(out, func(a, b journalv2.Change) int { return a.Version.Compare(b.Version) })
	return func(yield func(journalv2.Change, error) bool) {
		if err != nil {
			yield(journalv2.Change{}, err)
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
func (h *FakeHandle) Settle(id journalv2.FileID, v journalv2.Version) {
	h.j.mu.Lock()
	defer h.j.mu.Unlock()
	if f := h.j.files[id]; f != nil && f.tag == h.tag {
		f.changes = slices.DeleteFunc(f.changes, func(c journalv2.Change) bool { return c.Version.Compare(v) <= 0 })
	}
}

// Files yields the share's files holding a piece, a marker or a loss.
func (h *FakeHandle) Files() iter.Seq[journalv2.FileID] {
	h.j.mu.Lock()
	var ids []journalv2.FileID
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
		return journalv2.ErrClosed
	}
	h.closed = true
	return nil
}

func (h *FakeHandle) CloneTarget(context.Context, journalv2.FileID, journalv2.CloneSpec) (journalv2.Version, error) {
	return journalv2.Version{}, journalv2.ErrNotImplemented
}

func (h *FakeHandle) Reserve(int64, int) (enginev2.Reservation, error) {
	return nil, journalv2.ErrNotImplemented
}
