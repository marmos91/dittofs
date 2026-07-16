package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/backup"
)

// sqliteEngineTag identifies SQLite-produced backup streams; RestoreSnapshot refuses a
// stream tagged for a different engine.
const sqliteEngineTag = "sqlite"

// sqliteSchemaVersion is bumped whenever the on-disk schema changes in a way
// that makes an older backup unrestorable. RestoreSnapshot rejects a mismatch.
// v2 removes the dead rollup_offsets section post-journal-switchover. Migration
// 000006 also drops local_chunk_index, but that table was never in this backup
// (it is not listed in backupTables — it was pg-snapshot-only), so dropping it
// does not change the sqlite snapshot payload and only rollup_offsets bumps this.
const sqliteSchemaVersion uint32 = 2

// backupTables lists every table dumped by WriteSnapshot and reloaded by RestoreSnapshot, in a
// FK-safe order for restore (parents before children). inodes must precede
// parent_child_map / file_block_refs / locks / durable_handles (all reference
// inodes), and shares references inodes(root_file_id).
var backupTables = []string{
	"inodes",
	"shares",
	"filesystem_meta",
	"parent_child_map",
	"file_blocks",
	"file_block_refs",
	"locks",
	"nsm_client_registrations",
	"durable_handles",
	"v4_client_recovery",
	"synced_hashes",
	"server_config",
	"server_epoch",
	"filesystem_capabilities",
}

// isKnownTable guards Restore against an unexpected table name in the stream.
func isKnownTable(name string) bool {
	for _, t := range backupTables {
		if t == name {
			return true
		}
	}
	return false
}

// Cell value-kind tags for the self-describing per-row encoding.
const (
	cellNull  byte = 0
	cellInt   byte = 1
	cellFloat byte = 2
	cellText  byte = 3
	cellBlob  byte = 4
)

// Compile-time assertion: SQLiteMetadataStore implements Snapshotable.
var _ metadata.Snapshotable = (*SQLiteMetadataStore)(nil)

// WriteSnapshot serializes the entire metadata database into a CRC-protected envelope
// and returns the set of distinct content hashes referenced by file_block_refs
// (so the caller can pin the corresponding blocks). It is a logical, row-based
// export — portable across SQLite file formats and independent of the on-disk
// page layout.
func (s *SQLiteMetadataStore) WriteSnapshot(ctx context.Context, w io.Writer) (*block.HashSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", metadata.ErrSnapshotAborted, err)
	}

	// A single read transaction gives a consistent snapshot across all tables.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("backup: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	envW, err := backup.NewWriter(w, sqliteEngineTag)
	if err != nil {
		return nil, fmt.Errorf("backup: create envelope: %w", err)
	}

	var verBuf [4]byte
	binary.LittleEndian.PutUint32(verBuf[:], sqliteSchemaVersion)
	if _, err := envW.Write(verBuf[:]); err != nil {
		return nil, fmt.Errorf("backup: write schema version: %w", err)
	}

	var countBuf [4]byte
	binary.LittleEndian.PutUint32(countBuf[:], uint32(len(backupTables)))
	if _, err := envW.Write(countBuf[:]); err != nil {
		return nil, fmt.Errorf("backup: write table count: %w", err)
	}

	for _, table := range backupTables {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", metadata.ErrSnapshotAborted, err)
		}
		if err := backupTable(ctx, tx, envW, table); err != nil {
			return nil, fmt.Errorf("backup: table %s: %w", table, err)
		}
	}

	hs, err := extractHashes(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("backup: extract hashes: %w", err)
	}

	if err := envW.Finish(); err != nil {
		return nil, fmt.Errorf("backup: finish envelope: %w", err)
	}
	return hs, nil
}

// backupTable streams every row of one table as a self-describing section:
//
//	name_len(u16) name
//	col_count(u16) [ col_name_len(u16) col_name ]*
//	row_count(u32) [ row ]*    where each row is col_count tagged cells.
func backupTable(ctx context.Context, tx *sql.Tx, w io.Writer, table string) error {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("columns: %w", err)
	}

	if err := writeString(w, table); err != nil {
		return err
	}
	if err := writeU16(w, uint16(len(cols))); err != nil {
		return err
	}
	for _, c := range cols {
		if err := writeString(w, c); err != nil {
			return err
		}
	}

	// Buffer the rows so the row count can be written before the rows. Metadata
	// row counts are bounded by the share's inode count; this matches the
	// Postgres backend buffering a table's COPY output in memory.
	var rowBuf []byte
	var n uint32
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		for _, v := range cells {
			rowBuf = appendCell(rowBuf, v)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate: %w", err)
	}

	if err := writeU32(w, n); err != nil {
		return err
	}
	if _, err := w.Write(rowBuf); err != nil {
		return err
	}
	return nil
}

