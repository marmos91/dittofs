package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: the memory engine implements SyncedHashStore.
var _ metadata.SyncedHashStore = (*MemoryMetadataStore)(nil)

// IsSynced reports whether hash has been mirrored to remote. Returns
// (false, nil) when no entry exists for hash.
func (s *MemoryMetadataStore) IsSynced(ctx context.Context, hash blockstore.ContentHash) (bool, error) {
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
// re-applying the same hash overwrites the timestamp and returns nil.
func (s *MemoryMetadataStore) MarkSynced(ctx context.Context, hash blockstore.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.syncedMu.Lock()
	defer s.syncedMu.Unlock()
	if s.synced == nil {
		s.synced = make(map[blockstore.ContentHash]time.Time)
	}
	s.synced[hash] = time.Now()
	return nil
}

// DeleteSynced removes the synced marker for hash. Idempotent: deleting
// an absent hash returns nil.
func (s *MemoryMetadataStore) DeleteSynced(ctx context.Context, hash blockstore.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.syncedMu.Lock()
	defer s.syncedMu.Unlock()
	if s.synced == nil {
		return nil
	}
	delete(s.synced, hash)
	return nil
}
