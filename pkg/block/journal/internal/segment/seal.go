package segment

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// The seal writer's errors.
var (
	// ErrSegmentEnded is matched by the error of a seal whose segment was
	// ended rather than sealed: a record write or a sync of the segment
	// failed before or during the seal, so the segment takes no append and
	// no seal while the journal runs, and the successor the roll would take
	// names none.
	ErrSegmentEnded = errors.New("segment: the segment is ended, never sealed")

	// ErrBadCatalog is the refusal of a seal handed entries that do not
	// describe the segment up to the marker: not strictly ascending by
	// RecordOffset, a last record that does not end at the marker, or no
	// room for the marker, the footer and the trailer before the cap.
	// Nothing was written and no counter moved.
	ErrBadCatalog = errors.New("segment: the catalog does not describe the segment up to the marker")

	// ErrNoSegment is the refusal of a seal on a stream that has not taken
	// a segment: there is nothing to close.
	ErrNoSegment = errors.New("segment: the stream has no segment to seal")

	// ErrLedgerDisagrees is the refusal of a ledger call that runs past
	// the seal's second step, with the marker, footer and trailer already
	// durable: the segment is sealed on disk but the ledger would not
	// carry it. It never claims the segment is ended — the disk says
	// otherwise — and it names a caller whose own sequencing should not
	// have reached it.
	ErrLedgerDisagrees = errors.New("segment: the ledger refused a durable seal")
)

// SealResult is what a seal left.
type SealResult struct {
	// Marker is the seal marker's own offset: the end of the last record.
	Marker int64
	// Size is the size the segment is accounted at now: the cap, or the
	// trailer's end after a durable truncation.
	Size int64
	// TailErr is the failed truncation or its failed sync of an early seal:
	// the segment is sealed — sealed and accounted at its cap — but its
	// free tail stayed. Nil on every full seal and every clean early one.
	TailErr error
}

