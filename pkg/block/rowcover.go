package block

import "fmt"

// FindRowCoveringOffset returns the row whose absolute byte range
// [start, start+DataSize) contains target, together with that start, or nil if
// no row in rows covers it. The walk is O(N) over the per-payload row
// list — acceptable for the FastCDC steady-state (chunks average ~4 MiB
// so even a 4 GiB file produces ~1000 rows).
//
// A row whose ID does not parse cannot be placed, so its range is unknown. That
// is only fatal to reads it might have covered: if some other row covers target,
// that answer is unaffected and is returned. Only when nothing covers target does
// the unplaceable row matter, because then the choice is between reporting a hole
// — which the caller zero-fills, inventing data the file may never have had — and
// admitting the manifest is inconsistent. It admits.
//
// Scoping it this way keeps one bad row from making a whole payload unreadable.
// The alternative, refusing the moment such a row is seen at any offset, would
// take a file that reads correctly apart from one damaged range and make all of
// it unavailable.
//
// When two rows cover target, the greatest start wins, which is what the indexed
// badger lookup returns. Returning whichever row the walk reached first made the
// answer depend on ListFileChunks ordering, so the same read could serve
// different bytes on different backends. Overlap is not hypothetical: a truncate
// narrows a straddling row to the new size, and a later write re-carves from an
// earlier chunk boundary, leaving the narrowed row still claiming bytes the new
// row also covers. The greater start is the newer row, so it is also the one
// holding the bytes the last write put there.
func FindRowCoveringOffset(rows []*FileChunk, target uint64) (*FileChunk, uint64, error) {
	unplaceable := ""
	var hit *FileChunk
	var hitAbs uint64
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, ok := ParseChunkOffset(fb.ID)
		if !ok {
			if unplaceable == "" {
				unplaceable = fb.ID
			}
			continue
		}
		// target-abs is overflow-free because target >= abs is checked first;
		// abs+DataSize would wrap on an absurd offset.
		if target >= abs && target-abs < uint64(fb.DataSize) {
			if hit == nil || abs > hitAbs {
				hit, hitAbs = fb, abs
			}
		}
	}
	if hit == nil && unplaceable != "" {
		return nil, 0, fmt.Errorf("%w: nothing covers offset %d and manifest holds unplaceable row %q",
			ErrManifestInconsistent, target, unplaceable)
	}
	return hit, hitAbs, nil
}
