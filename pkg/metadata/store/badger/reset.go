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
// too: the share read cache would otherwise keep answering GetShareOptions
// from entries whose backing records no longer exist, which is a wrong
// permission decision.
func (s *BadgerMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("badger reset cancelled: %w", err)
	}
	if err := s.db.DropAll(); err != nil {
		return fmt.Errorf("badger reset: drop all: %w", err)
	}
	s.shareCache.InvalidateAll()
	s.usedBytes.Store(0)
	s.quotaMu.Lock()
	s.quota.Reset()
	s.quotaMu.Unlock()
	return nil
}
