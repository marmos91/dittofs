package postgres

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/block"
)

// ============================================================================
// file_block_refs CRUD
// ============================================================================
//
// Stores FileAttr.Blocks []block.ChunkRef rows for files. Per
// we use a separate table (not JSONB on files) to avoid TOAST write
// amplification on the VM-primary workload.
//
// All helpers operate against a pgx.Tx so that PutFile's ChunkRef replace
// happens atomically with the files row UPDATE.
//
// Schema lives in migrations/000012_file_block_refs.up.sql.

// putFileChunkRefs brings the file_block_refs rows for fileID into agreement
// with blocks by writing only the rows that actually differ, and reports
// whether any row was written. Atomic when called inside the caller's pgx.Tx.
//
// Rather than rewriting the whole manifest on every data write, it diffs the
// incoming list against the stored rows (keyed by the unique (file_id,
// "offset") PK) and applies a targeted delta:
//   - offsets present in blocks whose (size, hash) differ from the stored row,
//     or that have no stored row, are UPSERTed;
//   - offsets stored but absent from blocks are DELETEd (a shrink/truncate must
//     not leave stale higher-offset rows behind);
//   - offsets whose stored (size, hash) already match are left untouched.
//
// The resulting rows are byte-identical to a full DELETE+INSERT of blocks —
// only the write volume shrinks. When nothing differs it returns false and
// touches no rows. The delete plus every upsert ship in a single SendBatch to
// keep round-trips down.
//
// hasPriorRefs lets a freshly-inserted file skip the stored-row query: with no
// prior rows every incoming ref is a plain insert.
func putFileChunkRefs(ctx context.Context, tx pgx.Tx, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool) (bool, error) {
	upserts, deletes, err := fileChunkRefsDelta(ctx, tx, fileID, blocks, hasPriorRefs)
	if err != nil {
		return false, err
	}
	if len(upserts) == 0 && len(deletes) == 0 {
		return false, nil
	}

	batch := &pgx.Batch{}
	if len(deletes) > 0 {
		// A single ANY() carries the whole removed-offset set as one array
		// parameter, so a shrink of any size is one statement.
		batch.Queue(
			`DELETE FROM file_block_refs WHERE file_id = $1 AND "offset" = ANY($2)`,
			fileID, deletes,
		)
	}
	for _, b := range upserts {
		batch.Queue(
			`INSERT INTO file_block_refs (file_id, "offset", size, hash) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (file_id, "offset") DO UPDATE SET size = excluded.size, hash = excluded.hash`,
			fileID, int64(b.Offset), int32(b.Size), b.Hash[:],
		)
	}

	br := tx.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()
	statements := len(upserts)
	if len(deletes) > 0 {
		statements++
	}
	for i := 0; i < statements; i++ {
		if _, err := br.Exec(); err != nil {
			return false, fmt.Errorf("apply file_block_refs delta: %w", err)
		}
	}
	return true, nil
}

// storedChunkRef is a stored file_block_refs row minus its offset (the map key).
type storedChunkRef struct {
	size int32
	hash []byte
}