// extractHashes collects the distinct content hashes referenced by
// file_block_refs so the caller can pin the corresponding blocks.
func extractHashes(ctx context.Context, tx *sql.Tx) (*block.HashSet, error) {
	// Exclude unlinked (nlink=0) files: they are dead and GC may have already
	// reclaimed their blocks, so a manifest listing them would reference hashes
	// absent from remote and fail the snapshot durability verify (#1433).
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT fbr.hash FROM file_block_refs fbr
JOIN inodes i ON fbr.file_id = i.id
WHERE i.nlink > 0`)
	if err != nil {
		return nil, fmt.Errorf("query hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hs := block.NewHashSet(0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan hash: %w", err)
		}
		if len(raw) != block.HashSize {
			return nil, fmt.Errorf("hash has unexpected length %d (want %d)", len(raw), block.HashSize)
		}
		var ch block.ContentHash
		copy(ch[:], raw)
		hs.Add(ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hashes: %w", err)
	}
	return hs, nil
}

// RestoreSnapshot replaces the (must-be-empty) database contents from a snapshot
// stream produced by WriteSnapshot. It verifies the envelope CRC before committing so a
// truncated or corrupt stream never leaves a partial state.
func (s *SQLiteMetadataStore) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore cancelled: %w", err)
	}

	var hasShares bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shares)`).Scan(&hasShares); err != nil {
		return fmt.Errorf("restore: check empty: %w", err)
	}
	if hasShares {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	engineTag, payloadR, acc, err := backup.ReadHeader(r)
	if err != nil {
		return fmt.Errorf("%w: envelope: %v", metadata.ErrRestoreCorrupt, err)
	}
	if err := backup.VerifyEngine(engineTag, sqliteEngineTag); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	var verBuf [4]byte
	if _, err := io.ReadFull(payloadR, verBuf[:]); err != nil {
		return fmt.Errorf("%w: read schema version: %v", metadata.ErrRestoreCorrupt, err)
	}
	if v := binary.LittleEndian.Uint32(verBuf[:]); v != sqliteSchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", metadata.ErrSchemaVersionMismatch, v, sqliteSchemaVersion)
	}

	var countBuf [4]byte
	if _, err := io.ReadFull(payloadR, countBuf[:]); err != nil {
		return fmt.Errorf("%w: read table count: %v", metadata.ErrRestoreCorrupt, err)
	}
	tableCount := binary.LittleEndian.Uint32(countBuf[:])

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("restore: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := truncateAllTables(ctx, tx); err != nil {
		return fmt.Errorf("%w: truncate: %v", metadata.ErrRestoreCorrupt, err)
	}

	for i := uint32(0); i < tableCount; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
		}
		if err := restoreTable(ctx, tx, payloadR); err != nil {
			return fmt.Errorf("%w: table %d: %v", metadata.ErrRestoreCorrupt, i, err)
		}
	}

	// Verify the CRC BEFORE committing so a corrupt tail aborts the restore.
	if err := backup.VerifyCRC(r, acc); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("restore: commit: %w", err)
	}
	committed = true

	// Re-seed the in-memory used-bytes / quota counters from the restored rows.
	if err := s.initUsedBytesCounter(ctx); err != nil {
		return fmt.Errorf("restore: reinitialize used-bytes counter: %w", err)
	}
	// The cached store_id was read at open; the restored stream may carry a
	// different one, so re-read it from server_config (the row Restore just
	// reloaded) so GetStoreID reflects the restored database without a reopen.
	sid, err := s.ensureStoreID(ctx)
	if err != nil {
		return fmt.Errorf("restore: re-read store_id: %w", err)
	}
	s.storeID = sid
	return nil
}

