package postgres

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

var _ metadata.Resetable = (*PostgresMetadataStore)(nil)

// Reset truncates every metadata table in a single REPEATABLE READ
// transaction. The table list is the same backupTables slice used by
// Backup/Restore; the TestBackupTablesCoversAllMigrations CI guard keeps
// it in sync with migrations. While the tx holds, concurrent writers
// block on the truncated tables; Reset assumes Runtime.RestoreSnapshot
// has already verified the share is disabled.
func (s *PostgresMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reset cancelled: %w", err)
	}

	acquireCtx, acquireCancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer acquireCancel()

	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		return fmt.Errorf("reset: acquire connection: %w", err)
	}
	defer conn.Release()

	pgRaw := conn.Conn().PgConn()

	if _, err := pgRaw.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ").ReadAll(); err != nil {
		return fmt.Errorf("reset: begin txn: %w", err)
	}
	// Deferred rollback is a no-op after a successful COMMIT (Postgres
	// reports the tx is over and Exec returns an error which we ignore).
	defer func() { _, _ = pgRaw.Exec(ctx, "ROLLBACK").ReadAll() }()

	if err := truncateAllTables(ctx, pgRaw); err != nil {
		return fmt.Errorf("reset: %w", err)
	}

	if _, err := pgRaw.Exec(ctx, "COMMIT").ReadAll(); err != nil {
		return fmt.Errorf("reset: commit: %w", err)
	}

	return nil
}
