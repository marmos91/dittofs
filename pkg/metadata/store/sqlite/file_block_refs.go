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
// All helpers operate against a pgx.Tx so that UpdateAttrs's ChunkRef replace
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
//
// scope, when non-nil, restricts the whole delta to those offsets — see
// fileChunkRefsDelta. The returned count is how many stored rows the diff had
// to read, which is what a scope bounds.
func putFileChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool, scope []uint64) (bool, int, error) {
	upserts, deletes, scanned, err := fileChunkRefsDelta(ctx, tx, fileID, blocks, hasPriorRefs, scope)
	if err != nil {
		return false, scanned, err
	}
	if len(upserts) == 0 && len(deletes) == 0 {
		return false, scanned, nil
	}
	// DELETE removed offsets, then UPSERT changed/new ones. Order is
	// immaterial (disjoint offset sets) but delete-first keeps a shrink's row
	// count from transiently peaking.
	if err := deleteChunkRefOffsets(ctx, tx, fileID, deletes); err != nil {
		return false, scanned, err
	}
	if err := upsertChunkRefs(ctx, tx, fileID, upserts); err != nil {
		return false, scanned, err
	}
	return true, scanned, nil
}

// storedChunkRef is a stored file_block_refs row minus its offset (the map key).
type storedChunkRef struct {
	size  int32
	start int32
	hash  []byte
}

// fileChunkRefsDelta diffs blocks against the rows currently stored for fileID
// and returns the refs to UPSERT (new offset, or changed size/start/hash) and the
// stored offsets to DELETE (absent from blocks). When hasPriorRefs is false the
// stored set is known-empty, so the query is skipped and every ref is an
// upsert. Offsets are unique under the (file_id, "offset") PK, so keying the
// diff on offset is sound.
//
// scope, when non-nil, is the caller's promise that no offset outside it can
// differ from what is stored. The stored-row query, the incoming scan and the
// delete set are then all confined to it, so a commit that touches a handful of
// chunks costs a handful of rows instead of the file's entire manifest. A nil
// scope keeps the full diff, which is what a caller that re-derived the
// manifest from scratch needs.
func fileChunkRefsDelta(ctx context.Context, tx execer, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool, scope []uint64) ([]block.ChunkRef, []int64, int, error) {
	var inScope map[int64]struct{}
	if scope != nil {
		inScope = make(map[int64]struct{}, len(scope))
		for _, off := range scope {
			inScope[int64(off)] = struct{}{}
		}
	}

	stored := make(map[int64]storedChunkRef)
	if hasPriorRefs {
		if err := scanStoredChunkRefs(ctx, tx, fileID, inScope, stored); err != nil {
			return nil, nil, len(stored), err
		}
	}

	var upserts []block.ChunkRef
	incoming := make(map[int64]struct{}, len(blocks))
	for _, b := range blocks {
		off := int64(b.Offset)
		if inScope != nil {
			if _, ok := inScope[off]; !ok {
				continue
			}
		}
		incoming[off] = struct{}{}
		if s, ok := stored[off]; ok && s.size == int32(b.Size) && s.start == int32(b.StartOffset) && bytes.Equal(s.hash, b.Hash[:]) {
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
	return upserts, deletes, len(stored), nil
}

// scanStoredChunkRefs loads the stored rows for fileID into out. A nil inScope
// reads the whole manifest; otherwise only those offsets are read, in IN-list
// batches capped the same way as the write helpers so the bound-parameter count
// stays under SQLite's default limit.
func scanStoredChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID, inScope map[int64]struct{}, out map[int64]storedChunkRef) error {
	const selectRefs = `SELECT "offset", size, start_offset, hash FROM file_block_refs WHERE file_id = ?`
	if inScope == nil {
		return scanChunkRefBatch(ctx, tx, fileID, selectRefs, []any{fileID}, out)
	}

	offsets := make([]int64, 0, len(inScope))
	for off := range inScope {
		offsets = append(offsets, off)
	}
	const perBatch = 200
	for start := 0; start < len(offsets); start += perBatch {
		batch := offsets[start:min(start+perBatch, len(offsets))]
		var sb strings.Builder
		sb.WriteString(selectRefs)
		sb.WriteString(` AND "offset" IN (`)
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
		if err := scanChunkRefBatch(ctx, tx, fileID, sb.String(), args, out); err != nil {
			return err
		}
	}
	return nil
}

func scanChunkRefBatch(ctx context.Context, tx execer, fileID uuid.UUID, query string, args []any, out map[int64]storedChunkRef) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query file_block_refs for %s: %w", fileID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var off int64
		var sz, start int32
		var raw []byte
		if err := rows.Scan(&off, &sz, &start, &raw); err != nil {
			return fmt.Errorf("scan file_block_ref: %w", err)
		}
		h := make([]byte, len(raw))
		copy(h, raw)
		out[off] = storedChunkRef{size: sz, start: start, hash: h}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate file_block_refs: %w", err)
	}
	return nil
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
// capped so the bound-parameter count stays under SQLite's default limit; 5
// columns per row → 200 rows = 1000 params. Incoming offsets are unique under
// the (file_id, "offset") PK, so no batch upserts the same row twice.
func upsertChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID, refs []block.ChunkRef) error {
	const colsPerRow = 5
	const rowsPerBatch = 200
	for start := 0; start < len(refs); start += rowsPerBatch {
		end := start + rowsPerBatch
		if end > len(refs) {
			end = len(refs)
		}
		batch := refs[start:end]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO file_block_refs (file_id, "offset", size, start_offset, hash) VALUES `)
		args := make([]any, 0, len(batch)*colsPerRow)
		for i, b := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?, ?, ?, ?, ?)")
			args = append(args, fileID, int64(b.Offset), int32(b.Size), int32(b.StartOffset), b.Hash[:])
		}
		sb.WriteString(` ON CONFLICT (file_id, "offset") DO UPDATE SET size = excluded.size, start_offset = excluded.start_offset, hash = excluded.hash`)
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("upsert file_block_refs batch: %w", err)
		}
	}
	return nil
}

// PutFileChunkRefsCallCount returns how many times UpdateAttrs actually wrote
// file_block_refs rows — the delta upserted or deleted at least one row —
// since store open. Test-only — proves attr-only writes and no-op
// re-projections of an unchanged manifest perform ZERO manifest writes.
func (s *SQLiteMetadataStore) PutFileChunkRefsCallCount() int64 {
	return s.manifestWrites.Load()
}

// PutFileChunkRefsManifestRowsScanned returns how many stored file_block_refs
// rows UpdateAttrs's manifest diff has read since store open. Test-only — proves a
// scoped commit's read cost tracks the changed offsets, not the file's total
// chunk count.
func (s *SQLiteMetadataStore) PutFileChunkRefsManifestRowsScanned() int64 {
	return s.manifestRowsScanned.Load()
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