// WriteSeal seals segment id, which no stream appends to anymore, at marker,
// with entries as its catalog, and truncates the free tail when early. It is
// the one seal writer: the stream calls it on the segment it drained, and
// recovery calls it on the entries it scanned.
//
// The steps, in their order, each made durable before the next begins:
// first the records are made durable — a catalog made durable over a record
// that never reached the device would report that torn record as corruption;
// then the seal marker, the footer and the trailer are written and made
// durable — a full seal puts the trailer at the cap's last 32 bytes, an early
// one right after the footer; then, for an early seal alone, the file is
// truncated past the trailer and that is made durable.
//
// decision: no footer is ever written for a segment sealed by a previous
// process, though the format permits it, because a segment sealed before this
// process began is scanned instead on the next open — a footer written here
// would have to re-verify the whole segment first, which is the scan it would
// spare. The exemption holds while scanning such a tail is cheap; a time to
// first read dominated by those scans overturns it.
func WriteSeal(set *Set, ref Ref, identity core.JournalID, id core.SegmentID,
	marker int64, entries []format.CatalogEntry, early bool) (SealResult, error) {

	// The validation, no I/O: a refusal makes no seam call and no ledger
	// change, so nothing about the segment moved.
	if err := checkSeal(set, id, marker, entries); err != nil {
		return SealResult{}, err
	}

	// Step 1: every record durable.
	if err := ref.Sync(); err != nil {
		if unerr := set.End(id); unerr != nil {
			err = errors.Join(err, unerr)
		}
		return SealResult{}, fmt.Errorf("%w: making the records durable: %w", ErrSegmentEnded, err)
	}

	// Step 2: the marker, the footer and the trailer, written then durable.
	// markerAndFooter holds the marker followed by the entries, the bytes
	// that run from the marker's own offset; footer names its entry bytes
	// alone, for the trailer's CRC.
	footerOff := marker + format.SealMarkerSize
	markerAndFooter := format.AppendSealMarker(nil, identity, format.SealMarker{Segment: id, Offset: marker})
	for _, e := range entries {
		markerAndFooter = format.AppendCatalogEntry(markerAndFooter, e)
	}
	footer := markerAndFooter[format.SealMarkerSize:]

	if err := writeSealSteps(ref, markerAndFooter, footer, marker, footerOff, early, set.cap); err != nil {
		if unerr := set.End(id); unerr != nil {
			err = errors.Join(err, unerr)
		}
		return SealResult{}, fmt.Errorf("%w: writing the marker, the footer and the trailer: %w", ErrSegmentEnded, err)
	}
	if err := ref.Sync(); err != nil {
		if unerr := set.End(id); unerr != nil {
			err = errors.Join(err, unerr)
		}
		return SealResult{}, fmt.Errorf("%w: making the marker, the footer and the trailer durable: %w", ErrSegmentEnded, err)
	}

	// The seal is complete: the segment is sealed in the ledger from here,
	// and the steps below are already refusals a second seal would answer.
	// A refusal here — the ledger refusing a transition its own caller
	// sequence should never ask for — happens with the seal already
	// durable on disk, so it is not an ended segment and never carries
	// ErrSegmentEnded: the caller is handed the refusal itself, which is
	// the only honest word for a state the stream and the ledger now
	// disagree on.
	if err := set.Seal(id, !early); err != nil {
		// The seal is durable on disk whatever the ledger said: the
		// result carries the facts of what stands there, so a caller
		// that wants them — the marker it placed, the cap the file is
		// accounted at — has them beside the refusal. No test reaches
		// this return: it needs a ledger that refuses a transition the
		// stream's own sequencing only ever makes valid, which is a
		// Set double the real-storage rule forbids.
		return SealResult{Marker: marker, Size: set.cap}, fmt.Errorf("%w: %w", ErrLedgerDisagrees, err)
	}

	if !early {
		return SealResult{Marker: marker, Size: set.cap}, nil
	}

	// Step 3: the free tail, cut and made durable.
	size := footerOff + int64(len(footer)) + format.TrailerSize
	//
	// decision: a failed truncation, or a failed sync of one, returns a nil
	// error with TailErr set, because "the seal is complete after the second
	// step" and "a failed truncation sync does not end the segment, whose
	// marker and catalog are durable already, and leaves it accounted at its
	// cap" — the successor the roll may take is not to wait on a cut the
	// bytes the seal names are free of. Overturned by a caller that must
	// refuse the successor until the tail is returned. The Shrink below is
	// the same rule's ledger half: it runs only once the cut is durable,
	// and its refusal is a sealed segment's too, not an ended one's.
	tailErr := func() error {
		if err := ref.Truncate(size); err != nil {
			return err
		}
		if err := ref.Sync(); err != nil {
			return err
		}
		if err := set.Shrink(id, size); err != nil {
			return fmt.Errorf("%w: %w", ErrLedgerDisagrees, err)
		}
		return nil
	}()
	if tailErr != nil {
		return SealResult{Marker: marker, Size: set.cap, TailErr: tailErr}, nil
	}
	return SealResult{Marker: marker, Size: size}, nil
}

