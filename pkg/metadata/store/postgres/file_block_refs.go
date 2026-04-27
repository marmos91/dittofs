package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ============================================================================
// file_block_refs CRUD (META-01)
// ============================================================================
//
// Stores FileAttr.Blocks []blockstore.BlockRef rows for files. Per Phase 12
// D-01 we use a separate table (not JSONB on files) to avoid TOAST write
// amplification on the VM-primary workload.
//
// All helpers operate against a pgx.Tx so that PutFile's BlockRef replace
// happens atomically with the files row UPDATE.
//
// Schema lives in migrations/000012_file_block_refs.up.sql.

// putFileBlockRefs replaces all rows in file_block_refs for fileID with
// the given blocks. Atomic when called inside a pgx.Tx (the caller's tx).
//
// Implementation: DELETE+INSERT. Engine-bug paths are defended by the
// (file_id, "offset") PK — a duplicate offset would be rejected. The
// DELETE first ensures stale offsets from a prior list are not left
// behind when the new list is shorter.
func putFileBlockRefs(ctx context.Context, tx pgx.Tx, fileID uuid.UUID, blocks []blockstore.BlockRef) error {
	if _, err := tx.Exec(ctx, `DELETE FROM file_block_refs WHERE file_id = $1`, fileID); err != nil {
		return fmt.Errorf("delete file_block_refs for %s: %w", fileID, err)
	}
	if len(blocks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, b := range blocks {
		batch.Queue(
			`INSERT INTO file_block_refs (file_id, "offset", size, hash) VALUES ($1, $2, $3, $4)`,
			fileID, int64(b.Offset), int32(b.Size), b.Hash[:],
		)
	}
	br := tx.SendBatch(ctx, batch)
	defer br.Close()
	for range blocks {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("insert file_block_ref: %w", err)
		}
	}
	return nil
}

// getFileBlockRefs returns all rows for fileID ordered by offset ASC.
// Returns nil (no error) if no rows exist.
//
// Used by GetFile to populate FileAttr.Blocks. The covering index
// (PRIMARY KEY ... INCLUDE(size, hash)) means this is index-only; no
// heap fetch on the cold-cache read path.
func getFileBlockRefs(ctx context.Context, tx pgx.Tx, fileID uuid.UUID) ([]blockstore.BlockRef, error) {
	rows, err := tx.Query(ctx,
		`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = $1 ORDER BY "offset" ASC`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("query file_block_refs for %s: %w", fileID, err)
	}
	defer rows.Close()

	var out []blockstore.BlockRef
	for rows.Next() {
		var off int64
		var sz int32
		var raw []byte
		if err := rows.Scan(&off, &sz, &raw); err != nil {
			return nil, fmt.Errorf("scan file_block_ref: %w", err)
		}
		// T-12-06 mitigation: never coerce a malformed BYTEA hash to a
		// half-decoded BlockRef — surface the error explicitly.
		if len(raw) != blockstore.HashSize {
			return nil, fmt.Errorf(
				"file_block_refs.hash for %s/%d has unexpected length %d (want %d)",
				fileID, off, len(raw), blockstore.HashSize,
			)
		}
		var br blockstore.BlockRef
		copy(br.Hash[:], raw)
		br.Offset = uint64(off)
		br.Size = uint32(sz)
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file_block_refs: %w", err)
	}
	return out, nil
}

// deleteFileBlockRefs removes all rows for fileID. The FK cascade (D-03)
// handles this automatically when the files row is deleted; this helper
// is exposed for callers that pre-clear refs without dropping the row.
//
//nolint:unused // exported as part of the API surface; FK cascade handles
// the file-delete path today, but plan-defined future callers may need
// pre-clear semantics.
func deleteFileBlockRefs(ctx context.Context, tx pgx.Tx, fileID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM file_block_refs WHERE file_id = $1`, fileID); err != nil {
		return fmt.Errorf("delete file_block_refs for %s: %w", fileID, err)
	}
	return nil
}

// loadFileBlockRefs loads all rows for fileID via the pool (not a tx).
// Used by GetFile, which intentionally avoids a tx for read performance.
// Same SQL as getFileBlockRefs but routed through the store's pool helper.
func (s *PostgresMetadataStore) loadFileBlockRefs(ctx context.Context, fileID uuid.UUID) ([]blockstore.BlockRef, error) {
	rows, err := s.query(ctx,
		`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = $1 ORDER BY "offset" ASC`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("query file_block_refs for %s: %w", fileID, err)
	}
	defer rows.Close()

	var out []blockstore.BlockRef
	for rows.Next() {
		var off int64
		var sz int32
		var raw []byte
		if err := rows.Scan(&off, &sz, &raw); err != nil {
			return nil, fmt.Errorf("scan file_block_ref: %w", err)
		}
		if len(raw) != blockstore.HashSize {
			return nil, fmt.Errorf(
				"file_block_refs.hash for %s/%d has unexpected length %d (want %d)",
				fileID, off, len(raw), blockstore.HashSize,
			)
		}
		var br blockstore.BlockRef
		copy(br.Hash[:], raw)
		br.Offset = uint64(off)
		br.Size = uint32(sz)
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file_block_refs: %w", err)
	}
	return out, nil
}

// ============================================================================
// Test capability: RawSQLAccessor
// ============================================================================

// RawSQLAccessor is an optional capability backends may implement to expose
// a small set of test-only direct-SQL helpers. Used by Phase 12
// postgres_blockref_test.go to assert FK cascade behavior.
type RawSQLAccessor interface {
	// CountFileBlockRefs returns the number of file_block_refs rows for
	// fileID. Test-only — never call this from production code.
	CountFileBlockRefs(ctx context.Context, fileID uuid.UUID) (int, error)
}

// CountFileBlockRefs implements RawSQLAccessor for *PostgresMetadataStore.
func (s *PostgresMetadataStore) CountFileBlockRefs(ctx context.Context, fileID uuid.UUID) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM file_block_refs WHERE file_id = $1`, fileID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count file_block_refs: %w", err)
	}
	return n, nil
}
