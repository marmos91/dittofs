package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/backup"
)

const (
	// postgresEngineTag identifies the Postgres backup driver in the
	// envelope header. Must match exactly during restore.
	postgresEngineTag = "postgres"

	// postgresSchemaVersion is the backup payload schema version.
	// Increment when the payload layout changes (new table, changed
	// framing, etc.) and add a migration path in Restore.
	postgresSchemaVersion = uint32(1)
)

// backupTables lists every metadata table in FK-safe dependency order
// for restore. Parent tables must be restored before children so that
// FK constraints are satisfied. The order follows the migration
// dependency chain (initial schema + later migrations).
//
// NOTE: schema_migrations is excluded -- it is owned by golang-migrate
// and recreated automatically via AutoMigrate on store open.
var backupTables = []string{
	"server_config",
	"filesystem_capabilities",
	"files",
	"shares",
	"parent_child_map",
	"link_counts",
	"pending_writes",
	"file_block_refs",
	"file_blocks",
	"locks",
	"server_epoch",
	"nsm_client_registrations",
	"durable_handles",
	"rollup_offsets",
	"synced_hashes",
}

// Compile-time assertion: PostgresMetadataStore implements Backupable.
var _ metadata.Backupable = (*PostgresMetadataStore)(nil)

// Backup serializes all Postgres metadata tables into w using COPY TO
// STDOUT inside a single REPEATABLE READ transaction for snapshot
// isolation. Returns the set of unique block hashes referenced by the
// snapshot (extracted from file_block_refs via a dedicated COPY query).
func (s *PostgresMetadataStore) Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
	}

	// Acquire a dedicated connection for raw protocol-level COPY operations.
	acquireCtx, acquireCancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer acquireCancel()

	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		return nil, fmt.Errorf("backup: acquire connection: %w", err)
	}
	defer conn.Release()

	raw := conn.Conn().PgConn()

	// Begin REPEATABLE READ transaction for snapshot isolation.
	// All COPY TO operations within this txn see the same snapshot,
	// satisfying the ConcurrentWriter conformance test.
	if _, err := raw.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ").ReadAll(); err != nil {
		return nil, fmt.Errorf("backup: begin txn: %w", err)
	}
	defer func() { _, _ = raw.Exec(ctx, "ROLLBACK").ReadAll() }()

	// Create envelope writer -- writes magic, version, engine tag.
	envW, err := backup.NewWriter(w, postgresEngineTag)
	if err != nil {
		return nil, fmt.Errorf("backup: create envelope: %w", err)
	}

	// Write schema version (4 bytes LE).
	var verBuf [4]byte
	binary.LittleEndian.PutUint32(verBuf[:], postgresSchemaVersion)
	if _, err := envW.Write(verBuf[:]); err != nil {
		return nil, fmt.Errorf("backup: write schema version: %w", err)
	}

	// Write table count (4 bytes LE).
	var countBuf [4]byte
	binary.LittleEndian.PutUint32(countBuf[:], uint32(len(backupTables)))
	if _, err := envW.Write(countBuf[:]); err != nil {
		return nil, fmt.Errorf("backup: write table count: %w", err)
	}

	// COPY each table to the envelope.
	for _, table := range backupTables {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
		}

		if err := backupTable(ctx, raw, envW, table); err != nil {
			return nil, fmt.Errorf("backup: table %s: %w", table, err)
		}
	}

	// Extract unique block hashes from file_block_refs within the same
	// REPEATABLE READ snapshot. Uses CSV format for simpler parsing --
	// Postgres hex-encodes BYTEA as \x... in CSV output.
	hs, err := extractHashes(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("backup: extract hashes: %w", err)
	}

	// Commit the read-only transaction.
	if _, err := raw.Exec(ctx, "COMMIT").ReadAll(); err != nil {
		return nil, fmt.Errorf("backup: commit: %w", err)
	}

	// Finalize envelope (writes trailing CRC32).
	if err := envW.Finish(); err != nil {
		return nil, fmt.Errorf("backup: finish envelope: %w", err)
	}

	return hs, nil
}

