package index

import (
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// LiftSuperseded stops retaining what the record of version v superseded in
// id: the superseding record is synced, so the older content has nothing left
// to be kept for. It changes nothing, and takes the file's lock only shared,
// for a v that superseded nothing, and creates no entry for a file that has
// none. Otherwise it takes the lock exclusively.
//
// decision: the lift drops every piece the version superseded, because no
// snapshot hold exists in this package to exempt one. A hold must keep the
// pieces it covers past the sync that superseded them. Withdraw this when
// holds land, and have the lift skip a held piece.
func (ix *Index) LiftSuperseded(id core.FileID, v core.Version) (HeldDelta, error) {
	var any bool
	if !ix.fs.view(id, func(f *file) {
		any = slices.ContainsFunc(f.retained.pieces, bySuperseder(v))
	}) || !any {
		return HeldDelta{}, nil
	}
	return ix.fs.update(id, func(f *file) error {
		f.retained.drop(whole, bySuperseder(v), nil)
		return nil
	})
}

// FailedWindow is what failing a window did to one file.
type FailedWindow struct {
	// Dropped are the held pieces the window's records stopped holding, each
	// with its record and offloaded bit, in the order they were cut.
	Dropped []Piece
	// Covers are the versions of newer content the window's superseded parts
	// were handed to: the caller lifts by each one it knows has synced, since
	// that sync would otherwise have dropped them.
	Covers []core.Version
}

// FailWindow drops exactly the window's records of id and holds again what
// they superseded, so a read serves the last synced write, then drops the
// parts left under no newer content. It takes the file's lock exclusively. A
// file with no entry, or none of whose records are in the window, changes
// nothing.
//
// decision: a part left under no content is dropped, because no snapshot hold
// exists in this package to keep it. A hold must keep the gap it covers.
// Withdraw this when holds land, and return the gaps for the caller to hand
// to the hold.
func (ix *Index) FailWindow(id core.FileID, window []WindowRecord) (FailedWindow, HeldDelta, error) {
	var fw FailedWindow
	if !ix.fs.view(id, func(*file) {}) {
		return fw, HeldDelta{}, nil
	}
	d, err := ix.fs.update(id, func(f *file) error {
		_, fw.Covers, fw.Dropped = f.failWindow(window, nil)
		f.retained.drop(whole, func(r Retained) bool {
			return slices.ContainsFunc(window, func(w WindowRecord) bool { return w.Version == r.By })
		}, nil)
		return nil
	})
	return fw, d, err
}
