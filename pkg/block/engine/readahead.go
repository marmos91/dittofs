package engine

import "sync/atomic"

// maxReadaheadEntries bounds the per-payload readahead state map. Readahead
// state is disposable — a dropped entry just re-ramps from a cold start (the
// re-creating read establishes the frontier, the next sequential read schedules
// the window) — so on overflow we evict an arbitrary entry rather than track
// access order.
// ponytail: arbitrary eviction; switch to LRU only if churn across >4096
// concurrently-hot files ever measurably costs re-ramps.
const maxReadaheadEntries = 4096

// raState tracks a payload's read frontier so the readahead driver can keep a
// fixed window of blocks fetched ahead of a sequential reader and schedule each
// block exactly once. Fields are atomic so planWindow updates the frontier
// without a lock; concurrent reads of one payload may race here, but the state
// is a disposable heuristic (a stale frontier only mis-sizes prefetch), so the
// races are benign.
type raState struct {
	lastEnd       atomic.Uint64 // block index the previous read ended at
	scheduledUpTo atomic.Uint64 // highest block index already scheduled for prefetch
	valid         atomic.Bool   // false until the first read establishes lastEnd
}

// planWindow advances the payload's read frontier for a read spanning block
// indices [start, end] and returns the half-open block range (from, to] that
// should be scheduled for prefetch this call, or fire=false when nothing new
// should be scheduled.
//
// Unlike the old geometric planReadahead (which ramped 1->2->4 and only ran on
// a local miss inside EnsureAvailableAndRead), this keeps a FULL fixed window of
// config.PrefetchBlocks blocks in flight ahead of the frontier and is driven on
// EVERY read (local hits included) via scheduleReadahead. A sequential reader
// serving from the local tier therefore keeps the window sliding forward instead
// of stalling once reads stop missing. Each block is scheduled exactly once
// (bounded by scheduledUpTo); the first read of a payload only establishes the
// frontier, and a random jump resets the anchor so we never prefetch blocks a
// random reader will not touch.
func (m *Syncer) planWindow(payloadID string, start, end uint64) (from, to uint64, fire bool) {
	maxDepth := m.config.PrefetchBlocks
	if maxDepth <= 0 {
		return 0, 0, false // prefetch disabled
	}

	// Lock-free per-payload lookup. A gosync.Map keeps the read hot path free of
	// the global mutex that previously serialized every read here.
	v, loaded := m.readahead.LoadOrStore(payloadID, &raState{})
	if !loaded && m.readaheadN.Add(1) > maxReadaheadEntries {
		m.pruneReadahead()
	}
	st := v.(*raState)

	// Sequential = the new read begins at or immediately after the previous
	// frontier (tolerates a re-read of the last block and a contiguous advance).
	// Load lastEnd once; a concurrent same-payload read may interleave, but the
	// frontier is a disposable heuristic so a mis-detected pattern only affects
	// prefetch sizing, never correctness.
	last := st.lastEnd.Load()
	sequential := st.valid.Load() && start >= last && start <= last+1
	st.lastEnd.Store(end)
	st.valid.Store(true)

	// First read of a payload is not a pattern (sequential requires the prior
	// valid, so a first read is never sequential); a random jump resets the
	// anchor. Either way we re-anchor scheduledUpTo to the current frontier and
	// prefetch nothing this call.
	if !sequential {
		st.scheduledUpTo.Store(end)
		return 0, 0, false
	}

	// Confirmed sequential run: keep a full window ahead of the frontier. No
	// geometric ramp — that lagged the reader (the reader consumes a local block
	// faster than a single S3 fetch completes, so a slow ramp never gets ahead).
	target := end + uint64(maxDepth)
	from = st.scheduledUpTo.Load()
	if from < end {
		from = end
	}
	if target <= from {
		return 0, 0, false // window already scheduled — nothing new
	}
	st.scheduledUpTo.Store(target)
	return from, target, true
}

// pruneReadahead best-effort trims the disposable readahead map back toward
// half of maxReadaheadEntries when it overflows. Only one goroutine prunes at a
// time; entries are dropped in gosync.Map.Range order (arbitrary) — a dropped
// payload just re-ramps from a cold frontier on its next read. readaheadN is
// approximate under concurrent inserts, which is fine for a soft bound.
func (m *Syncer) pruneReadahead() {
	if !m.readaheadPruning.CompareAndSwap(false, true) {
		return // another goroutine is already pruning
	}
	defer m.readaheadPruning.Store(false)
	target := int64(maxReadaheadEntries / 2)
	m.readahead.Range(func(k, _ any) bool {
		if m.readaheadN.Load() <= target {
			return false
		}
		m.readahead.Delete(k)
		m.readaheadN.Add(-1)
		return true
	})
}

// scheduleReadahead is the offset-based sliding-window readahead driver, invoked
// on every read from Store.ReadAt. It keeps the local read-serving tier populated
// ahead of a sequential reader by enqueuing the next window of blocks onto the
// SyncQueue's bounded worker pool (config.ParallelDownloads wide). The prefetch
// workers register in the in-flight map (fetchBlock -> inlineFetchOrWait) so a
// demand read for an already-in-flight block piggybacks instead of issuing a
// duplicate S3 GET — the shared budget that bounds total remote concurrency.
//
// No-op without a remote, while the remote is unhealthy, or for a zero-length
// read. The hot-path cost is one in-memory frontier update (planWindow); the
// per-block local/remote probes happen in the worker pool, off the read path.
func (m *Syncer) scheduleReadahead(payloadID string, offset uint64, length uint32) {
	if length == 0 || m.queue == nil {
		return
	}
	if !m.hasRemote.Load() || !m.IsRemoteHealthy() {
		return
	}
	start, end := blockRange(offset, length)
	from, to, fire := m.planWindow(payloadID, start, end)
	if !fire {
		return
	}
	// Best-effort enqueue: EnqueuePrefetch drops when the SyncQueue is saturated.
	// A dropped block is NOT a window hole — the reader's own demand fetch
	// (EnsureAvailableAndRead) still serves it, just without the prefetch head
	// start. We deliberately do NOT roll scheduledUpTo back on a drop: under a
	// saturated queue, degrading prefetch to demand is the correct backpressure,
	// and re-enqueue churn on every read would only deepen the saturation. The
	// advancing frontier keeps scheduling fresh blocks regardless.
	for b := from + 1; b <= to; b++ {
		m.queue.EnqueuePrefetch(TransferRequest{PayloadID: payloadID, BlockIndex: b})
	}
}
