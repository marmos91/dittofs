// Reverse indexes over unifiedLocks for O(1)/bounded lookups on hot paths.
//
// Two indexes are maintained:
//
//   - leaseKeyIndex: leaseKey [16]byte -> set of handleKeys (ref-counted per
//     bucket) that hold a record for it. Lets findLeaseByKey probe only the
//     buckets that actually hold the key instead of scanning every
//     (handleKey, lock) pair in unifiedLocks. The same numeric lease key may be
//     bound on multiple files at once (distinct handleKey buckets — see
//     hasLeaseKeyOnOtherFile), so the index tracks every holder; removing a
//     record from one bucket never drops the bindings in the others.
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
		lm.leaseKeyIndex = make(map[[16]byte]map[string]int)
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
		// Track this bucket as a holder of the key. A lease record may share a
		// numeric key with records on other files, so the index counts holders
		// per bucket rather than binding to a single one — removing one bucket's
		// record can never orphan another bucket that still holds the key.
		buckets := lm.leaseKeyIndex[l.Lease.LeaseKey]
		if buckets == nil {
			buckets = make(map[string]int)
			lm.leaseKeyIndex[l.Lease.LeaseKey] = buckets
		}
		buckets[handleKey]++
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
		// Decrement this bucket's holder count; only drop the bucket (and the
		// whole key entry, once empty) when it holds no more records for the
		// key. Other buckets that share the same numeric key are untouched.
		if buckets := lm.leaseKeyIndex[l.Lease.LeaseKey]; buckets != nil {
			if n := buckets[handleKey]; n > 1 {
				buckets[handleKey] = n - 1
			} else {
				delete(buckets, handleKey)
				if len(buckets) == 0 {
					delete(lm.leaseKeyIndex, l.Lease.LeaseKey)
				}
			}
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
