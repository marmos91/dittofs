package sql

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/block"
)

// file_block_refs stores a file's []block.ChunkRef as one row per reference
// rather than a document column on the inode, so writing a manifest does not
// rewrite the inode row (and, on postgres, its TOAST chain) as well.
//
// Every statement here is spelled once: the columns, the predicates and the
// conflict clause are identical in both dialects, and only the placeholders
// differ, which Dialect.Placeholder supplies.
//
// Schema lives in migrations/000012_file_block_refs.up.sql.

// chunkRefCols are the four stored columns other than file_id, in the order
// every scan below reads them.
const chunkRefCols = `"offset", size, start_offset, hash`

// maxBoundParams caps how many parameters one generated statement binds.
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999 where postgres allows
// 65535, so the lower ceiling governs both; the margin leaves room for a
// statement that binds a value outside the per-row group.
//
// ponytail: one ceiling for both dialects. Postgres only pays an extra
// round-trip past 180 changed chunks in a single commit; give it its own
// ceiling if a profile ever shows that boundary being crossed.
const maxBoundParams = 900

// perBatchOffsets caps how many offsets one generated IN-list carries. It is
// well under maxBoundParams because these statements bind one parameter per
// offset plus the file id, not a row's worth each.
const perBatchOffsets = 200

// StoredChunkRef is a stored file_block_refs row minus its offset, which is the
// key it is stored under.
type StoredChunkRef struct {
	Size  int32
	Start int32
	Hash  []byte
}

// PutFileChunkRefs brings the stored file_block_refs rows for fileID into
// agreement with blocks by writing only the rows that actually differ, and
// reports whether it wrote any. It is atomic when reached through a
// transaction's Core.
//
// Rather than rewriting the whole manifest on every data write, it diffs the
// incoming list against the stored rows — keyed by the unique (file_id,
// "offset") primary key — and applies a targeted delta:
//   - an offset in blocks whose (size, start, hash) differs from the stored
//     row, or that has no stored row at all, is upserted;
//   - an offset that is stored but absent from blocks is deleted, so a shrink
//     or truncate leaves no stale higher-offset rows behind;
//   - an offset whose stored triple already matches is left untouched.
//
// The resulting rows are byte-identical to what a full DELETE+INSERT of blocks
// would leave; only the write volume shrinks. When nothing differs it reports
// false and touches no rows, which is the common case for an in-place
// overwrite that reuses the same chunk boundaries.
//
// hasPriorRefs lets a freshly-inserted file skip the stored-row query: with no
// prior rows every incoming ref is a plain insert.
//
// scope, when non-nil, restricts the whole delta to those offsets — see
// chunkRefsDelta. The returned count is how many stored rows the diff had to
// read, which is what scope bounds.
func (c *Core) PutFileChunkRefs(ctx context.Context, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool, scope []uint64) (bool, int, error) {
	upserts, deletes, scanned, err := c.chunkRefsDelta(ctx, fileID, blocks, hasPriorRefs, scope)
	if err != nil {
		return false, scanned, err
	}
	if len(upserts) == 0 && len(deletes) == 0 {
		return false, scanned, nil
	}

	// Delete the removed offsets, then upsert the changed and new ones. The
	// order is immaterial — the two offset sets are disjoint — but deleting
	// first keeps a shrink's row count from transiently peaking.
	if err := c.deleteChunkRefOffsets(ctx, fileID, deletes); err != nil {
		return false, scanned, err
	}
	if err := c.upsertChunkRefs(ctx, fileID, upserts); err != nil {
		return false, scanned, err
	}
	return true, scanned, nil
}

