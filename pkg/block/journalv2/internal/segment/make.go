package segment

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/format"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// Maker makes spares: whole segments at the cap, zeroed end to end, durable
// under their final name, accounted at the cap in the ledger before any
// storage is touched. A spare is made whole or not at all — a crash leaves no
// `<id>.seg`, or a whole one — and whatever a failed making leaves behind is
// unlinked before the reservation it holds is returned.
type Maker struct {
	fs  storage.FS
	set *Set
	max int64
	log *slog.Logger

	// decision: last is the id allocator, a capacity-domain fact held in one
	// atomic: no lock of its own and none taken while an id is drawn, so ids
	// ascend in draw order across every caller. An id a make drew and then
	// lost — refused, or failed — is never drawn again: reuse would have a
	// crash-durable leftover and a later spare meet under one name. Gaps are
	// allowed; no rule asks the ids to be dense. Overturn only if one does —
	// then this becomes a ledger-owned range, under its lock.
	last atomic.Uint64 // the largest id drawn or named by the directory
}

// NewMaker returns a Maker drawing spares over fs into the ledger set,
// reserving each spare's cap against the journal maximum max, and drawing its
// first id above last. A nil fs or set, or a maximum that is not a size, is a
// caller bug and panics, as NewSet panics on its cap. A nil logger keeps the
// Maker silent.
func NewMaker(fsys storage.FS, set *Set, max int64, last core.SegmentID, log *slog.Logger) *Maker {
	if fsys == nil {
		panic("segment: a Maker needs its storage")
	}
	if set == nil {
		panic("segment: a Maker needs its ledger")
	}
	if max <= 0 {
		panic(fmt.Sprintf("segment: the journal maximum %d is not a size spares can be reserved against", max))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Maker{fs: fsys, set: set, max: max, log: log}
	m.last.Store(uint64(last))
	return m
}

// HighestID returns the largest id over the names of the segments and spares
// a journal directory holds, or 0 over none: the id a fresh Maker resumes
// drawing above, so the first spare a restart makes is above every name the
// directory already holds. Names the format refuses count for nothing,
// however large a number they carry, and so do the format's own files.
func HighestID(names []string) core.SegmentID {
	var top core.SegmentID
	for _, name := range names {
		switch kind, id := format.ClassifyName(name); kind {
		case format.NameSegment, format.NameSpare:
			if id > top {
				top = id
			}
		}
	}
	return top
}

// drawID returns the next id. It is Maker's own, but the recycle (a later
// step of this epic) draws from the same counter.
func (m *Maker) drawID() core.SegmentID {
	return core.SegmentID(m.last.Add(1))
}

// Make makes one spare and returns its id. On every failure it returns an id
// of 0 and an error naming the id and the step that failed, wrapping the
// step's own error — so errors.Is still holds for the caller reading its
// class out of it — and the spare is never taken: a failed make leaves no
// spare in the ledger, or one its retirement, not a stream, unlinks.
func (m *Maker) Make() (core.SegmentID, error) {
	// The reservation sits before any storage is touched, so a refusal owns
	// no I/O at all, and it is the ledger's own critical section that decides
	// against the maximum, so two makes near it never both pass.
	id := m.drawID()
	if err := m.set.AddSpare(id, m.max); err != nil {
		return 0, fmt.Errorf("segment: making %d: reserve: %w", id, err)
	}
	if ferr := m.make(id); ferr.err != nil {
		return 0, fmt.Errorf("segment: making %d: %w", id, ferr)
	}
	return id, nil
}

// make performs one spare's making, with the reservation already held. Its
// steps are ordered so the spare appears under its final name only once it
// is whole and durable: create under the temporary name, allocate the whole
// cap, sync, close, rename into place, sync the directory. A failure at any
// of them is cleaned up by fail; the step's own error is what the caller
// sees, named with the step that returned it.
func (m *Maker) make(id core.SegmentID) stepFailure {
	spare := format.SpareName(id)
	name := format.SegmentName(id)

	f, err := m.fs.Create(spare)
	if err == nil {
		// decision: Make writes no zeros of its own. The platform's Allocate
		// zero-fills where RFC 28 Appendix A requires it (on Linux, because
		// fallocate alone leaves unwritten extents whose conversion would
		// force a filesystem-journal commit on every later sync, so the fill
		// buys RFC 1 §5.2's one flush per Sync) and only preallocates and
		// sets the size on macOS; the fill is paid only when the pool grows,
		// since a recycled segment needs none. Withdraw the fill if J1's
		// Sync-per-write row, run with and without it, shows the conversion
		// costing less, as the RFC says — the wording platform/alloc_linux.go
		// carries for its own call.
		if aerr := f.Allocate(0, m.set.cap); aerr == nil {
			// The sync comes before the close, so no descriptor that wrote
			// into the file is closed over unsynced writes.
			if serr := f.Sync(); serr != nil {
				err = serr
			}
		} else {
			err = aerr
		}
		if cerr := f.Close(); cerr != nil {
			// The close releases the handle whether or not it succeeds; its
			// error is a failed step too, joined with the one before it.
			err = errors.Join(err, cerr)
		}
	}
	if err != nil {
		return m.fail(id, "create", err)
	}

	if err := m.fs.Rename(spare, name); err != nil {
		return m.fail(id, "rename", err)
	}
	if err := m.fs.SyncDir(); err != nil {
		return m.fail(id, "sync_dir", err)
	}
	return stepFailure{}
}

// stepFailure is a failed make's step: the name of the step that failed and
// everything the failure joined to its own error.
type stepFailure struct {
	step string
	err  error
}

// Error names the step beside everything the failure joined to it.
func (s stepFailure) Error() string {
	return fmt.Sprintf("segment: making: %s: %v", s.step, s.err)
}

// Unwrap hands the joined chain to errors.Is and errors.As, so the class of
// the step's own error — ENOSPC, EIO — survives the naming.
func (s stepFailure) Unwrap() error { return s.err }

// fail cleans up after a failed step of one spare's making. A spare whose
// making fails is never taken, so whatever the attempt left is unlinked under
// both its names, and only after the unlink has happened is the reservation
// returned. A leftover that cannot be unlinked stays in the set, End-ed and
// accounted at its cap, until a retirement unlinks it.
//
// decision: a create that found the name existing means the Maker drew an id
// the directory already held — a caller bug of the seed, or a file nothing of
// the journal owns. Removing that name could unlink a live segment, so only
// the create step's refusal of an existing name removes nothing: the
// reservation is returned and the directory is left exactly as the Maker
// found it. Any other step has created the name itself, so unlinking both
// names is safe.
func (m *Maker) fail(id core.SegmentID, step string, stepErr error) stepFailure {
	spare := format.SpareName(id)
	name := format.SegmentName(id)

	if step == "create" && errors.Is(stepErr, fs.ErrExist) {
		// decision: a DropSpare that refuses a reservation the ledger does
		// not hold is a Make/Set split this step cannot heal; the accounting
		// rules the ledger upholds for it, so the refusal joins the step's
		// own error and the caller hears both.
		_, remErr := m.set.DropSpare(id)
		return stepFailure{step: step, err: errors.Join(stepErr, remErr)}
	}

	// Both names, whatever the failing step was: a rename that failed may
	// have either left the file where it was or moved it, and only storage
	// knows which. A remove that misses its name is as good as a removal.
	var remErr error
	removed := true
	for _, remove := range []string{spare, name} {
		if err := m.fs.Remove(remove); err != nil && !errors.Is(err, fs.ErrNotExist) {
			removed = false
			remErr = errors.Join(remErr, err)
		}
	}

	if removed {
		// Only after both removals have returned is the reservation given
		// back: the footprint never runs below the storage in use.
		_, err := m.set.DropSpare(id)
		return stepFailure{step: step, err: errors.Join(stepErr, remErr, err)}
	}

	// A leftover that could not be unlinked stays in the set, in the segment
	// half (Take moves the spare there, holding the cap) marked Ended, until
	// a retirement unlinks it under whichever name it finds.
	if err := m.set.Take(id); err != nil {
		remErr = errors.Join(remErr, err)
	}
	if err := m.set.End(id); err != nil {
		remErr = errors.Join(remErr, err)
	}
	m.log.Warn("segment: a failed make left a spare the unlink could not remove",
		"id", uint64(id), "spare", spare, "segment", name, "err", remErr)

	// decision: no SyncDir after the cleanup. No record depends on the
	// unlink's durability; a crash before it leaves a `.spare.tmp`, which
	// open unlinks, or a `.seg` that is whole (a `.seg` exists only after a
	// successful file sync), which open may re-adopt. Adding one here would
	// pay a second directory sync on the failure path of every step, and
	// hold Make's return on the durability of a removal the ledger has
	// already returned.
	return stepFailure{step: step, err: errors.Join(stepErr, remErr)}
}
