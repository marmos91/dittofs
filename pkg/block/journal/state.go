package journal

import (
	"context"
	"errors"
)

// The state model: where a written byte range lives, expressed as one value
// instead of a flag pair. Zeros are the correct answer for a hole and
// catastrophic for lost data, and the two are byte-identical — so the
// distinction lives in the type, not in reviewer attention.
//
// The interval struct stores the state as the synced/cold flag pair, which is
// also the on-disk record encoding; state() derives the State from it. Nothing
// holds StateLost today: a residency loss the store cannot make durable is
// refused (the interval keeps its local bytes and the caller gets ErrStateLost),
// so Lost exists as the vocabulary a caller of Extents can be answered with and
// the transition table can pin — the state the flag pair could fall into with
// nowhere to go and render as absent.
type State uint8

const (
	// StateAbsent is a range the journal holds no interval for: never written,
	// or dropped by truncate/delete. Reading zeros is CORRECT. An absent range
	// is no interval at all, so Extents never reports it — it is in the enum
	// because the transition table names it (Fill: Absent → Resident, Seed:
	// Absent → Remote).
	StateAbsent State = iota
	// StateDirty is local-only: the sole copy, never reclaimable, never
	// evictable. An fsynced dirty range survives this device's loss but has no
	// second copy — see Extent.Durable.
	StateDirty
	// StateResident is local AND durable remotely (carved and uploaded, or
	// hydrated from the remote): free to reclaim, because the remote holds the
	// bytes.
	StateResident
	// StateRemote is durable remotely only: the local bytes were evicted or
	// proven bad, so a read fetches instead of serving what is local.
	StateRemote
	// StateLost is written with no copy anywhere: reads MUST fail, never zero.
	// No code path holds it — a loss that cannot be made durable is refused —
	// but it is the state every residency-reducing path must be measured
	// against, and Extents is what would make it observable if one ever did.
	StateLost
)

func (st State) String() string {
	switch st {
	case StateDirty:
		return "dirty"
	case StateResident:
		return "resident"
	case StateRemote:
		return "remote"
	case StateLost:
		return "lost"
	default:
		return "absent"
	}
}

// ErrStateLost reports a residency loss the store could not make durable. The
// transition is refused: the interval keeps its local bytes (state unchanged)
// and the caller gets this sentinel joined with the cause. Demoting without a
// durable marker would let the next reclaim drop the only copy and a restart
// read the range as zeros.
var ErrStateLost = errors.New("journal: residency loss not durable; local bytes kept")

// Extent is one live interval's residency, as Extents reports it. Durable is
// deliberately separate from State: a StateDirty range whose record an fsync
// covered survives this device's loss but still has no second copy, and
// collapsing those two questions is how a durable-size answer over-reports.
type Extent struct {
	Off, Len int64
	State    State
	// Durable reports whether the range survives device loss now: fsynced
	// locally (Version at or below the shard's durable watermark), or durable
	// remotely (StateResident/StateRemote).
	Durable bool
}

// state derives the interval's State from its flag pair — the pair is the
// on-disk encoding, State is the vocabulary.
func (iv interval) state() State {
	if iv.cold {
		return StateRemote
	}
	if iv.synced {
		return StateResident
	}
	return StateDirty
}

// durable reports whether the interval's bytes survive device loss now: durable
// remotely (cold or synced), or fsynced locally (Version at or below the
// shard's durable watermark). localWatermark is sh.syncedVersion read under
// sh.mu. Anything else was only buffered.
func (iv interval) durable(localWatermark uint64) bool {
	return iv.cold || iv.synced || iv.version <= localWatermark
}

// Extents returns one Extent per live interval of id, ascending and
// non-overlapping — the state-truth primitive every residency query is
// expressed over. A range the journal holds no interval for (never written, or
// dropped by truncate/delete) appears as nothing: that absence is StateAbsent
// semantics, and reading it as zeros is correct.
//
// Adjacent intervals are not merged: two touches of the same bytes can hold
// different states, and merging would trade state truth for slice compactness.
// Derived answers that want runs (DataExtents) merge over this.
func (s *Store) Extents(ctx context.Context, id FileID) ([]Extent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed.Load() {
		return nil, errClosed
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil || len(fi.ivs) == 0 {
		return nil, nil
	}
	wm := sh.syncedVersion.Load()
	out := make([]Extent, 0, len(fi.ivs))
	for _, iv := range fi.ivs {
		out = append(out, Extent{
			Off:     iv.fileOff,
			Len:     iv.length,
			State:   iv.state(),
			Durable: iv.durable(wm),
		})
	}
	return out, nil
}

// demote is the one residency-loss chokepoint: it persists the cold markers to
// stable storage and only then runs flip, which takes the intervals
// resident→remote. appendCold fsyncs before returning, so by the time flip
// runs the markers survive a crash — without that order, a reclaim could drop
// the only local copy and a restart would read the range as zeros. Every path
// that marks a live interval cold (invalidate, eviction) goes through here;
// seeding cold and recovery replay are not losses and do not.
//
// A failed append returns ErrStateLost joined with the cause and runs nothing:
// the intervals stay resident, the caller's read fails closed rather than
// proceeding on a demotion the store cannot keep.
func (s *Store) demote(entries []coldEntry, flip func()) error {
	if err := s.appendCold(entries); err != nil {
		return errors.Join(ErrStateLost, err)
	}
	if flip != nil {
		flip()
	}
	return nil
}
