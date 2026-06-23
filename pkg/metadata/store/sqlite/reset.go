package sqlite

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

var _ metadata.Resetable = (*SQLiteMetadataStore)(nil)

// Reset empties every metadata table in a single transaction. The table list is
// the same backupTables slice used by Backup/Restore. While the tx holds, the
// single-writer engine blocks concurrent writers; Reset assumes the runtime has
// already verified the share is disabled.
//
// SQLite has no TRUNCATE; truncateAllTables issues an FK-safe sequence of
// DELETEs (reverse dependency order). The in-memory used-bytes / quota counters
// are reset to empty afterwards so a reused store handle reports zero usage.
func (s *SQLiteMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reset cancelled: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reset: begin txn: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := truncateAllTables(ctx, tx); err != nil {
		return fmt.Errorf("reset: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reset: commit: %w", err)
	}
	committed = true

	// Reset the in-memory counters to match the now-empty tables.
	s.usedBytes.Store(0)
	s.quotaMu.Lock()
	s.userUsage = make(map[uint32]*metadata.UsageStat)
	s.groupUsage = make(map[uint32]*metadata.UsageStat)
	s.quotaMu.Unlock()
	return nil
}
