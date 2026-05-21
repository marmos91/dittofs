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
// the first MarkSynced for a given hash stamps the current time;
// subsequent calls are no-ops that preserve the original timestamp.
// The first-write-wins semantics matter for operators reasoning about
// "when was this hash first mirrored" — overwriting on every re-apply
// would surface the most-recent re-Put time instead, which is
// misleading when a periodic mirror loop re-checks already-synced
// hashes.
func (s *MemoryMetadataStore) MarkSynced(ctx context.Context, hash blockstore.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.syncedMu.Lock()
	defer s.syncedMu.Unlock()
	if s.synced == nil {
		s.synced = make(map[blockstore.ContentHash]time.Time)
	}
	if _, ok := s.synced[hash]; ok {
		return nil
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
