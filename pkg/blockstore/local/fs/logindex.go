package fs

import (
	"slices"
	"sync"
)

// Per-file logIndex (Direction 1 redesign —
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
// R-7 (closes #580): consumption is keyed by FILE-OFFSET interval, not by
// logPos. An entry's frame bytes become eligible for fence advance once
// the file-offset extent it covers is fully chunked by SOME consumed
// record (itself or any later overlapping one). This kills the
// stalled-fence pathology where a head record stuck in the dirty tree
// pinned the on-disk byte prefix even when every byte of its file region
// had been superseded by a later, already-chunked overwrite.
//
// Memory bookkeeping (R-2, #581): in-memory `idx.entries` is TRIMMED in
// lockstep with AdvanceFence — every fence advance drops the prefix of
// entries that now sit at or below compactionFence. Steady-state RSS
// therefore tracks the unconsumed-record set, not the full payload
// history. The trim invariant after every AdvanceFence call is: for all
// i, idx.entries[i].logPos >= idx.compactionFence. (The consumedCoverage
// interval set is NOT trimmed in lockstep — its merged-interval rep is
// already proportional to distinct dirty file regions, not arrival count.)
//
// Physical log compaction (#579): the post-rollup pass invokes
// compactLogLocked when (compactionFence - logHeaderSize) exceeds the
// configured threshold. The rewrite drops consumed entries below the
// fence, rebases the surviving entries' logPos values, and rewinds
// fenceCursor to 0. After compaction the in-memory entries slice and
// the on-disk log both track live records only.

// logEntry is one record's position in the log + the file-offset extent it
// covers. The entry is immutable once appended; consumption is tracked
// separately via consumedCoverage on logIndex.
type logEntry struct {
	logPos     uint64 // byte offset of the frame's first byte in the log file
	fileOff    uint64 // record's file_offset field (16-byte frame header)
	payloadLen uint32 // payload byte count (excludes frame overhead)
}

// fileEnd returns the exclusive file-offset upper bound for this record.
// Saturates on overflow so a pathological fileOff near MaxUint64 produces
// a well-formed (non-wrapping) interval — AdvanceFence's coverage test
// and EntriesForInterval's overlap test then stay correct.
func (e logEntry) fileEnd() uint64 {
	end := e.fileOff + uint64(e.payloadLen)
	if end < e.fileOff {
		return ^uint64(0)
	}
	return end
}

// logIndex maintains per-file log-position bookkeeping. Entries are
// appended in arrival order (== logPos order, since AppendWrite advances
// lf.eofPos monotonically under per-file mu). consumedCoverage is the
// union of FILE-OFFSET intervals that have been rolled up into CAS; an
// entry's frame is eligible for fence advance once consumedCoverage fully
// covers [entry.fileOff, entry.fileEnd()). compactionFence is the largest
// logPos prefix such that every entry with logPos < fence has its file
// extent fully covered — i.e. the on-disk byte prefix that is now
// eligible for physical compaction (deferred to v0.17+, #579).
//
// rollup_offset on disk continues to mean "consumed up to this log byte
// offset"; conceptually it now records compactionFence rather than a
// running scan cursor.
type logIndex struct {
	// mu guards every field below. See package doc on logIndex above for
	// rationale (defensive guard against lifecycle drift; uncontended on
	// the steady-state hot path).
	mu               sync.Mutex
	entries          []logEntry
	consumedCoverage coverageSet
	compactionFence  uint64
	// fenceCursor is the index into `entries` of the first entry whose
	// logPos is at or beyond compactionFence. AdvanceFence resumes its
	// walk from this cursor instead of rescanning the entire slice on
	// every call, keeping amortized cost proportional to the entries
	// it newly consumes rather than the historical entry count.
	fenceCursor int
	// lookupScratch is the reusable result buffer for lookupInterval (the
	// rollup hot path). Reused across rollup passes so the per-pass result
	// slice grows to its high-water mark once instead of reallocating every
	// pass. Safe because lookupInterval is only ever called by rollupFile,
	// which holds the per-file mutex start to finish.
	lookupScratch []logEntry
}

// lookupInterval returns the entries overlapping [off, off+length) using
// the per-index reusable scratch buffer. The result aliases lookupScratch
// and is valid only until the next lookupInterval call on this index. The
// caller MUST hold the per-file mutex (rollupFile does) so no concurrent
// pass clobbers the buffer.
func (idx *logIndex) lookupInterval(off, length uint64) []logEntry {
	idx.lookupScratch = idx.EntriesForInterval(off, length, idx.lookupScratch[:0])
	return idx.lookupScratch
}

