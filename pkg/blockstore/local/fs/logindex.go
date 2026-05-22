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
// at compactionFence) is out of scope. The on-disk log keeps growing
// until DeleteAppendLog wipes the payload; pressure machinery caps the
// worst case via maxLogBytes. R-7 in
// .planning/proposals/2026-05-22-logindex-rollup-redesign.md is the
// follow-up to replace logPos-keyed consumption with file-offset-keyed
// consumption, eliminating the stalled-fence pathology entirely.
//
// Memory bookkeeping (R-2, issue #581): in-memory `idx.entries` and
// `idx.consumed` are TRIMMED in lockstep with AdvanceFence — every
// fence advance drops the prefix of entries that now sit at or below
// compactionFence and removes the matching keys from the consumed map.
// Steady-state RSS therefore tracks the unconsumed-record set, not the
// full payload history. The trim invariant after every AdvanceFence
// call is: for all i, idx.entries[i].logPos >= idx.compactionFence
// (the surviving prefix is the unconsumed-and-after-fence suffix).

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
// R-2 (#581) trim: any prefix that the fence walks past in this call
// is then DROPPED from `idx.entries` and the matching keys are removed
// from `idx.consumed`. This bounds steady-state memory at the
// unconsumed-record set, not the full payload history. Trimming is
// safe because:
//
//   - Fenced entries' chunks are already durable in CAS (the rollup
//     called MarkConsumed only after a successful StoreChunk pass).
//   - rollupFile invokes tree.ConsumeUpTo immediately after
//     AdvanceFence, so the matching file-offset intervals leave the
//     dirty tree at the same point and no future EntriesForInterval
//     query will look for them.
//   - Recovery's lastPos walk rebuilds the index from scratch from the
//     persisted rollup_offset forward — it never consults pre-fence
//     in-memory entries.
//
// After trim, the post-condition holds: for all i,
// idx.entries[i].logPos >= idx.compactionFence; fenceCursor is reset
// to 0 so the next AdvanceFence call starts from the new head.
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
	idx.trimBelowFenceLocked()
	return idx.compactionFence
}

// trimBelowFenceLocked drops the [0:fenceCursor) prefix of `entries`
// and removes the matching keys from `consumed`. Caller MUST hold
// idx.mu. fenceCursor is reset to 0 so subsequent AdvanceFence calls
// resume from the new head.
//
// A copy-shift (append-to-truncated-zero-len-slice) is used rather
// than `entries = entries[fenceCursor:]` so the underlying backing
// array's head bytes are released to the GC instead of being pinned
// forever by the slice header — the long-lived-payload pathology
// (#581) was precisely about that backing array growing unbounded.
func (idx *logIndex) trimBelowFenceLocked() {
	if idx.fenceCursor == 0 {
		return
	}
	for i := 0; i < idx.fenceCursor; i++ {
		delete(idx.consumed, idx.entries[i].logPos)
	}
	remaining := len(idx.entries) - idx.fenceCursor
	if remaining == 0 {
		// Reset to nil so the backing array drops to GC; a long-idle
		// payload that has just had its log fully consumed should not
		// hold on to a megabyte-class slice.
		idx.entries = nil
	} else {
		// Shift-and-truncate. copy + reslice keeps allocations at zero
		// on the hot path; the freed tail elements are zeroed so any
		// retained references to payload extents would be visible to
		// the race detector (none exist today — logEntry is a value
		// type — but the discipline keeps future logEntry additions
		// safe by default).
		copy(idx.entries, idx.entries[idx.fenceCursor:])
		for i := remaining; i < len(idx.entries); i++ {
			idx.entries[i] = logEntry{}
		}
		idx.entries = idx.entries[:remaining]
	}
	idx.fenceCursor = 0
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