// backupTable writes one table section to the envelope writer:
//
//	table_name_len  uint16 LE
//	table_name      [table_name_len]byte
//	data_len        uint64 LE
//	data            [data_len]byte  (CSV with header from COPY TO STDOUT)
func backupTable(ctx context.Context, raw *pgconn.PgConn, envW io.Writer, table string) error {
	// COPY table data into a buffer so we can length-prefix it.
	var tableBuf bytes.Buffer
	sql := fmt.Sprintf(`COPY %s TO STDOUT WITH (FORMAT csv, HEADER true)`, table)
	if _, err := raw.CopyTo(ctx, &tableBuf, sql); err != nil {
		return fmt.Errorf("COPY TO: %w", err)
	}

	// Write table name length + name.
	nameBytes := []byte(table)
	var nameLenBuf [2]byte
	binary.LittleEndian.PutUint16(nameLenBuf[:], uint16(len(nameBytes)))
	if _, err := envW.Write(nameLenBuf[:]); err != nil {
		return fmt.Errorf("write name len: %w", err)
	}
	if _, err := envW.Write(nameBytes); err != nil {
		return fmt.Errorf("write name: %w", err)
	}

	// Write data length + data.
	var dataLenBuf [8]byte
	binary.LittleEndian.PutUint64(dataLenBuf[:], uint64(tableBuf.Len()))
	if _, err := envW.Write(dataLenBuf[:]); err != nil {
		return fmt.Errorf("write data len: %w", err)
	}
	if _, err := envW.Write(tableBuf.Bytes()); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return nil
}

