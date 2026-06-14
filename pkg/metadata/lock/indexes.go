// Reverse indexes over unifiedLocks for O(1)/bounded lookups on hot paths.
//
// Two indexes are maintained:
//
//   - leaseKeyIndex: leaseKey [16]byte -> handleKey. Lets findLeaseByKey
//     resolve a lease in O(1) instead of scanning every (handleKey, lock)
//     pair in unifiedLocks. A lease key resolves to whichever handleKey bucket
//     most recently added a record for it; reindexHandleLocked rebinds it from
//     the live slice on every mutation, so the binding never drifts.
//
//   - clientHandleIndex: clientID -> set of handleKeys that hold at least one
//     lock owned by that client (reference-counted per bucket). Lets
//     RemoveClientLocks touch only the buckets a client actually appears in
//     instead of scanning the whole map under the global mutex.
//
// Both indexes are DERIVED state: every mutation site updates the live
// unifiedLocks slice and then calls reindexHandleLocked(handleKey, old) with
// the pre-mutation slice. reindexHandleLocked removes every old contribution
// and re-adds every current one, so the index is always reconstructed from the
// authoritative slice and can never silently drift. All index access requires
// lm.mu (write for mutation, read or write for lookup).
package lock

// ensureIndexes lazily initializes the reverse-index maps. The index maps are
// created on first use so existing constructors (and any test doubles building
// a Manager literal) keep working without an explicit init.
//
// Must be called with lm.mu held for writing.
func (lm *Manager) ensureIndexes() {
	if lm.leaseKeyIndex == nil {
		lm.leaseKeyIndex = make(map[[16]byte]string)
	}
	if lm.clientHandleIndex == nil {
		lm.clientHandleIndex = make(map[string]map[string]int)
	}
}

// indexAddLockLocked records a single lock's contributions to the reverse
// indexes for handleKey. Must hold lm.mu for writing.
func (lm *Manager) indexAddLockLocked(handleKey string, l *UnifiedLock) {
	if l == nil {
		return
	}
	lm.ensureIndexes()

	if l.Lease != nil {
		// Last add for a given key wins the bucket binding. findLeaseByKey
		// previously returned the first (handleKey, lock) match while scanning
		// non-deterministic map iteration order; binding to whichever bucket
		// most recently added the key is equally valid and is reconciled from
		// the live slice on every reindex.
		lm.leaseKeyIndex[l.Lease.LeaseKey] = handleKey
	}

	if cid := l.Owner.ClientID; cid != "" {
		set := lm.clientHandleIndex[cid]
		if set == nil {
			set = make(map[string]int)
			lm.clientHandleIndex[cid] = set
		}
		set[handleKey]++
	}
}

// indexRemoveLockLocked removes a single lock's contributions from the reverse
// indexes for handleKey. Must hold lm.mu for writing.
func (lm *Manager) indexRemoveLockLocked(handleKey string, l *UnifiedLock) {
	if l == nil {
		return
	}
	lm.ensureIndexes()

	if l.Lease != nil {
		// Only drop the leaseKey binding if it still points at this bucket.
		// A re-add in another bucket (same key across files) may have rebound
		// it; the bucket that now owns the binding keeps it.
		if lm.leaseKeyIndex[l.Lease.LeaseKey] == handleKey {
			delete(lm.leaseKeyIndex, l.Lease.LeaseKey)
		}
	}

	if cid := l.Owner.ClientID; cid != "" {
		if set := lm.clientHandleIndex[cid]; set != nil {
			if n := set[handleKey]; n > 1 {
				set[handleKey] = n - 1
			} else {
				delete(set, handleKey)
				if len(set) == 0 {
					delete(lm.clientHandleIndex, cid)
				}
			}
		}
	}
}

// reindexHandleLocked reconciles the reverse indexes for handleKey after the
// authoritative lm.unifiedLocks[handleKey] slice has been mutated. `old` is the
// slice as it was BEFORE the mutation; the current slice is read live from the
// map. The delta is computed by removing every old contribution and re-adding
// every current one — cheap because both slices are per-file (small) and this
// keeps the index a pure function of live state, immune to drift.
//
// Must hold lm.mu for writing.
func (lm *Manager) reindexHandleLocked(handleKey string, old []*UnifiedLock) {
	for _, l := range old {
		lm.indexRemoveLockLocked(handleKey, l)
	}
	for _, l := range lm.unifiedLocks[handleKey] {
		lm.indexAddLockLocked(handleKey, l)
	}
}
