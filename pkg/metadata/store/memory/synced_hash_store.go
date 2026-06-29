package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: the memory engine implements SyncedHashStore.
var _ metadata.SyncedHashStore = (*MemoryMetadataStore)(nil)

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
func (s *MemoryMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash) error {
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
	return nil
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
	return nil
}
