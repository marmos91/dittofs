package badger

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

var _ metadata.Resetable = (*BadgerMetadataStore)(nil)

// Reset truncates every key in the BadgerDB metadata store via db.DropAll.
// The same *badger.DB handle stays valid; callers can immediately follow
// up with Snapshotable.RestoreSnapshot. The cfg:store_id key is dropped along with
// everything else and gets repopulated by the next operation that needs
// it (typically the restore dump that follows).
//
// Every in-memory structure derived from the dropped keys is re-derived here
// too: the derived caches would otherwise keep answering from entries whose
// backing records no longer exist — a wrong permission decision for share
// options, and pre-drop attributes or directory entries for the rest.
func (s *BadgerMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("badger reset cancelled: %w", err)
	}
	if err := s.db.DropAll(); err != nil {
		return fmt.Errorf("badger reset: drop all: %w", err)
	}
	s.invalidateDerivedCaches()
	s.usedBytes.Store(0)
	s.quotaMu.Lock()
	s.quota.Reset()
	s.quotaMu.Unlock()
	return nil
}

// invalidateDerivedCaches clears every cache the store derives from the
// durable records, for the paths that replace those records wholesale rather
// than one at a time. Ordinary mutations invalidate per key from
// withTransaction; a wholesale replacement has no key list to work from, and
// the restore path never goes through a badgerTransaction at all.
//
// The filesystem-statistics cache is deliberately left alone: it carries its
// own short TTL and re-derives itself, so it cannot outlive the wipe the way a
// TTL-less entry can.
func (s *BadgerMetadataStore) invalidateDerivedCaches() {
	s.shareCache.InvalidateAll()
	s.readCache.invalidateAll()
	s.parentCache.invalidateAll()
	s.direntCache.invalidateAll()
}
