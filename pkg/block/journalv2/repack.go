package journalv2

// RepackScope aims a repack pass: at the segments holding one share's records,
// at a set of segments, or, when zero, at every segment.
type RepackScope struct {
	Share    ShareTag    // zero: any share
	Segments []SegmentID // empty: any segment
}

// Repack copies the still-live records of the best candidate segments within
// scope into another segment, repoints the placement index and retires the
// originals, copying at most budget bytes. It returns freed, the storage its
// retirements returned to the filesystem or to the spare pool, once they are
// durable; the journal's used bytes fall at the same moment, never earlier.
//
// Repack preserves every held and retained extent, the bytes they produce and
// their offloaded bits, and discards nothing. It decides which segments to
// repack without reading them, taking the ones with the least live payload
// first, and never selects a segment a running offer pins.
func (j *Journal) Repack(budget int64, scope RepackScope) (freed int64, err error) {
	return 0, ErrNotImplemented
}