// extractHashes runs a dedicated COPY query to extract unique BYTEA
// hash values from file_block_refs within the current transaction
// snapshot. Returns a HashSet of all unique ContentHash values.
//
// Uses CSV format: Postgres hex-encodes BYTEA columns as \x followed
// by hex digits. Each line is one hash value.
func extractHashes(ctx context.Context, raw *pgconn.PgConn) (*blockstore.HashSet, error) {
	var hashBuf bytes.Buffer
	sql := `COPY (SELECT DISTINCT hash FROM file_block_refs) TO STDOUT WITH (FORMAT csv)`
	if _, err := raw.CopyTo(ctx, &hashBuf, sql); err != nil {
		return nil, fmt.Errorf("COPY hash query: %w", err)
	}

	hs := blockstore.NewHashSet(0)

	data := hashBuf.String()
	if data == "" {
		return hs, nil
	}

	for _, line := range strings.Split(strings.TrimRight(data, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Postgres CSV-encodes BYTEA as \x followed by hex digits.
		hexStr := strings.TrimPrefix(line, "\\x")
		rawBytes, err := hex.DecodeString(hexStr)
		if err != nil {
			return nil, fmt.Errorf("decode hash hex %q: %w", line, err)
		}
		if len(rawBytes) != blockstore.HashSize {
			return nil, fmt.Errorf("hash has unexpected length %d (want %d)", len(rawBytes), blockstore.HashSize)
		}

		var ch blockstore.ContentHash
		copy(ch[:], rawBytes)
		hs.Add(ch)
	}

	return hs, nil
}

// Restore reads a Postgres backup stream from r and rebuilds metadata
// by COPY FROM STDIN into each table. The destination store must be
// empty (no shares); returns ErrRestoreDestinationNotEmpty otherwise.
func (s *PostgresMetadataStore) Restore(ctx context.Context, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore cancelled: %w", err)
	}

	// Check destination is empty: any share existing means non-empty.
	var hasShares bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM shares)`).Scan(&hasShares)
	if err != nil {
		return fmt.Errorf("restore: check empty: %w", err)
	}
	if hasShares {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	// Read envelope header -- validates magic, version, engine tag.
	engineTag, payloadR, acc, err := backup.ReadHeader(r)
	if err != nil {
		return fmt.Errorf("%w: envelope: %v", metadata.ErrRestoreCorrupt, err)
	}
	if err := backup.VerifyEngine(engineTag, postgresEngineTag); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Read schema version.
	var verBuf [4]byte
	if _, err := io.ReadFull(payloadR, verBuf[:]); err != nil {
		return fmt.Errorf("%w: read schema version: %v", metadata.ErrRestoreCorrupt, err)
	}
	schemaVer := binary.LittleEndian.Uint32(verBuf[:])
	if schemaVer != postgresSchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", metadata.ErrSchemaVersionMismatch, schemaVer, postgresSchemaVersion)
	}

	// Read table count.
	var countBuf [4]byte
	if _, err := io.ReadFull(payloadR, countBuf[:]); err != nil {
		return fmt.Errorf("%w: read table count: %v", metadata.ErrRestoreCorrupt, err)
	}
	tableCount := binary.LittleEndian.Uint32(countBuf[:])

	// Acquire a dedicated connection for raw COPY FROM operations.
	acquireCtx, acquireCancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
	defer acquireCancel()

	conn, err := s.pool.Acquire(acquireCtx)
	if err != nil {
		return fmt.Errorf("restore: acquire connection: %w", err)
	}
	defer conn.Release()

	pgRaw := conn.Conn().PgConn()

	// Begin transaction for atomic restore.
	if _, err := pgRaw.Exec(ctx, "BEGIN").ReadAll(); err != nil {
		return fmt.Errorf("restore: begin txn: %w", err)
	}
	defer func() { _, _ = pgRaw.Exec(ctx, "ROLLBACK").ReadAll() }()

	// Truncate all tables to clear AutoMigrate-seeded data (server_config,
	// filesystem_capabilities). CASCADE handles FK dependencies.
	if err := truncateAllTables(ctx, pgRaw); err != nil {
		return fmt.Errorf("%w: truncate: %v", metadata.ErrRestoreCorrupt, err)
	}

	// Restore each table section from the stream.
	for i := uint32(0); i < tableCount; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
		}

		if err := restoreTable(ctx, pgRaw, payloadR); err != nil {
			return fmt.Errorf("%w: table %d: %v", metadata.ErrRestoreCorrupt, i, err)
		}
	}

	// Verify CRC BEFORE committing. The tee reader has accumulated all
	// payload bytes during the restoreTable loop; the original reader r
	// still has the trailing 4 CRC bytes unread. If CRC fails, we return
	// immediately and the deferred ROLLBACK cleans up the transaction.
	if err := backup.VerifyCRC(r, acc); err != nil {
		return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
	}

	// CRC verified — commit the restore transaction.
	if _, err := pgRaw.Exec(ctx, "COMMIT").ReadAll(); err != nil {
		return fmt.Errorf("restore: commit: %w", err)
	}

	return nil
}

// truncateAllTables truncates all backup tables with CASCADE to handle
// FK constraints. This clears any data seeded by AutoMigrate
// (server_config, filesystem_capabilities).
func truncateAllTables(ctx context.Context, raw *pgconn.PgConn) error {
	// Build a single TRUNCATE statement with all tables for efficiency.
	// TRUNCATE ... CASCADE handles FK dependencies regardless of order.
	sql := "TRUNCATE " + strings.Join(backupTables, ", ") + " CASCADE"
	if _, err := raw.Exec(ctx, sql).ReadAll(); err != nil {
		return fmt.Errorf("TRUNCATE: %w", err)
	}
	return nil
}

// restoreTable reads one table section from the payload reader and
// COPY FROM STDIN into the database. Validates the table name against
// the hardcoded backupTables list to prevent SQL injection.
func restoreTable(ctx context.Context, raw *pgconn.PgConn, payloadR io.Reader) error {
	// Read table name length + name.
	var nameLenBuf [2]byte
	if _, err := io.ReadFull(payloadR, nameLenBuf[:]); err != nil {
		return fmt.Errorf("read name len: %w", err)
	}
	nameLen := binary.LittleEndian.Uint16(nameLenBuf[:])
	nameBytes := make([]byte, nameLen)
	if _, err := io.ReadFull(payloadR, nameBytes); err != nil {
		return fmt.Errorf("read name: %w", err)
	}
	tableName := string(nameBytes)

	// Validate table name against hardcoded list to prevent SQL injection
	// from crafted backup streams.
	if !isKnownTable(tableName) {
		return fmt.Errorf("unknown table %q in backup stream", tableName)
	}

	// Read data length + data.
	var dataLenBuf [8]byte
	if _, err := io.ReadFull(payloadR, dataLenBuf[:]); err != nil {
		return fmt.Errorf("read data len: %w", err)
	}
	dataLen := binary.LittleEndian.Uint64(dataLenBuf[:])

	// Reject dataLen values that would overflow when cast to int64.
	// A uint64 > math.MaxInt64 wraps to a negative int64, causing
	// LimitReader to return EOF immediately and desynchronize the
	// stream parser.
	if dataLen > uint64(math.MaxInt64) {
		return fmt.Errorf("data length %d exceeds int64 range: %w", dataLen, metadata.ErrRestoreCorrupt)
	}

	// Read exactly dataLen bytes of CSV data.
	dataReader := io.LimitReader(payloadR, int64(dataLen))

	// If no data (empty table), skip the COPY FROM.
	if dataLen == 0 {
		return nil
	}

	// COPY FROM STDIN with the same CSV format used during backup.
	sql := fmt.Sprintf(`COPY %s FROM STDIN WITH (FORMAT csv, HEADER true)`, tableName)
	if _, err := raw.CopyFrom(ctx, dataReader, sql); err != nil {
		return fmt.Errorf("COPY FROM %s: %w", tableName, err)
	}

	// Drain any unread bytes from the limited reader. CopyFrom may not
	// consume all bytes if the CSV has trailing newlines that COPY ignores.
	if _, err := io.Copy(io.Discard, dataReader); err != nil {
		return fmt.Errorf("drain %s remainder: %w", tableName, err)
	}

	return nil
}

// isKnownTable checks if the table name exists in the hardcoded
// backupTables slice. Used during restore to prevent SQL injection
// from crafted backup streams.
func isKnownTable(name string) bool {
	return slices.Contains(backupTables, name)
}
