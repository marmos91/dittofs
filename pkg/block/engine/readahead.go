package engine

// maxReadaheadEntries bounds the per-payload readahead state map. Readahead
// state is disposable — a dropped entry just re-ramps from depth 1 — so on
// overflow we evict an arbitrary entry rather than track access order.
// ponytail: arbitrary eviction; switch to LRU only if churn across >4096
// concurrently-hot files ever measurably costs re-ramps.
const maxReadaheadEntries = 4096

// raState tracks a payload's recent read frontier so prefetch can distinguish
// sequential runs (ramp the window) from random access (stop prefetching).
type raState struct {
	lastEnd uint64 // block index the previous read ended at
	depth   int    // current prefetch window in blocks ahead; 0 = random / no prefetch
	valid   bool   // false until the first read establishes lastEnd
}

// planReadahead updates the payload's access frontier for a read spanning
// [startBlockIdx, endBlockIdx] and returns how many blocks to prefetch ahead.
//
// Sequential reads (continuing at or just past the previous frontier) ramp the
// window 1->2->4->...->PrefetchBlocks, following the Linux readahead pattern.
// A random jump resets the window to 0: prefetching blocks after a random read
// only wastes remote GETs on data that will not be read, hurting WAN throughput
// and object-store cost. The ramp cap is SyncerConfig.PrefetchBlocks.
func (m *Syncer) planReadahead(payloadID string, startBlockIdx, endBlockIdx uint64) int {
	maxDepth := m.config.PrefetchBlocks
	if maxDepth <= 0 {
		return 0 // prefetch disabled
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
	sequential := st.valid && startBlockIdx >= st.lastEnd && startBlockIdx <= st.lastEnd+1
	switch {
	case !sequential:
		st.depth = 0
	case st.depth < 1:
		st.depth = 1
	case st.depth < maxDepth:
		st.depth *= 2
		if st.depth > maxDepth {
			st.depth = maxDepth
		}
	}
	st.lastEnd = endBlockIdx
	st.valid = true
	return st.depth
}
