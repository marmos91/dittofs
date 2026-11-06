package index

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// Transition is how one update moved its file across the line between
// holding a byte whose offloaded bit is unset, held or retained, and not:
// the dirty-file list's membership.
type Transition int8

const (
	Unchanged   Transition = iota // still dirty, or still clean
	BecameDirty                   // first unset bit, held or retained
	BecameClean                   // no held or retained byte with an unset bit left
)

// HeldDelta is the change one index update made to one file's held state:
// the account function's input. It is distinct from Change, Since's yield,
// which has no tag and no byte deltas. The zero HeldDelta means the update
// changed nothing.
//
// decision: the delta is measured from per-file counters the leaf functions
// keep (heldSet.put and cut, retainedSet.add and edit), inside the update's
// lock, rather than built by each operation or computed by a caller from a
// before and an after, because a caller or an operation that adjusts a figure
// itself can forget one. The leaf functions are the only places bytes enter
// or leave a set, so no operation can bypass them. Overturned if a set gains
// a write path that bypasses put, cut, add and edit.
type HeldDelta struct {
	// Tag is the file's owner when the update ended.
	//
	// decision: a share's tag is never 0, which marks the journal's own records. fill claims the file in
	// its own update, so no delta of a fill carries tag 0. Every other update
	// follows a claim of the journal call that made it, so a file that holds
	// bytes has an owner by then; only an update that skips the claim, as a
	// test's direct mutation does, changes bytes of a file nothing owns and
	// returns the zero tag. Overturned by a journal call that changes a file's
	// bytes before claiming it: its delta would put those bytes under tag 0
	// and a later release through the share would drive its sum below zero.
	Tag           core.ShareTag
	File          core.FileID
	HeldBytes     int64 // change in the sum of held extent lengths
	DirtyBytes    int64 // change in held bytes whose offloaded bit is unset
	RetainedBytes int64 // change in the sum of retained extent lengths
	Dirty         Transition
}

// heldTotals is one file's four per-set counters, read under its lock.
// retainedDirty counts toward the file's dirty membership only: DirtyBytes is
// held bytes alone.
type heldTotals struct{ held, dirty, retained, retainedDirty int64 }

func (f *file) totals() heldTotals {
	return heldTotals{f.held.bytes, f.held.dirty, f.retained.bytes, f.retained.dirty}
}

func (t heldTotals) isDirty() bool { return t.dirty > 0 || t.retainedDirty > 0 }

// delta returns the change from was to f's totals now, for id. It returns the
// zero HeldDelta, with no tag and no file, when no figure moved and the
// transition is Unchanged. The caller holds f's lock.
func (f *file) delta(id core.FileID, was heldTotals) HeldDelta {
	now := f.totals()
	d := HeldDelta{
		File:          id,
		HeldBytes:     now.held - was.held,
		DirtyBytes:    now.dirty - was.dirty,
		RetainedBytes: now.retained - was.retained,
	}
	switch {
	case !was.isDirty() && now.isDirty():
		d.Dirty = BecameDirty
	case was.isDirty() && !now.isDirty():
		d.Dirty = BecameClean
	}
	if d.HeldBytes == 0 && d.DirtyBytes == 0 && d.RetainedBytes == 0 && d.Dirty == Unchanged {
		return HeldDelta{}
	}
	d.Tag = f.tag
	return d
}