// checkSeal validates the seal's facts against the ledger and the cap: the
// segment is Active, every entry is one the encoder takes (a record slot on
// the grid within the 32-bit column, a known kind, a zero epoch, a
// non-negative extent), the entries ascend strictly by RecordOffset, the
// last record ends exactly at the marker — as the footer's reader computes
// it, which also puts the marker itself on a record slot, since with the
// entries on the grid their end is too — and the marker, the footer and
// the trailer fit before the cap. A violation answers ErrBadCatalog, or
// the ledger's own refusal, and nothing else runs: no seam call, no ledger
// change, and no encoder panic on entries the writer was handed as data.
func checkSeal(set *Set, id core.SegmentID, marker int64, entries []format.CatalogEntry) error {
	if err := sealableSegment(set, id); err != nil {
		return err
	}
	prev := int64(0)
	for i, e := range entries {
		if e.RecordOffset < format.SegmentHeaderSize {
			return fmt.Errorf("%w: entry %d's record offset %d is below the first record slot %d", ErrBadCatalog, i, e.RecordOffset, format.SegmentHeaderSize)
		}
		if e.RecordOffset%8 != 0 {
			return fmt.Errorf("%w: entry %d's record offset %d is not a multiple of 8", ErrBadCatalog, i, e.RecordOffset)
		}
		if e.RecordOffset > math.MaxUint32 {
			return fmt.Errorf("%w: entry %d's record offset %d is above MaxUint32", ErrBadCatalog, i, e.RecordOffset)
		}
		// The kinds table's own range, read from the constants it
		// defines: the encoder refuses anything outside it.
		if e.Kind < format.KindWrite || e.Kind > format.KindForget {
			return fmt.Errorf("%w: entry %d's kind %d is not one this format encodes", ErrBadCatalog, i, uint8(e.Kind))
		}
		if e.Version.Epoch != 0 {
			return fmt.Errorf("%w: entry %d carries a version epoch %d, want 0", ErrBadCatalog, i, e.Version.Epoch)
		}
		if e.Extent.Off < 0 || e.Extent.Len < 0 || e.Extent.Off+e.Extent.Len < 0 {
			return fmt.Errorf("%w: entry %d carries a negative extent %+v", ErrBadCatalog, i, e.Extent)
		}
		if e.RecordOffset <= prev {
			return fmt.Errorf("%w: entries do not ascend strictly by record offset: %d after %d", ErrBadCatalog, e.RecordOffset, prev)
		}
		prev = e.RecordOffset
	}
	end := int64(format.SegmentHeaderSize)
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		whole := format.RecordHeader{Kind: last.Kind, Extent: core.Extent{Len: last.Extent.Len}}.Size()
		end = last.RecordOffset + whole
	}
	if end != marker {
		return fmt.Errorf("%w: the last record ends at %d, want the marker %d", ErrBadCatalog, end, marker)
	}
	need := marker + format.SealMarkerSize + int64(len(entries))*format.CatalogEntrySize + format.TrailerSize
	if need > set.cap {
		return fmt.Errorf("%w: the marker, the footer and the trailer need %d bytes, past the cap %d", ErrBadCatalog, need, set.cap)
	}
	return nil
}

// sealableSegment reports whether the set holds id as Active, the only state
// a seal closes. Its refusal is the set's own, so a caller can tell an ended
// or sealed segment from a bad catalog.
func sealableSegment(set *Set, id core.SegmentID) error {
	state, err := set.State(id)
	if err != nil {
		return err
	}
	if state != Active {
		return ErrWrongState
	}
	return nil
}

// writeSealSteps writes the marker, the footer and the trailer at the
// marker's own offset. An early seal writes them as one buffer, the trailer
// right after the footer; a full seal writes the marker and the footer at the
// marker and the trailer at the cap's last 32 bytes, as one write when the
// two ranges meet.
// A short write with a nil error is io.ErrShortWrite; any other failure is
// the write's own.
func writeSealSteps(ref Ref, markerAndFooter, footer []byte, marker, footerOff int64, early bool, cap int64) error {
	if early {
		buf := markerAndFooter
		buf = format.AppendTrailer(buf, footerOff, footer)
		return writeWhole(ref, buf, marker)
	}
	trailer := format.AppendTrailer(nil, footerOff, footer)
	if footerOff+int64(len(footer)) == cap-format.TrailerSize {
		return writeWhole(ref, append(markerAndFooter, trailer...), marker)
	}
	if err := writeWhole(ref, markerAndFooter, marker); err != nil {
		return err
	}
	return writeWhole(ref, trailer, cap-format.TrailerSize)
}

// writeWhole writes p at off, whole or not at all: a short write with a nil
// error counts as io.ErrShortWrite, and any error is returned as itself.
func writeWhole(ref Ref, p []byte, off int64) error {
	n, err := ref.WriteAt(p, off)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// sortSealEntries sorts a seal's entries by RecordOffset, the order the
// footer's reader requires. The stream keeps its entries in completion
// order, so sorting is its own step before it hands them to the writer.
func sortSealEntries(entries []format.CatalogEntry) {
	slices.SortFunc(entries, func(a, b format.CatalogEntry) int {
		return cmp.Compare(a.RecordOffset, b.RecordOffset)
	})
}
