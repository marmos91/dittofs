package journal

// Evict frees whole sealed segments under storage pressure (not yet
// implemented). The victim is the coldest sealed segment by lastAccess whose
// records are all synced; on eviction its .seg/.idx are unlinked and every
// interval-tree entry pointing into it is replaced with a cold marker so a
// later read falls to a remote fetch rather than reporting a false hole. If
// pressure persists with no evictable segment (dirty bytes pinned), the write
// path backpressures and UnsyncedBytes surfaces the stall.

// EvictResult reports what an eviction pass reclaimed.
type EvictResult struct {
	SegmentsEvicted int
	BytesFreed      int64
}
