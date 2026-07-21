package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
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
// whether any row was written. Atomic when called inside the caller's tx.
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
// touches no rows (the common in-place-overwrite-with-same-boundaries case).
//
// hasPriorRefs lets a freshly-inserted file skip the stored-row query: with no
// prior rows every incoming ref is a plain insert.
func putFileChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool) (bool, error) {
	upserts, deletes, err := fileChunkRefsDelta(ctx, tx, fileID, blocks, hasPriorRefs)
	if err != nil {
		return false, err
	}
	if len(upserts) == 0 && len(deletes) == 0 {
		return false, nil
	}
	// DELETE removed offsets, then UPSERT changed/new ones. Order is
	// immaterial (disjoint offset sets) but delete-first keeps a shrink's row
	// count from transiently peaking.
	if err := deleteChunkRefOffsets(ctx, tx, fileID, deletes); err != nil {
		return false, err
	}
	if err := upsertChunkRefs(ctx, tx, fileID, upserts); err != nil {
		return false, err
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
func fileChunkRefsDelta(ctx context.Context, tx execer, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool) ([]block.ChunkRef, []int64, error) {
	stored := make(map[int64]storedChunkRef)
	if hasPriorRefs {
		rows, err := tx.Query(ctx,
			`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = ?1`,
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

// deleteChunkRefOffsets removes the given offsets for fileID. Offsets are
// batched into IN-lists capped so the bound-parameter count stays under
// SQLite's default limit (SQLITE_MAX_VARIABLE_NUMBER).
func deleteChunkRefOffsets(ctx context.Context, tx execer, fileID uuid.UUID, offsets []int64) error {
	const perBatch = 200
	for start := 0; start < len(offsets); start += perBatch {
		end := start + perBatch
		if end > len(offsets) {
			end = len(offsets)
		}
		batch := offsets[start:end]
		var sb strings.Builder
		sb.WriteString(`DELETE FROM file_block_refs WHERE file_id = ? AND "offset" IN (`)
		args := make([]any, 0, len(batch)+1)
		args = append(args, fileID)
		for i, off := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
			args = append(args, off)
		}
		sb.WriteByte(')')
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("delete file_block_refs offsets: %w", err)
		}
	}
	return nil
}

// upsertChunkRefs inserts-or-updates the given refs for fileID with a multi-row
// INSERT ... ON CONFLICT per batch instead of one Exec per ref. Batches are
// capped so the bound-parameter count stays under SQLite's default limit; 4
// columns per row → 200 rows = 800 params. Incoming offsets are unique under
// the (file_id, "offset") PK, so no batch upserts the same row twice.
func upsertChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID, refs []block.ChunkRef) error {
	const colsPerRow = 4
	const rowsPerBatch = 200
	for start := 0; start < len(refs); start += rowsPerBatch {
		end := start + rowsPerBatch
		if end > len(refs) {
			end = len(refs)
		}
		batch := refs[start:end]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO file_block_refs (file_id, "offset", size, hash) VALUES `)
		args := make([]any, 0, len(batch)*colsPerRow)
		for i, b := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?, ?, ?, ?)")
			args = append(args, fileID, int64(b.Offset), int32(b.Size), b.Hash[:])
		}
		sb.WriteString(` ON CONFLICT (file_id, "offset") DO UPDATE SET size = excluded.size, hash = excluded.hash`)
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("upsert file_block_refs batch: %w", err)
		}
	}
	return nil
}

// PutFileChunkRefsCallCount returns how many times PutFile persisted the
// file_block_refs manifest (ran past the BlocksDirty gate) since store open.
// Test-only — proves attr-only writes perform ZERO manifest writes.
func (s *SQLiteMetadataStore) PutFileChunkRefsCallCount() int64 {
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
func deleteFileChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM file_block_refs WHERE file_id = ?1`, fileID); err != nil {
		return fmt.Errorf("delete file_block_refs for %s: %w", fileID, err)
	}
	return nil
}

// loadFileChunkRefs loads all rows for fileID via the pool (not a tx),
// ordered by offset ASC; returns a nil slice when no rows exist.
// Used by FindByObjectID. GetFile no longer calls this — it folds the same
// rows into its metadata read via blockRefsAggExpr (#1176).
func (s *SQLiteMetadataStore) loadFileChunkRefs(ctx context.Context, fileID uuid.UUID) ([]block.ChunkRef, error) {
	rows, err := s.query(ctx,
		`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = ?1 ORDER BY "offset" ASC`,
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

// CountFileChunkRefs implements RawSQLAccessor for *SQLiteMetadataStore.
func (s *SQLiteMetadataStore) CountFileChunkRefs(ctx context.Context, fileID uuid.UUID) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM file_block_refs WHERE file_id = ?1`, fileID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count file_block_refs: %w", err)
	}
	return n, nil
}

// InsertNullHashFileChunk implements RawSQLAccessor for *SQLiteMetadataStore.
func (s *SQLiteMetadataStore) InsertNullHashFileChunk(ctx context.Context, id string, dataSize uint32) error {
	_, err := s.exec(ctx, `
		INSERT INTO file_blocks (id, hash, data_size, ref_count, state)
		VALUES (?1, NULL, ?2, 1, 0)
		ON CONFLICT (id) DO UPDATE SET hash = NULL`,
		id, int32(dataSize))
	if err != nil {
		return fmt.Errorf("insert null-hash file_block: %w", err)
	}
	return nil
}

// FileChunkHashHex implements RawSQLAccessor for *SQLiteMetadataStore.
func (s *SQLiteMetadataStore) FileChunkHashHex(ctx context.Context, id string) (string, error) {
	var hash *string
	err := s.queryRow(ctx, `SELECT hash FROM file_blocks WHERE id = ?1`, id).Scan(&hash)
	if err != nil {
		return "", fmt.Errorf("read file_block hash: %w", err)
	}
	if hash == nil {
		return "", nil
	}
	return *hash, nil
}
