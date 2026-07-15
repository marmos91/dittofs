package journal

// Garbage collection / repack (not yet implemented).
//
// deadBytes on a segmentMeta increments whenever an interval-tree node is
// overwritten by a newer version or its file is tombstoned. The victim is the
// segment with the highest dead/total ratio; GCDeadRatioForce forces an
// immediate repack regardless of scheduling so space amplification stays
// bounded.
//
// Repack copies a segment's live records into a repack-target segment,
// preserving each record's Version and synced flag (never reissued, so
// newest-wins survives relocation), fsyncs the destination and the directory,
// updates the interval index, and only then unlinks the source. A crash before
// the unlink leaves a harmless orphan that the recovery sweep reclaims. The
// per-segment membership filter is rebuilt on every repack, never carried.
