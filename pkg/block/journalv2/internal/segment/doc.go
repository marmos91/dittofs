// Package segment manages the segment files records are written to: their
// lifecycle, the pool of spare segments, the append streams, sealing a
// segment with its catalog, and recycling or unlinking a segment once it holds
// nothing live. It also accounts for the storage each segment uses.
//
// Records are only ever appended at the end of a stream. Space freed inside a
// segment is never written again; storage returns to the filesystem one whole
// segment at a time.
package segment
