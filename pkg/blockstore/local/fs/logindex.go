package fs

import "sync"

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
// Concurrency: every public method takes `idx.mu` internally. Higher-level
// callers (AppendWrite, rollupFile) still hold the per-file `mu` for log
// ordering — the internal mutex is a defensive belt-and-braces guard
// against lifecycle drift where a stale snapshot of `idx` could otherwise
// be appended to concurrently with a fresh snapshot from a recreated
// payload. The internal lock is uncontended on the steady-state hot path
// (per-file mu already serialises same-payload writers) so the overhead
// is minimal — sync.Mutex's uncontended fast path is a couple of atomic
// ops; the slow path only trips under genuine contention.
//
// TRANSITIONAL-V0.17+: physical log compaction (truncating the log file
// at compactionFence) is out of scope. Consumed entries linger in
// idx.entries until DeleteAppendLog wipes the payload; memory grows
// roughly with arrival count between payload-lifetime endpoints. R-7 in
// .planning/proposals/2026-05-22-logindex-rollup-redesign.md is the
// follow-up to replace logPos-keyed consumption with file-offset-keyed
// consumption, eliminating the stalled-fence pathology entirely.

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
	// mu guards every field below. See package doc on logIndex above for
	// rationale (defensive guard against lifecycle drift; uncontended on
	// the steady-state hot path).
	mu              sync.Mutex
	entries         []logEntry
	consumed        map[uint64]struct{} // key = logEntry.logPos
	compactionFence uint64
	// fenceCursor is the index into `entries` of the first entry whose
	// logPos is at or beyond compactionFence. AdvanceFence resumes its
	// walk from this cursor instead of rescanning the entire slice on
	// every call, keeping amortized cost proportional to the entries
	// it newly consumes rather than the historical entry count.
	fenceCursor int
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

// Append records a new entry. Caller MUST pass a logPos strictly greater
// than the most recent entry's logPos (eofPos advances monotonically).
// Append does not validate ordering — the contract is on the caller.
// Internal mu guards the slice mutation.
func (idx *logIndex) Append(logPos uint64, fileOff uint64, payloadLen uint32) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
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
// Length == 0 returns nil. The fileOff + length sum is computed with
// overflow detection: a query that would wrap past math.MaxUint64
// saturates end to MaxUint64 so the overlap predicate stays well-
// formed (no record can have fileOff == MaxUint64 in practice — the
// log can't store payloads near that scale — but the cheap guard
// prevents pathological queries from missing genuine overlaps).
func (idx *logIndex) EntriesForInterval(fileOff uint64, length uint64) []logEntry {
	if length == 0 {
		return nil
	}
	end := fileOff + length
	if end < fileOff {
		end = ^uint64(0)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
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
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.consumed[logPos] = struct{}{}
}

// AdvanceFence walks consumed entries in logPos order from the current
// fenceCursor forward, advancing compactionFence past every entry that
// is consumed and contiguous. Stops at the first non-consumed entry.
// The new fence is returned. An out-of-order consumed entry (a "hole"
// left by an unconsumed predecessor) does NOT advance the fence — it
// stays in `consumed` until the predecessor is consumed too.
//
// fenceCursor tracks the first entry index at or beyond the current
// fence so repeated calls cost O(newly-consumed-entries) rather than
// O(total-entries), avoiding quadratic walks across long-lived
// payloads.
//
// This is the operation the rollup invokes after persisting a CAS
// commit; the returned fence is what advanceRollupOffset eventually
// writes to disk.
func (idx *logIndex) AdvanceFence() uint64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for idx.fenceCursor < len(idx.entries) {
		e := idx.entries[idx.fenceCursor]
		if _, ok := idx.consumed[e.logPos]; !ok {
			break
		}
		// Fence advances past the consumed entry's full record extent.
		idx.compactionFence = e.logPos + uint64(recordFrameOverhead) + uint64(e.payloadLen)
		idx.fenceCursor++
	}
	return idx.compactionFence
}

// Fence returns the current compactionFence without mutation.
func (idx *logIndex) Fence() uint64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.compactionFence
}

// SetFence overrides the compaction fence. Intended for recovery, which
// reconstructs the logIndex from records at and after the persisted
// rollup_offset; any record below that offset was already consumed by a
// pre-boot rollup pass and must not be replayed. Setting the fence to
// the boot-time rollup_offset preserves that invariant for the
// post-boot AdvanceFence walks. MUST be called before any MarkConsumed
// /AdvanceFence calls on the same logIndex.
//
// fenceCursor is rewound to 0 — recovery seeds entries in order, so
// the cursor naturally starts at the head of the slice; subsequent
// AdvanceFence calls will skip past any pre-fence entries on the first
// invocation only.
func (idx *logIndex) SetFence(fence uint64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.compactionFence = fence
	idx.fenceCursor = 0
}

// Len returns the total number of indexed entries (consumed + unconsumed).
// Test/debug surface.
func (idx *logIndex) Len() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return len(idx.entries)
}
