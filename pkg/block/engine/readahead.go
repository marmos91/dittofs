package engine

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
// block exactly once.
type raState struct {
	lastEnd       uint64 // block index the previous read ended at
	scheduledUpTo uint64 // highest block index already scheduled for prefetch
	valid         bool   // false until the first read establishes lastEnd
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

	m.readaheadMu.Lock()
	defer m.readaheadMu.Unlock()

	st, ok := m.readahead[payloadID]
	if !ok {
		if len(m.readahead) >= maxReadaheadEntries {
			for k := range m.readahead { // evict one arbitrary disposable entry
				delete(m.readahead, k)
				break
			}
		}
		st = &raState{}
		m.readahead[payloadID] = st
	}

	// Sequential = the new read begins at or immediately after the previous
	// frontier (tolerates a re-read of the last block and a contiguous advance).
	sequential := st.valid && start >= st.lastEnd && start <= st.lastEnd+1
	st.lastEnd = end
	st.valid = true

	// First read of a payload is not a pattern (sequential requires the prior
	// st.valid, so a first read is never sequential); a random jump resets the
	// anchor. Either way we re-anchor scheduledUpTo to the current frontier and
	// prefetch nothing this call.
	if !sequential {
		st.scheduledUpTo = end
		return 0, 0, false
	}

	// Confirmed sequential run: keep a full window ahead of the frontier. No
	// geometric ramp — that lagged the reader (the reader consumes a local block
	// faster than a single S3 fetch completes, so a slow ramp never gets ahead).
	target := end + uint64(maxDepth)
	from = st.scheduledUpTo
	if from < end {
		from = end
	}
	if target <= from {
		return 0, 0, false // window already scheduled — nothing new
	}
	st.scheduledUpTo = target
	return from, target, true
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
