package fs

// Per-file logIndex (Direction 1 redesign — see
// .planning/proposals/2026-05-22-logindex-rollup-redesign.md).
//
// The existing interval tree (interval_tree.go) is the oracle for which
// FILE-OFFSET regions are dirty and stable. The logIndex is the oracle for
// where in the on-disk LOG each AppendWrite's framed record lives. The two
// domains are decoupled so the rollup can find every record covering a
// stable file-offset interval — independent of arrival order — by
// translating a file-offset range into a set of log positions and pread-ing
// them directly.
//
// The previous design assumed records on the per-file log were stored in
// file-offset order; macOS NFSv3 (and any parallel-write client) violates
// that — AppendWrite records land in arrival order. The linear log scan
// short-circuited on the first in-range frame and silently produced
// recs=0 rollups for parallel-write workloads.
//
// Concurrency: logIndex is NOT internally synchronized. All callers hold
// the per-file `mu` (see appendwrite.go::AppendWrite and rollup.go::
// rollupFile). The invariant matches the rest of the per-payload state
// (interval tree, truncation boundary).

// logEntry is one record's position in the log + the file-offset extent it
// covers. The entry is immutable once appended; consumption is tracked
// separately via the consumed map on logIndex.
type logEntry struct {
	logPos     uint64 // byte offset of the frame's first byte in the log file
	fileOff    uint64 // record's file_offset field (16-byte frame header)
	payloadLen uint32 // payload byte count (excludes frame overhead)
}

// fileEnd returns the exclusive file-offset upper bound for this record.
func (e logEntry) fileEnd() uint64 {
	return e.fileOff + uint64(e.payloadLen)
}

// logIndex maintains per-file log-position bookkeeping. Entries are
// appended in arrival order (== logPos order, since AppendWrite advances
// lf.eofPos monotonically under per-file mu). The consumed map records
// which logPos values have been rolled up into CAS; compactionFence is
// the largest logPos such that every entry with logPos <= fence has been
// consumed — i.e. the on-disk byte prefix that is now eligible for
// physical compaction (deferred to v0.17+).
//
// rollup_offset on disk continues to mean "consumed up to this log byte
// offset"; conceptually it now records compactionFence rather than a
// running scan cursor.
type logIndex struct {
	entries         []logEntry
	consumed        map[uint64]struct{} // key = logEntry.logPos
	compactionFence uint64
}

// newLogIndex constructs an empty logIndex. compactionFence starts at
// logHeaderSize because the header itself is never a candidate for
// compaction.
func newLogIndex() *logIndex {
	return &logIndex{
		entries:         nil,
		consumed:        make(map[uint64]struct{}),
		compactionFence: logHeaderSize,
	}
}

// Append records a new entry. Caller MUST hold the per-file mu and MUST
// pass a logPos strictly greater than the most recent entry's logPos
// (eofPos advances monotonically). Append does not validate ordering —
// the contract is on the caller.
func (idx *logIndex) Append(logPos uint64, fileOff uint64, payloadLen uint32) {
	idx.entries = append(idx.entries, logEntry{
		logPos:     logPos,
		fileOff:    fileOff,
		payloadLen: payloadLen,
	})
}

// EntriesForInterval returns every entry whose file-offset extent
// intersects [fileOff, fileOff+length). Order of returned entries
// preserves logPos order (== arrival order) so the rollup can apply
// FastCDC over the canonical stream while still walking out-of-order
// arrivals.
//
// Length == 0 returns nil.
func (idx *logIndex) EntriesForInterval(fileOff uint64, length uint64) []logEntry {
	if length == 0 {
		return nil
	}
	end := fileOff + length
	out := make([]logEntry, 0, 4)
	for _, e := range idx.entries {
		if e.fileEnd() <= fileOff {
			continue
		}
		if e.fileOff >= end {
			continue
		}
		out = append(out, e)
	}
	return out
}

// MarkConsumed records that the entry at logPos has been rolled into
// CAS. Idempotent — repeat calls for the same logPos are a no-op.
func (idx *logIndex) MarkConsumed(logPos uint64) {
	idx.consumed[logPos] = struct{}{}
}

// AdvanceFence walks consumed entries in logPos order from the current
// fence forward, advancing compactionFence past every entry that is
// consumed and contiguous. Stops at the first non-consumed entry. The
// new fence is returned. An out-of-order consumed entry (a "hole" left
// by an unconsumed predecessor) does NOT advance the fence — it stays
// in `consumed` until the predecessor is consumed too.
//
// This is the operation the rollup invokes after persisting a CAS
// commit; the returned fence is what advanceRollupOffset eventually
// writes to disk.
func (idx *logIndex) AdvanceFence() uint64 {
	for _, e := range idx.entries {
		if e.logPos < idx.compactionFence {
			continue
		}
		if _, ok := idx.consumed[e.logPos]; !ok {
			break
		}
		// Fence advances past the consumed entry's full record extent.
		idx.compactionFence = e.logPos + uint64(recordFrameOverhead) + uint64(e.payloadLen)
	}
	return idx.compactionFence
}

// Fence returns the current compactionFence without mutation.
func (idx *logIndex) Fence() uint64 {
	return idx.compactionFence
}

// SetFence overrides the compaction fence. Intended for recovery, which
// reconstructs the logIndex from records at and after the persisted
// rollup_offset; any record below that offset was already consumed by a
// pre-boot rollup pass and must not be replayed. Setting the fence to
// the boot-time rollup_offset preserves that invariant for the
// post-boot AdvanceFence walks. MUST be called before any MarkConsumed
// /AdvanceFence calls on the same logIndex.
func (idx *logIndex) SetFence(fence uint64) {
	idx.compactionFence = fence
}

// Len returns the total number of indexed entries (consumed + unconsumed).
// Test/debug surface.
func (idx *logIndex) Len() int {
	return len(idx.entries)
}
