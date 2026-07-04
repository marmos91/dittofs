package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertions: the memory engine and its transaction implement
// SyncedHashStore.
var (
	_ metadata.SyncedHashStore = (*MemoryMetadataStore)(nil)
	_ metadata.SyncedHashStore = (*memoryTransaction)(nil)
)

// MarkSyncedAtForTest stamps a synced marker with an explicit first-mirror
// time. Test-only: it lets GC grace-window tests backdate when a hash was
// synced, mirroring remotememory.Store.SetNowFnForTest for the remote object
// clock. Production code uses MarkSynced, which stamps the current time.
func (s *MemoryMetadataStore) MarkSyncedAtForTest(hash block.ContentHash, when time.Time) {
	s.syncedMu.Lock()
	defer s.syncedMu.Unlock()
	if s.synced == nil {
		s.synced = make(map[block.ContentHash]time.Time)
	}
	s.synced[hash] = when
}

// EnumerateSynced streams every synced marker with its first-mirror time.
// It snapshots the map under the read lock so fn runs without holding it.
func (s *MemoryMetadataStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.syncedMu.RLock()
	snapshot := make(map[block.ContentHash]time.Time, len(s.synced))
	for h, t := range s.synced {
		snapshot[h] = t
	}
	s.syncedMu.RUnlock()

	for h, t := range snapshot {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(h, t); err != nil {
			return err
		}
	}
	return nil
}

// IsSynced reports whether hash has been mirrored to remote. Returns
// (false, nil) when no entry exists for hash.
func (s *MemoryMetadataStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.syncedMu.RLock()
	defer s.syncedMu.RUnlock()
	if s.synced == nil {
		return false, nil
	}
	_, ok := s.synced[hash]
	return ok, nil
}

// MarkSynced records that hash has been mirrored to remote. Idempotent:
// the first MarkSynced for a given hash stamps the current time;
// subsequent calls are no-ops that preserve the original timestamp.
// The first-write-wins semantics matter for operators reasoning about
// "when was this hash first mirrored" — overwriting on every re-apply
// would surface the most-recent re-Put time instead, which is
// misleading when a periodic mirror loop re-checks already-synced
// hashes.
func (s *MemoryMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.syncedMu.Lock()
	defer s.syncedMu.Unlock()
	if s.synced == nil {
		s.synced = make(map[block.ContentHash]time.Time)
	}
	if _, ok := s.synced[hash]; ok {
		return nil
	}
	s.synced[hash] = time.Now()
	// Record only block locators; standalone chunks resolve from absence.
	if !loc.IsStandalone() {
		if s.syncedLocators == nil {
			s.syncedLocators = make(map[block.ContentHash]block.ChunkLocator)
		}
		s.syncedLocators[hash] = loc
	}
	return nil
}

// GetLocator returns the recorded remote locator for hash. (zero, false, nil)
// when unsynced; a synced standalone/legacy hash yields the zero (standalone)
// locator with found == true.
func (s *MemoryMetadataStore) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}
	s.syncedMu.RLock()
	defer s.syncedMu.RUnlock()
	if s.synced == nil {
		return block.ChunkLocator{}, false, nil
	}
	if _, ok := s.synced[hash]; !ok {
		return block.ChunkLocator{}, false, nil
	}
	return s.syncedLocators[hash], true, nil
}

// DeleteSynced removes the synced marker for hash. Idempotent: deleting
// an absent hash returns nil.
func (s *MemoryMetadataStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.syncedMu.Lock()
	defer s.syncedMu.Unlock()
	if s.synced == nil {
		return nil
	}
	delete(s.synced, hash)
	delete(s.syncedLocators, hash)
	return nil
}

// ============================================================================
// Transaction-level SyncedHashStore
// ============================================================================
//
// The synced maps are guarded by syncedMu (not the store.mu the transaction
// holds), so the tx methods do NOT mutate them directly. Mutations buffer in
// tx.syncedOps — overlaid over the store for read-your-writes — and
// WithTransaction applies the buffer under syncedMu after a successful
// commit, so a rollback discards them (see transaction.go).

func (tx *memoryTransaction) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if st, ok := tx.syncedOps[hash]; ok {
		return !st.deleted, nil
	}
	return tx.store.IsSynced(ctx, hash)
}

// MarkSynced records the synced marker inside the transaction. First-wins,
// matching the direct method: a hash already marked (in this tx or in the
// store) is a no-op — EXCEPT after a DeleteSynced in the same tx, where the
// new locator wins (read-your-writes; this is what gives DefaultCommitBlock
// its locator-overwrite semantics).
func (tx *memoryTransaction) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if st, ok := tx.syncedOps[hash]; ok {
		if st.deleted {
			tx.syncedOps[hash] = syncedTxState{loc: loc}
		}
		return nil // already marked in this tx — first locator wins
	}
	synced, err := tx.store.IsSynced(ctx, hash)
	if err != nil {
		return err
	}
	if synced {
		return nil // already marked in the store — first locator wins
	}
	if tx.syncedOps == nil {
		tx.syncedOps = make(map[block.ContentHash]syncedTxState)
	}
	tx.syncedOps[hash] = syncedTxState{loc: loc}
	return nil
}

func (tx *memoryTransaction) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if st, ok := tx.syncedOps[hash]; ok {
		if st.deleted {
			return block.ChunkLocator{}, false, nil
		}
		if st.loc.IsStandalone() {
			return block.ChunkLocator{}, true, nil
		}
		return st.loc, true, nil
	}
	return tx.store.GetLocator(ctx, hash)
}

func (tx *memoryTransaction) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx.syncedOps == nil {
		tx.syncedOps = make(map[block.ContentHash]syncedTxState)
	}
	tx.syncedOps[hash] = syncedTxState{deleted: true}
	return nil
}