// fileChunkRefsDelta diffs blocks against the rows currently stored for fileID
// and returns the refs to UPSERT (new offset, or changed size/hash) and the
// stored offsets to DELETE (absent from blocks). When hasPriorRefs is false the
// stored set is known-empty, so the query is skipped and every ref is an
// upsert. Offsets are unique under the (file_id, "offset") PK, so keying the
// diff on offset is sound.
func fileChunkRefsDelta(ctx context.Context, tx pgx.Tx, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool) ([]block.ChunkRef, []int64, error) {
	stored := make(map[int64]storedChunkRef)
	if hasPriorRefs {
		rows, err := tx.Query(ctx,
			`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = $1`,
			fileID,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("query file_block_refs for %s: %w", fileID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var off int64
			var sz int32
			var raw []byte
			if err := rows.Scan(&off, &sz, &raw); err != nil {
				return nil, nil, fmt.Errorf("scan file_block_ref: %w", err)
			}
			h := make([]byte, len(raw))
			copy(h, raw)
			stored[off] = storedChunkRef{size: sz, hash: h}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("iterate file_block_refs: %w", err)
		}
	}

	var upserts []block.ChunkRef
	incoming := make(map[int64]struct{}, len(blocks))
	for _, b := range blocks {
		off := int64(b.Offset)
		incoming[off] = struct{}{}
		if s, ok := stored[off]; ok && s.size == int32(b.Size) && bytes.Equal(s.hash, b.Hash[:]) {
			continue // identical row already stored — no write
		}
		upserts = append(upserts, b)
	}
	var deletes []int64
	for off := range stored {
		if _, ok := incoming[off]; !ok {
			deletes = append(deletes, off)
		}
	}
	return upserts, deletes, nil
}

// PutFileChunkRefsCallCount returns how many times PutFile actually wrote
// file_block_refs rows — the delta upserted or deleted at least one row —
// since store open. Test-only — proves attr-only writes and no-op
// re-projections of an unchanged manifest perform ZERO manifest writes.
func (s *PostgresMetadataStore) PutFileChunkRefsCallCount() int64 {
	return s.manifestWrites.Load()
}

// deleteFileChunkRefs removes all rows for fileID. The FK cascade
// handles this automatically when the files row is deleted; this helper
// is exposed for callers that pre-clear refs without dropping the row.
//
// the file-delete path today, but plan-defined future callers may need
// pre-clear semantics.
//
//nolint:unused // exported as part of the API surface; FK cascade handles
func deleteFileChunkRefs(ctx context.Context, tx pgx.Tx, fileID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM file_block_refs WHERE file_id = $1`, fileID); err != nil {
		return fmt.Errorf("delete file_block_refs for %s: %w", fileID, err)
	}
	return nil
}

// loadFileChunkRefs loads all rows for fileID via the pool (not a tx),
// ordered by offset ASC; returns a nil slice when no rows exist.
// Used by FindByObjectID. GetFile no longer calls this — it folds the same
// rows into its metadata read via blockRefsAggExpr (#1176).
func (s *PostgresMetadataStore) loadFileChunkRefs(ctx context.Context, fileID uuid.UUID) ([]block.ChunkRef, error) {
	rows, err := s.query(ctx,
		`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = $1 ORDER BY "offset" ASC`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("query file_block_refs for %s: %w", fileID, err)
	}
	defer rows.Close()

	var out []block.ChunkRef
	for rows.Next() {
		var off int64
		var sz int32
		var raw []byte
		if err := rows.Scan(&off, &sz, &raw); err != nil {
			return nil, fmt.Errorf("scan file_block_ref: %w", err)
		}
		if len(raw) != block.HashSize {
			return nil, fmt.Errorf(
				"file_block_refs.hash for %s/%d has unexpected length %d (want %d)",
				fileID, off, len(raw), block.HashSize,
			)
		}
		var br block.ChunkRef
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
// a small set of test-only direct-SQL helpers. Used by
// postgres_blockref_test.go to assert FK cascade behavior.
type RawSQLAccessor interface {
	// CountFileChunkRefs returns the number of file_block_refs rows for
	// fileID. Test-only — never call this from production code.
	CountFileChunkRefs(ctx context.Context, fileID uuid.UUID) (int, error)

	// InsertNullHashFileChunk inserts a file_blocks row with a NULL hash
	// column, simulating a legacy backup produced before the Put
	// hash-gate fix. Test-only — never call this from production code.
	InsertNullHashFileChunk(ctx context.Context, id string, dataSize uint32) error

	// FileChunkHashHex returns the hex hash string stored on the
	// file_blocks row for id, or "" when the hash column is NULL.
	// Test-only — never call this from production code.
	FileChunkHashHex(ctx context.Context, id string) (string, error)
}

// CountFileChunkRefs implements RawSQLAccessor for *PostgresMetadataStore.
func (s *PostgresMetadataStore) CountFileChunkRefs(ctx context.Context, fileID uuid.UUID) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM file_block_refs WHERE file_id = $1`, fileID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count file_block_refs: %w", err)
	}
	return n, nil
}

// InsertNullHashFileChunk implements RawSQLAccessor for *PostgresMetadataStore.
func (s *PostgresMetadataStore) InsertNullHashFileChunk(ctx context.Context, id string, dataSize uint32) error {
	_, err := s.exec(ctx, `
		INSERT INTO file_blocks (id, hash, data_size, ref_count, state)
		VALUES ($1, NULL, $2, 1, 0)
		ON CONFLICT (id) DO UPDATE SET hash = NULL`,
		id, int32(dataSize))
	if err != nil {
		return fmt.Errorf("insert null-hash file_block: %w", err)
	}
	return nil
}

// FileChunkHashHex implements RawSQLAccessor for *PostgresMetadataStore.
func (s *PostgresMetadataStore) FileChunkHashHex(ctx context.Context, id string) (string, error) {
	var hash *string
	err := s.queryRow(ctx, `SELECT hash FROM file_blocks WHERE id = $1`, id).Scan(&hash)
	if err != nil {
		return "", fmt.Errorf("read file_block hash: %w", err)
	}
	if hash == nil {
		return "", nil
	}
	return *hash, nil
}
