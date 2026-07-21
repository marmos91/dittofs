package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"sort"
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

// putFileChunkRefs replaces all rows in file_block_refs for fileID with
// the given blocks. Atomic when called inside a pgx.Tx (the caller's tx).
//
// Implementation: DELETE+INSERT. Engine-bug paths are defended by the
// (file_id, "offset") PK — a duplicate offset would be rejected. The
// DELETE first ensures stale offsets from a prior list are not left
// behind when the new list is shorter.
func putFileChunkRefs(ctx context.Context, tx execer, fileID uuid.UUID, blocks []block.ChunkRef) error {
	if _, err := tx.Exec(ctx, `DELETE FROM file_block_refs WHERE file_id = ?1`, fileID); err != nil {
		return fmt.Errorf("delete file_block_refs for %s: %w", fileID, err)
	}
	if len(blocks) == 0 {
		return nil
	}
	// Multi-row INSERT: one statement per batch instead of one Exec per ref
	// (#1715 #8b). Batches are capped so the bound-parameter count stays under
	// SQLite's default limit (SQLITE_MAX_VARIABLE_NUMBER); 4 columns per row →
	// 200 rows = 800 params. The (file_id, "offset") PK rejects a duplicate
	// offset within any batch.
	const colsPerRow = 4
	const rowsPerBatch = 200
	for start := 0; start < len(blocks); start += rowsPerBatch {
		end := start + rowsPerBatch
		if end > len(blocks) {
			end = len(blocks)
		}
		batch := blocks[start:end]
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
		if _, err := tx.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("insert file_block_refs batch: %w", err)
		}
	}
	return nil
}

// fileChunkRefsUnchanged reports whether the file_block_refs rows already
// stored for fileID exactly match blocks — same count, offsets, sizes, and
// hashes. When true, a PutFile carrying an identical manifest can skip the
// DELETE+INSERT rewrite: the stored rows are already correct. This is the
// common case for an in-place overwrite that reuses the same chunk
// boundaries and re-projects the same manifest.
//
// Any difference (count, offset, size, or hash) returns false, so the
// caller performs the full rewrite — false is the safe default: a spurious
// false only costs a rewrite that was already the previous behaviour, while
// a spurious true would drop a real change. The incoming list is compared in
// offset order (offsets are unique under the (file_id, "offset") PK, giving a
// total order that matches the stored rows' ORDER BY "offset").
func fileChunkRefsUnchanged(ctx context.Context, tx execer, fileID uuid.UUID, blocks []block.ChunkRef) (bool, error) {
	// Canonicalise the incoming list into offset order without reordering the
	// caller's slice.
	want := make([]block.ChunkRef, len(blocks))
	copy(want, blocks)
	sort.Slice(want, func(i, j int) bool { return want[i].Offset < want[j].Offset })

	rows, err := tx.Query(ctx,
		`SELECT "offset", size, hash FROM file_block_refs WHERE file_id = ?1 ORDER BY "offset" ASC`,
		fileID,
	)
	if err != nil {
		return false, fmt.Errorf("query file_block_refs for %s: %w", fileID, err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var off int64
		var sz int32
		var raw []byte
		if err := rows.Scan(&off, &sz, &raw); err != nil {
			return false, fmt.Errorf("scan file_block_ref: %w", err)
		}
		if i >= len(want) {
			// More stored rows than incoming — the manifest shrank.
			return false, nil
		}
		w := want[i]
		if uint64(off) != w.Offset || uint32(sz) != w.Size || !bytes.Equal(raw, w.Hash[:]) {
			return false, nil
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate file_block_refs: %w", err)
	}
	// Equal only when every incoming ref was matched by a stored row.
	return i == len(want), nil
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
