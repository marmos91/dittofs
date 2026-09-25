package engine

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// DataExtents returns the sorted, non-overlapping byte ranges [start, end)
// within [0, fileSize) that hold data across ALL tiers the engine reads from:
// the local append log + in-memory buffer (via local.DataExtents) UNION the
// persisted CAS FileChunk manifest. This is the same union readAtInternal
// reconstructs, expressed as a hole map, so SEEK and READ agree on where the
// data is.
//
// NFSv4.2 SEEK and READ_PLUS use it instead of deriving holes from the
// persisted CAS block list alone: that list is empty/partial for
// written-but-not-yet-rolled-up data, so a CAS-only map reports a hole where
// data exists — which RFC 7862 forbids (a sparse-copy client skips the real
// data, silently losing it). #1481.
//
// The map may over-report data but must never under-report it. Both callers
// ignore an error from here and fall back to the CAS block list, which cannot
// see an unplaceable row either, so a row whose range is unknown widens the map
// to the whole file rather than failing.
//
// The counterpart surface is journal.LocalStore.ReadAt's ReadState.Hole, in
// pkg/block/journal/localstore.go, which obeys the same underlying rule through the
// opposite token: there a range the tier cannot classify reports Hole true,
// never false. The polarity differs because the terminal answer differs — here
// "hole" ends the inquiry (a sparse-copy client skips the range), while there
// "not a hole" ends it (the engine serves the zero fill without hydrating).
//
// The shared rule is that uncertainty resolves toward the answer forcing
// another question, never toward the one that ends the inquiry. The token that
// satisfies it is opposite on each surface BY DESIGN, so do not reconcile the
// two by making them agree on a token — that edit is the bug in whichever
// direction it is made.
func (bs *Store) DataExtents(ctx context.Context, payloadID string, fileSize uint64) ([][2]uint64, error) {
	// decision: a closed store returns the error rather than widening to the
	// whole file, unlike the two unknown-range cases below. Those widen because
	// the store is live and measuring and met one range it cannot place, so the
	// whole-file answer is an over-report of a real measurement. A closed store
	// measured nothing: its tiers are torn down, so "the whole file holds data"
	// would be invented rather than over-reported. It is also the only case
	// where the caller's fallback is not the narrower view — with the engine
	// gone, the metadata block list is the best remaining source, and widening
	// would replace it with a fabrication. Withdraw this if a caller ever acts
	// on the extents without a READ that re-checks liveness.
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
	if fileSize == 0 {
		return nil, nil
	}

	// (a) Bytes the local journal tier knows (dirty or evicted/cold).
	ext, err := bs.local.DataExtents(ctx, journal.FileID(payloadID), int64(fileSize))
	if err != nil {
		// The tier holds exactly the bytes this map cannot get from anywhere
		// else — written but not yet rolled up — so a failure here leaves their
		// ranges unknown, not empty. Returning the error reads as an under-
		// report: both callers drop it and answer from the CAS block list
		// alone, which is the narrower view this function exists to widen, so
		// the range would come back a hole over data that is really there.
		// Report the whole file as data instead, for the same reason the
		// unplaceable row below does — the READ the client is then forced to
		// issue surfaces the fault rather than inventing zeros.
		//
		// The error is logged rather than swallowed: the answer must not
		// narrow, but an I/O fault that only ever widened a hole map would
		// otherwise leave no trace.
		logger.Error("local tier could not report data extents; widening the hole map to the "+
			"whole file so a sparse-copy client cannot skip bytes it holds",
			"payload", payloadID, "error", err)
		return [][2]uint64{{0, fileSize}}, nil
	}

	// (b) Persisted CAS chunks (post-rollup bytes). Optional store in tests.
	if bs.fileChunkStore != nil {
		rows, lerr := bs.fileChunkStore.ListFileChunks(ctx, payloadID)
		if lerr != nil {
			// The manifest could not be enumerated in full — a row whose value
			// will not decode is one way to get here — so the ranges those rows
			// would have claimed are unknown. Same class as the unplaceable row
			// below, and it takes the same widening rather than an error, which
			// the callers would turn into the narrower CAS-only map.
			return [][2]uint64{{0, fileSize}}, nil
		}
		for _, fb := range rows {
			if fb == nil {
				continue
			}
			off, ok := block.ParseChunkOffset(fb.ID)
			if !ok {
				// The row's range is unknown, so no extent can stand for it, and
				// leaving it out would call its bytes hole. Report the whole file
				// as data instead: over-reporting is the RFC-safe direction, and it
				// keeps a sparse-copy client from skipping the range on SEEK alone —
				// the READ it is then forced to issue refuses with
				// ErrManifestInconsistent rather than inventing zeros.
				return [][2]uint64{{0, fileSize}}, nil
			}
			if fb.Hash.IsZero() {
				continue // pending/incomplete chunk — no committed bytes yet
			}
			if off >= fileSize {
				continue
			}
			end := off + uint64(fb.DataSize)
			if end > fileSize {
				end = fileSize
			}
			if end <= off {
				continue
			}
			ext = append(ext, [2]uint64{off, end})
		}
	}

	return block.CoalesceExtents(ext), nil
}
