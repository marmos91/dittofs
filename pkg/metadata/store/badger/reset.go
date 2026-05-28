package badger

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

var _ metadata.Resetable = (*BadgerMetadataStore)(nil)

// Reset truncates every key in the BadgerDB metadata store via db.DropAll.
// The same *badger.DB handle stays valid; callers can immediately follow
// up with Backupable.Restore. The cfg:store_id key is dropped along with
// everything else and gets repopulated by the next operation that needs
// it (typically the restore dump that follows).
func (s *BadgerMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("badger reset cancelled: %w", err)
	}
	if err := s.db.DropAll(); err != nil {
		return fmt.Errorf("badger reset: drop all: %w", err)
	}
	return nil
}