// restoreTable reads one table section and re-inserts its rows.
func restoreTable(ctx context.Context, tx *sql.Tx, r io.Reader) error {
	table, err := readString(r)
	if err != nil {
		return fmt.Errorf("read name: %w", err)
	}
	if !isKnownTable(table) {
		return fmt.Errorf("unknown table %q in backup stream", table)
	}

	colCount, err := readU16(r)
	if err != nil {
		return fmt.Errorf("read col count: %w", err)
	}
	cols := make([]string, colCount)
	for i := range cols {
		if cols[i], err = readString(r); err != nil {
			return fmt.Errorf("read col name: %w", err)
		}
	}

	rowCount, err := readU32(r)
	if err != nil {
		return fmt.Errorf("read row count: %w", err)
	}
	if rowCount == 0 {
		return nil
	}

	insertSQL := buildInsert(table, cols)
	for i := uint32(0); i < rowCount; i++ {
		vals := make([]any, colCount)
		for c := 0; c < int(colCount); c++ {
			if vals[c], err = readCell(r); err != nil {
				return fmt.Errorf("read cell: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, insertSQL, vals...); err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
	}
	return nil
}

// truncateAllTables empties every table in reverse FK order. SQLite has no
// TRUNCATE; a plain DELETE with foreign_keys enabled is sufficient and the
// reverse order avoids tripping ON DELETE RESTRICT (shares.root_file_id).
func truncateAllTables(ctx context.Context, tx *sql.Tx) error {
	for i := len(backupTables) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+backupTables[i]); err != nil {
			return fmt.Errorf("delete from %s: %w", backupTables[i], err)
		}
	}
	return nil
}

// buildInsert renders `INSERT INTO t (c1,c2,...) VALUES (?,?,...)`.
func buildInsert(table string, cols []string) string {
	q := `INSERT INTO ` + quoteIdent(table) + ` (`
	for i, c := range cols {
		if i > 0 {
			q += ", "
		}
		q += quoteIdent(c)
	}
	q += `) VALUES (`
	for i := range cols {
		if i > 0 {
			q += ", "
		}
		q += "?"
	}
	q += `)`
	return q
}

// quoteIdent double-quotes a SQL identifier, doubling embedded quotes. Table and
// column names here come from the fixed backupTables list and the dumped
// column metadata, never operator input.
func quoteIdent(id string) string {
	out := make([]byte, 0, len(id)+2)
	out = append(out, '"')
	for i := 0; i < len(id); i++ {
		if id[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, id[i])
	}
	out = append(out, '"')
	return string(out)
}

// --------------------------------------------------------------------------
// Self-describing cell / primitive codec
// --------------------------------------------------------------------------

func appendCell(buf []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(buf, cellNull)
	case int64:
		buf = append(buf, cellInt)
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(t))
		return append(buf, b[:]...)
	case float64:
		buf = append(buf, cellFloat)
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(t))
		return append(buf, b[:]...)
	case []byte:
		buf = append(buf, cellBlob)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(t)))
		buf = append(buf, b[:]...)
		return append(buf, t...)
	case string:
		buf = append(buf, cellText)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(t)))
		buf = append(buf, b[:]...)
		return append(buf, []byte(t)...)
	case bool:
		buf = append(buf, cellInt)
		var b [8]byte
		if t {
			binary.LittleEndian.PutUint64(b[:], 1)
		}
		return append(buf, b[:]...)
	default:
		// Should not happen: database/sql yields nil/int64/float64/[]byte/string
		// for SQLite columns. Encode the fmt string as text for resilience.
		s := fmt.Sprintf("%v", t)
		buf = append(buf, cellText)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(s)))
		buf = append(buf, b[:]...)
		return append(buf, []byte(s)...)
	}
}

func readCell(r io.Reader) (any, error) {
	var kind [1]byte
	if _, err := io.ReadFull(r, kind[:]); err != nil {
		return nil, err
	}
	switch kind[0] {
	case cellNull:
		return nil, nil
	case cellInt:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		return int64(binary.LittleEndian.Uint64(b[:])), nil
	case cellFloat:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b[:])), nil
	case cellText:
		n, err := readU32(r)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf), nil
	case cellBlob:
		n, err := readU32(r)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	default:
		return nil, fmt.Errorf("unknown cell kind %d", kind[0])
	}
}

func writeString(w io.Writer, s string) error {
	if err := writeU16(w, uint16(len(s))); err != nil {
		return err
	}
	_, err := w.Write([]byte(s))
	return err
}

func readString(r io.Reader) (string, error) {
	n, err := readU16(r)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeU16(w io.Writer, v uint16) error {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func writeU32(w io.Writer, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}
