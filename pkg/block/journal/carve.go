package journal

// Carve packs dirty ranges into fixed-size remote blocks. The flow (not yet
// implemented): snapshot covering dirty intervals for files whose dirty-byte
// count crosses CarveBlockSize or whose oldest dirty record exceeds
// CarveMaxAge; stream those bytes in file-offset order through
// FastCDC -> BLAKE3 -> per-share dedup; PutBlock the novel chunks; atomically
// commit the block records; then flip each carved record's synced flag in place
// with a one-byte pwrite. Flipping strictly after the commit means a crash in
// between costs one harmless re-carve, never data loss.

// CarveOptions selects what an explicit Carve targets.
type CarveOptions struct {
	// FileID, if set, carves only that file; empty means every eligible file.
	FileID FileID
	// Force carves eligible files regardless of the age/size batching gates.
	Force bool
}

// CarveResult reports what a carve pass moved to the remote store.
type CarveResult struct {
	BlocksWritten int
	BytesCarved   int64
}