// chunkRefsDelta diffs blocks against the rows currently stored for fileID and
// returns the refs to upsert and the stored offsets to delete. When
// hasPriorRefs is false the stored set is known-empty, so the query is skipped
// and every ref is an upsert. Offsets are unique under the (file_id, "offset")
// primary key, so keying the diff on offset alone is sound.
//
// scope, when non-nil, is the caller's promise that no offset outside it can
// differ from what is stored. The stored-row query, the incoming scan and the
// delete set are then all confined to it, so a commit that touches a handful of
// chunks costs a handful of rows instead of the file's entire manifest. A nil
// scope keeps the full diff, which is what a caller that re-derived the
// manifest from scratch needs.
func (c *Core) chunkRefsDelta(ctx context.Context, fileID uuid.UUID, blocks []block.ChunkRef, hasPriorRefs bool, scope []uint64) ([]block.ChunkRef, []int64, int, error) {
	var inScope map[int64]struct{}
	if scope != nil {
		inScope = make(map[int64]struct{}, len(scope))
		for _, off := range scope {
			inScope[int64(off)] = struct{}{}
		}
	}

	stored := make(map[int64]StoredChunkRef)
	if hasPriorRefs {
		if err := c.scanStoredChunkRefs(ctx, fileID, inScope, stored); err != nil {
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
		if s, ok := stored[off]; ok &&
			s.Size == int32(b.Size) &&
			s.Start == int32(b.StartOffset) &&
			bytes.Equal(s.Hash, b.Hash[:]) {
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
// batches capped the same way the write helpers are.
func (c *Core) scanStoredChunkRefs(ctx context.Context, fileID uuid.UUID, inScope map[int64]struct{}, out map[int64]StoredChunkRef) error {
	selectRefs := `SELECT ` + chunkRefCols + ` FROM file_block_refs WHERE file_id = ` + c.D.Placeholder(1)
	if inScope == nil {
		return c.scanChunkRefBatch(ctx, fileID, selectRefs, []any{fileID}, out)
	}

	offsets := make([]int64, 0, len(inScope))
	for off := range inScope {
		offsets = append(offsets, off)
	}
	for _, batch := range batchOffsets(offsets) {
		var sb strings.Builder
		sb.WriteString(selectRefs)
		sb.WriteString(` AND "offset" IN (`)
		args := make([]any, 0, len(batch)+1)
		args = append(args, fileID)
		for i, off := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(c.D.Placeholder(len(args) + 1))
			args = append(args, off)
		}
		sb.WriteByte(')')
		if err := c.scanChunkRefBatch(ctx, fileID, sb.String(), args, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) scanChunkRefBatch(ctx context.Context, fileID uuid.UUID, query string, args []any, out map[int64]StoredChunkRef) error {
	rows, err := c.X.Query(ctx, query, args...)
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
		// pgx reuses the row buffer between Next calls, so the hash has to be
		// copied out before it is retained in the map.
		h := make([]byte, len(raw))
		copy(h, raw)
		out[off] = StoredChunkRef{Size: sz, Start: start, Hash: h}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate file_block_refs: %w", err)
	}
	return nil
}

// deleteChunkRefOffsets removes the given offsets for fileID, batched into
// IN-lists so the bound-parameter count stays under maxBoundParams.
func (c *Core) deleteChunkRefOffsets(ctx context.Context, fileID uuid.UUID, offsets []int64) error {
	for _, batch := range batchOffsets(offsets) {
		var sb strings.Builder
		sb.WriteString(`DELETE FROM file_block_refs WHERE file_id = `)
		sb.WriteString(c.D.Placeholder(1))
		sb.WriteString(` AND "offset" IN (`)
		args := make([]any, 0, len(batch)+1)
		args = append(args, fileID)
		for i, off := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(c.D.Placeholder(len(args) + 1))
			args = append(args, off)
		}
		sb.WriteByte(')')
		if _, err := c.X.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("delete file_block_refs offsets: %w", err)
		}
	}
	return nil
}

// upsertChunkRefs inserts-or-updates the given refs for fileID with one
// multi-row INSERT ... ON CONFLICT per batch rather than one statement per ref.
// The rows per batch are DERIVED from the column count rather than fixed, so a
// column added to the statement shrinks the batch instead of pushing it past
// maxBoundParams, where the whole Exec would fail. Incoming offsets are unique
// under the (file_id, "offset") primary key, so no batch upserts the same row
// twice.
func (c *Core) upsertChunkRefs(ctx context.Context, fileID uuid.UUID, refs []block.ChunkRef) error {
	const colsPerRow = 5
	const rowsPerBatch = maxBoundParams / colsPerRow
	for start := 0; start < len(refs); start += rowsPerBatch {
		batch := refs[start:min(start+rowsPerBatch, len(refs))]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO file_block_refs (file_id, ` + chunkRefCols + `) VALUES `)
		args := make([]any, 0, len(batch)*colsPerRow)
		for i, b := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('(')
			for col := range colsPerRow {
				if col > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(c.D.Placeholder(len(args) + col + 1))
			}
			sb.WriteByte(')')
			args = append(args, fileID, int64(b.Offset), int32(b.Size), int32(b.StartOffset), b.Hash[:])
		}
		sb.WriteString(` ON CONFLICT (file_id, "offset") DO UPDATE SET` +
			` size = excluded.size, start_offset = excluded.start_offset, hash = excluded.hash`)
		if _, err := c.X.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("upsert file_block_refs batch: %w", err)
		}
	}
	return nil
}

// batchOffsets slices offsets into IN-list-sized runs.
func batchOffsets(offsets []int64) [][]int64 {
	var batches [][]int64
	for start := 0; start < len(offsets); start += perBatchOffsets {
		batches = append(batches, offsets[start:min(start+perBatchOffsets, len(offsets))])
	}
	return batches
}

// DeleteFileChunkRefs removes every ref row for fileID. The foreign key
// cascades when the inode row is deleted, so this exists for a caller that
// needs to pre-clear the refs without dropping the row.
//
//nolint:unused // kept as the pre-clear half of the API; FK cascade handles the delete path
func (c *Core) DeleteFileChunkRefs(ctx context.Context, fileID uuid.UUID) error {
	if _, err := c.X.Exec(ctx,
		`DELETE FROM file_block_refs WHERE file_id = `+c.D.Placeholder(1), fileID,
	); err != nil {
		return fmt.Errorf("delete file_block_refs %s: %w", fileID, err)
	}
	return nil
}

// LoadFileChunkRefs loads every ref row for fileID, offset ascending, and
// returns a nil slice when the file has none.
func (c *Core) LoadFileChunkRefs(ctx context.Context, fileID uuid.UUID) ([]block.ChunkRef, error) {
	rows, err := c.X.Query(ctx,
		`SELECT `+chunkRefCols+` FROM file_block_refs WHERE file_id = `+c.D.Placeholder(1)+` ORDER BY "offset" ASC`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("query file_block_refs %s: %w", fileID, err)
	}
	defer rows.Close()

	var out []block.ChunkRef
	for rows.Next() {
		var off int64
		var sz, start int32
		var raw []byte
		if err := rows.Scan(&off, &sz, &start, &raw); err != nil {
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
		br.StartOffset = uint32(start)
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

// RawSQLAccessor is the capability a backend implements to expose a small set
// of test-only direct-SQL helpers. Both SQL stores satisfy it through the
// promoted Core methods below.
type RawSQLAccessor interface {
	// CountFileChunkRefs returns the number of file_block_refs rows for
	// fileID. Test-only — never call it from production code.
	CountFileChunkRefs(ctx context.Context, fileID uuid.UUID) (int, error)

	// InsertNullHashFileChunk inserts a file_blocks row with a NULL hash
	// column, simulating a legacy backup produced before the Put hash-gate
	// fix. Test-only — never call it from production code.
	InsertNullHashFileChunk(ctx context.Context, id string, dataSize uint32) error

	// FileChunkHashHex returns the hex hash string stored on the file_blocks
	// row for id, or "" when the hash column is NULL. Test-only — never call
	// it from production code.
	FileChunkHashHex(ctx context.Context, id string) (string, error)
}

// CountFileChunkRefs implements RawSQLAccessor.
func (c *Core) CountFileChunkRefs(ctx context.Context, fileID uuid.UUID) (int, error) {
	var n int
	err := c.X.QueryRow(ctx,
		`SELECT COUNT(*) FROM file_block_refs WHERE file_id = `+c.D.Placeholder(1), fileID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count file_block_refs: %w", err)
	}
	return n, nil
}

// InsertNullHashFileChunk implements RawSQLAccessor.
func (c *Core) InsertNullHashFileChunk(ctx context.Context, id string, dataSize uint32) error {
	_, err := c.X.Exec(ctx,
		`INSERT INTO file_blocks (id, hash, data_size, ref_count, state)
		 VALUES (`+c.D.Placeholder(1)+`, NULL, `+c.D.Placeholder(2)+`, 1, 0)
		 ON CONFLICT (id) DO UPDATE SET hash = NULL`,
		id, int32(dataSize),
	)
	if err != nil {
		return fmt.Errorf("insert null-hash file_block: %w", err)
	}
	return nil
}

// FileChunkHashHex implements RawSQLAccessor.
func (c *Core) FileChunkHashHex(ctx context.Context, id string) (string, error) {
	var hash *string
	err := c.X.QueryRow(ctx,
		`SELECT hash FROM file_blocks WHERE id = `+c.D.Placeholder(1), id,
	).Scan(&hash)
	if err != nil {
		return "", fmt.Errorf("read file_block hash: %w", err)
	}
	if hash == nil {
		return "", nil
	}
	return *hash, nil
}