// newLogIndex constructs an empty logIndex. compactionFence starts at
// logHeaderSize because the header itself is never a candidate for
// compaction.
func newLogIndex() *logIndex {
	return &logIndex{
		entries:         nil,
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
// Passing a caller-owned scratch slice (sliced to len 0) avoids a per-call
// heap allocation on the rollup hot path; pass nil for a freshly-allocated
// result. The result is appended to dst, preserving logPos (arrival) order.
func (idx *logIndex) EntriesForInterval(fileOff uint64, length uint64, dst []logEntry) []logEntry {
	if length == 0 {
		return dst
	}
	end := fileOff + length
	if end < fileOff {
		end = ^uint64(0)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, e := range idx.entries {
		if e.fileEnd() <= fileOff {
			continue
		}
		if e.fileOff >= end {
			continue
		}
		dst = append(dst, e)
	}
	return dst
}

// MarkConsumed records that a record with the given file-offset extent
// has been rolled into CAS. Idempotent — overlapping calls are merged
// into the underlying interval set. The logPos that originated the chunk
// is intentionally NOT tracked: deadness is a property of the FILE-OFFSET
// region, not of the log position (R-7, #580). A later overlapping
// MarkConsumed can therefore release the on-disk bytes of a still-
// unconsumed earlier record whose file region has been fully superseded.
//
// payloadLen == 0 is a no-op (matches AppendWrite's len(data) == 0
// short-circuit). The end = fileOff + payloadLen sum is computed with
// overflow saturation (consistent with EntriesForInterval) so a
// pathological wrap can never produce an empty interval that would
// silently fail to advance the fence.
func (idx *logIndex) MarkConsumed(fileOff uint64, payloadLen uint32) {
	if payloadLen == 0 {
		return
	}
	end := fileOff + uint64(payloadLen)
	if end < fileOff {
		end = ^uint64(0)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.consumedCoverage.add(fileOff, end)
}

// AdvanceFence walks entries in logPos order from the current fenceCursor
// forward, advancing compactionFence past every entry whose file extent
// is fully covered by consumedCoverage. Stops at the first entry whose
// file extent is NOT fully covered. The new fence is returned.
//
// R-7 semantic: an entry's frame becomes dead on disk once EVERY byte of
// [entry.fileOff, entry.fileEnd()) is covered by some chunked record
// (itself or any other overlapping record). This is strictly weaker than
// the old "consumed by logPos" predicate — an overwritten head record
// can be released without ever being consumed itself, because the
// overwrite covers its bytes.
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
// safe because
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
// After trim, the post-condition holds: for all i
// idx.entries[i].logPos >= idx.compactionFence; fenceCursor is reset
// to 0 so the next AdvanceFence call starts from the new head.
//
// This is the operation the rollup invokes after persisting a CAS
// commit; the returned fence is what advanceRollupOffset eventually
// writes to disk.
func (idx *logIndex) AdvanceFence() uint64 {
	return idx.AdvanceFenceUpTo(^uint64(0))
}

// AdvanceFenceUpTo is AdvanceFence bounded to entries whose logPos is <=
// maxLogPos. It stops the forward walk at the first entry that is either not
// fully covered by consumedCoverage OR has a logPos greater than maxLogPos,
// whichever comes first.
//
// C1: the rollup releases the per-file append mutex during its CAS-store phase
// (Phase B), so a racing AppendWrite can append a record at the SAME file
// extent as one this pass is consuming, and that record is already present in
// `entries` by the time Phase C runs. Its bytes were NOT chunked by this pass;
// fencing past it — which plain AdvanceFence would do, because the older
// record's coverage spans the same file extent — would mark it dead and lose
// the write. Bounding the walk to the max logPos this pass actually read in
// Phase A excludes any record that arrived after the snapshot (log positions
// are monotonic in arrival order), so a racing same-extent write keeps its
// entry and is rolled up by a later pass. AdvanceFence (no ceiling) preserves
// the original behavior for every other caller.
func (idx *logIndex) AdvanceFenceUpTo(maxLogPos uint64) uint64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for idx.fenceCursor < len(idx.entries) {
		e := idx.entries[idx.fenceCursor]
		if e.logPos > maxLogPos {
			break
		}
		if !idx.consumedCoverage.covers(e.fileOff, e.fileEnd()) {
			break
		}
		// Fence advances past the dead entry's full record extent.
		idx.compactionFence = e.logPos + uint64(recordFrameOverhead) + uint64(e.payloadLen)
		idx.fenceCursor++
	}
	idx.trimBelowFenceLocked()
	return idx.compactionFence
}

// trimBelowFenceLocked drops the [0:fenceCursor) prefix of `entries`
// Caller MUST hold idx.mu. fenceCursor is reset to 0 so subsequent
// AdvanceFence calls resume from the new head.
//
// To bound RSS at ~steady-state (not at the historical high-water mark)
// the backing array is REALLOCATED whenever its capacity exceeds 4x the
// surviving length. A plain reslice (`entries[fenceCursor:]`) would pin
// the original backing array forever — defeating the point of #581 on
// long-lived payloads whose log shrinks after a burst.
//
// consumedCoverage is intentionally NOT trimmed here: its merged-interval
// representation is already bounded by the count of distinct dirty file
// regions rather than by AppendWrite arrival count, so trimming would not
// meaningfully shrink it and could break the AdvanceFence coverage test
// for entries that span ranges still partially covered by old intervals.
func (idx *logIndex) trimBelowFenceLocked() {
	if idx.fenceCursor == 0 {
		return
	}
	remaining := len(idx.entries) - idx.fenceCursor
	switch {
	case remaining == 0:
		// Full drain — release backing array to GC.
		idx.entries = nil
	case cap(idx.entries) > 4*remaining:
		// Bloated backing array — copy into a right-sized one so the old
		// allocation can be reclaimed by the GC. The 4x threshold keeps
		// reallocations rare in steady state while bounding cap at O(N).
		shrunk := make([]logEntry, remaining)
		copy(shrunk, idx.entries[idx.fenceCursor:])
		idx.entries = shrunk
	default:
		// Cap already proportional to remaining — in-place shift avoids
		// the allocation. Zero the freed tail so logEntry pointer fields
		// (none today — value type — but future-proofed) aren't pinned.
		copy(idx.entries, idx.entries[idx.fenceCursor:])
		clear(idx.entries[remaining:])
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
//
// R-7 (#580): recovery only replays records at logPos >= fence — those
// below the persisted rollup_offset were already chunked and the bytes
// already reclaimed at the on-disk level. They are NOT in `entries`, so
// AdvanceFence never tries to walk past them and no pre-seeding of
// consumedCoverage for pre-fence file regions is required. A post-boot
// re-write that lands at the same file-offset as a pre-boot chunked
// record will, in the steady state, be consumed by a future rollup pass
// the normal way; its own MarkConsumed adds its extent to
// consumedCoverage, allowing the fence to walk forward without needing
// pre-fence coverage seeding.
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

// coverageSet is a sorted, merged set of half-open [start, end) byte
// intervals over the file-offset domain. It answers two questions in
// log(N): "add this interval" and "is this interval fully covered". The
// invariant is that stored intervals are non-empty, non-overlapping, and
// non-adjacent (adjacent intervals are merged on insert) so coverage
// tests reduce to a single binary search.
//
// Not goroutine-safe on its own — embedded in logIndex which guards all
// access via idx.mu.
//
// The btree-backed intervalTree was considered and rejected: it carries
// per-entry Touched timestamps and an offset-only ordering keyed for the
// stabilization-window query (EarliestStable). The coverage problem here
// is a pure interval-union problem with neither timestamps nor stable-
// interval semantics — a flat sorted slice is faster, simpler, and has
// the same big-O for both operations at the workload-realistic entry
// counts (the per-payload coverage set tracks unique consumed regions
// which the rollup actively coalesces by chunking adjacent intervals).
type coverageSet struct {
	intervals []coverageInterval
}

type coverageInterval struct {
	start, end uint64 // half-open [start, end)
}

// add merges [start, end) into the set, coalescing with adjacent and
// overlapping intervals. O(log N) binary search + O(K) merge where K is
// the number of intervals fully subsumed by the new range (typically 0
// or 1 in steady state because the rollup chunks contiguous regions).
//
// Empty or inverted ranges (end <= start) are silently ignored.
func (cs *coverageSet) add(start, end uint64) {
	if end <= start {
		return
	}
	// Binary search for the first existing interval whose `end` is >= new
	// `start`. Any interval ending strictly before `start` cannot overlap
	// or be adjacent (we treat [a,b) and [b,c) as adjacent → merge), so we
	// keep it as-is to the left.
	lo, hi := 0, len(cs.intervals)
	for lo < hi {
		mid := (lo + hi) / 2
		if cs.intervals[mid].end < start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	insertAt := lo
	// Sweep forward from insertAt, absorbing every interval whose `start`
	// is <= mergeEnd (adjacent intervals merge because their boundary
	// touches). Stored intervals are sorted and non-overlapping, so only
	// intervals[insertAt] can possibly lower mergeStart — every later
	// interval starts at or beyond the previous one's end.
	mergeStart, mergeEnd := start, end
	j := insertAt
	for j < len(cs.intervals) && cs.intervals[j].start <= mergeEnd {
		mergeStart = min(mergeStart, cs.intervals[j].start)
		mergeEnd = max(mergeEnd, cs.intervals[j].end)
		j++
	}
	// Replace [insertAt, j) with the single merged interval. Handles
	// insert (j == insertAt), in-place rewrite (j == insertAt+1) and
	// subsumption (j > insertAt+1) uniformly.
	cs.intervals = slices.Replace(cs.intervals, insertAt, j,
		coverageInterval{start: mergeStart, end: mergeEnd})
}

// covers reports whether [start, end) is fully covered by the union of
// stored intervals. Empty / inverted ranges (end <= start) return true
// vacuously — a zero-length region has nothing to cover.
//
// O(log N) binary search to locate the candidate interval, plus a single
// containment test. Because the set is merged-on-insert, at most one
// stored interval can contain a given query range; if that one doesn't
// no other can.
func (cs *coverageSet) covers(start, end uint64) bool {
	if end <= start {
		return true
	}
	// Binary search for the LAST interval whose `start` is <= query start.
	// That candidate is the only interval that could possibly contain
	// [start, end).
	lo, hi := 0, len(cs.intervals)
	for lo < hi {
		mid := (lo + hi) / 2
		if cs.intervals[mid].start <= start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return false
	}
	cand := cs.intervals[lo-1]
	return cand.start <= start && cand.end >= end
}
