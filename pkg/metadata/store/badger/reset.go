package badger

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: BadgerMetadataStore implements Resetable.
var _ metadata.Resetable = (*BadgerMetadataStore)(nil)

// Reset truncates every key in the BadgerDB metadata store via db.DropAll,
// the Badger-documented atomic-truncate primitive. The same *badger.DB
// handle (s.db) stays valid afterward — callers can immediately follow up
// with Backupable.Restore or any other store operation without reopening
// the database.
//
// Reset preserves the live store handle and the engine-persistent storeID
// only insofar as Badger's cfg:store_id key was wiped along with everything
// else. The next operation that needs storeID will repopulate it via the
// existing first-open path. (For the in-flight Phase 24 restore flow, the
// dump that follows Reset re-establishes storeID atomically.)
func (s *BadgerMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("badger reset cancelled: %w", err)
	}
	if err := s.db.DropAll(); err != nil {
		return fmt.Errorf("badger reset: drop all: %w", err)
	}
	return nil
}
