package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileChunkStore Implementation for SQLite Store
// ============================================================================
//
// This file implements the FileChunkStore interface for the SQLite metadata store.
// It provides content-addressed file chunk tracking for deduplication and caching.
//
// The FileChunkStore interface is narrowed to 6 methods. The backend
// retains the legacy GetFileChunk + ListFileChunks helpers as
// concrete methods on the struct (not on the public interface) for
// engine-internal callers.
//
// Table:
//   - file_blocks: File block data with UUID as primary key and hash index
//
// Thread Safety: All operations use SQLite transactions for ACID guarantees.
//
// ============================================================================

// Ensure SQLiteMetadataStore implements FileChunkStore
var _ block.FileChunkStore = (*SQLiteMetadataStore)(nil)

// ============================================================================
// FileChunk Operations
// ============================================================================

// DecrementRefCountAndReap atomically decrements ref_count and, when it hits 0,
// deletes the row — both statements run inside ONE transaction so the
// decrement-and-reap is atomic and TOCTOU-free against a concurrent AddRef
// (which takes the same row lock). Returns (0, nil) when the row is already
// absent — a swept row is not a caller error. Running through WithTransaction
// also gives a SQLITE_BUSY collision the package's bounded retry instead of
// surfacing it as a hard error.
func (s *SQLiteMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var txErr error
		newCount, txErr = tx.DecrementRefCountAndReap(ctx, id)
		return txErr
	})
	if err != nil {
		return 0, err
	}
	return newCount, nil
}

// The bodies below carry the file_blocks CRUD SQL once. Both the pool path and
// the transaction path run them over their own executor, so the two surfaces
// cannot drift apart.

// fileChunkColumns is the file_blocks column list every chunk read and write
// names, in the order scanFileChunk reads them. Naming it once is what keeps a
// column added to one query from being missed by the others, or by the scan.
const fileChunkColumns = `id, hash, data_size, start_offset, ref_count, last_access, created_at, state, last_sync_attempt_at`

// insertFileChunk is the INSERT the row writers share; each appends its own
// ON CONFLICT clause.
const insertFileChunk = `INSERT INTO file_blocks (` + fileChunkColumns + `) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)`

// putFileChunkQuery omits ref_count from the ON CONFLICT update list so a
// concurrent IncrementRefCount / DecrementRefCount (atomic SQL +1 / -1 UPDATEs)
// cannot be overwritten by a stale in-memory RefCount; the INSERT path still
// writes the caller's value verbatim. hash uses COALESCE so a zero-hash Put
// never NULLs a previously persisted good hash.
const putFileChunkQuery = insertFileChunk + `
	ON CONFLICT (id) DO UPDATE SET
		hash = COALESCE(EXCLUDED.hash, file_blocks.hash),
		data_size = EXCLUDED.data_size,
		start_offset = EXCLUDED.start_offset,
		last_access = EXCLUDED.last_access,
		state = EXCLUDED.state,
		last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`

// AddRef atomically bumps RefCount on the FileChunk row(s) indexed by
// the given content hash. Implements the FileChunkStore.AddRef contract
// used by the in-memory hash dedup LRU hit path to
// reference an already-stored block without creating a new row.
//
// Atomicity: a single UPDATE statement performs the bump — the SQLite
// single-writer engine serializes contended updates against the same row,
// so AddRef is TOCTOU-free against concurrent DecrementRefCount cascade
// (matches the existing IncrementRefCount idiom).
//
// Returns metadata.ErrUnknownHash when RowsAffected == 0 (no row exists
// for this hash). Callers (the LRU hit site) fall back to the full Put
// path on this sentinel.
//
// Multi-row-per-hash tolerance:
// the hash index on file_blocks is a NON-UNIQUE partial index (migration
// 000011), so a single hash may match multiple rows in legacy data. The
// UPDATE deliberately omits LIMIT — all matching rows are bumped
// uniformly so refcount accounting stays correct regardless of which
// row a later DecrementRefCount targets. The conformance test seeds a
// single row, so RefCount goes from N to N+1 exactly.
//
// Only ref_count is mutated. block_state is never touched: AddRef
// references an existing block, and the LRU hit path never creates
// or transitions one.
func (s *SQLiteMetadataStore) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// postgres backend records ref count only — parameters intentionally
	// blanked.
	//
	// state = 2 (Remote) scoping mirrors GetByHash and the memory/badger
	// backends, whose AddRef resolves the hash only through the finalized
	// hash index. The dedup hit path references a block already confirmed
	// on the remote; a Pending row (which now also carries its hash) is
	// not a valid dedup donor, so AddRef must miss it and return
	// ErrUnknownHash exactly as before, letting the caller fall back to
	// the full Put path.
	return s.Core.AddRef(ctx, hash)
}

// listFileChunksQuery takes the bounds of block.PayloadPrefixRange and is a
// prefilter; block.ChunksForPayload decides membership and order.
//
// The range is compared and ordered in byte collation, not the database
// default: the bounds only bracket the prefix under byte ordering, and a
// byte-ordered id column lets the primary-key index seek the range instead of
// filtering the whole table.
const listFileChunksQuery = `SELECT ` + fileChunkColumns + `
	FROM file_blocks
	WHERE id >= ?1 COLLATE BINARY AND id < ?2 COLLATE BINARY
	ORDER BY id COLLATE BINARY ASC`

// enumerateHashesQuery is the GC mark live-set query. It UNIONs the CAS index
// (file_blocks.hash, VARCHAR hex) with the per-file manifest
// (file_block_refs.hash, BYTEA → encode hex) so the live set is a strict
// SUPERSET of both structures, so a hash present in only one (e.g. a manifest
// row whose CAS index row was never written or already reaped) still keeps its
// chunk live. The manifest arm is filtered to nlink>0 inodes (#1433): once a
// file is unlinked its manifest rows linger but the payload is dead, so
// including them would pin orphaned chunks live forever and the sweep could
// never reclaim them. Snapshot-held blocks are protected independently by the
// GC HoldProvider (on-disk snapshot manifests), not by this union. NULL
// hashes (legacy pre-CAS file_blocks rows) are emitted as the zero ContentHash
// and skipped by the mark phase; file_block_refs.hash is NOT NULL.
//
// UNION ALL, not UNION: the consumer (GC mark phase) dedupes hashes into a set,
// so cross-source and intra-source duplicates are harmless. UNION would force an
// expensive sort/hash-aggregate to dedupe at the query layer for no benefit.
const enumerateHashesQuery = `SELECT hash FROM file_blocks
UNION ALL
SELECT lower(hex(fbr.hash)) FROM file_block_refs fbr
JOIN inodes i ON fbr.file_id = i.id
WHERE i.nlink > 0`

// EnumeratePayloads streams every distinct payloadID that has at least one
// FileChunk row through fn. FileChunk row IDs have the form
// {payloadID}/{chunkOffset}; the payloadID is everything BEFORE THE LAST '/'
// (payloadIDs are BuildPayloadID(shareName, filePath) and themselves contain
// slashes, so a substr/split on the FIRST slash would truncate every
// subdirectory file to its share name). We therefore parse the payloadID in
// Go on the last slash rather than in SQL.
//
// The rows cursor is fully drained and CLOSED before any fn callback runs.
// The SQLite pool is MaxOpenConns(1), and warm/stats fn callbacks issue
// further reads (ListFileChunks) that need that single connection — calling fn
// while the cursor is still open would deadlock. This collect-then-call shape
// also matches the badger/memory backends.
func (s *SQLiteMetadataStore) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
	const query = `SELECT DISTINCT id FROM file_blocks`
	ids, err := s.collectFileChunkIDs(ctx, query)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate payloads: %w", err)
		}
		i := strings.LastIndex(id, "/")
		if i < 0 {
			continue
		}
		payloadID := id[:i]
		if _, ok := seen[payloadID]; ok {
			continue
		}
		seen[payloadID] = struct{}{}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// EnumerateLivePayloadIDs streams every distinct content_id referenced by a
// live inode. content_id IS the payloadID, so no id-splitting is needed.
// Hardlinks share one inode row, so DISTINCT yields one payloadID regardless of
// link count. nlink=0 (unlinked) inodes are excluded (#1433): their payload is
// dead, so the reconcile must treat it as stranded, not live.
func (s *SQLiteMetadataStore) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
	const query = `SELECT DISTINCT content_id FROM inodes WHERE content_id IS NOT NULL AND content_id != '' AND nlink > 0`
	ids, err := s.collectFileChunkIDs(ctx, query)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate live payloads: %w", err)
		}
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

// collectFileChunkIDs runs query (which must SELECT a single TEXT id column),
// scans every id into a slice, and closes the rows cursor before returning so
// the caller may safely issue further queries on the single-connection pool.
func (s *SQLiteMetadataStore) collectFileChunkIDs(ctx context.Context, query string) ([]string, error) {
	rows, err := s.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("enumerate payloads: query: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("enumerate payloads: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enumerate payloads: rows: %w", err)
	}
	return ids, nil
}

// ============================================================================
// Scan Helpers
// ============================================================================

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure sqliteTransaction implements FileChunkStore
var _ block.FileChunkStore = (*sqliteTransaction)(nil)

// Every FileChunkStore method below runs on the transaction's own executor,
// never the store's pool helpers: a pool connection cannot see the
// transaction's uncommitted writes and would survive its rollback, so a caller
// that bumped RefCount inside WithTransaction and then hit a downstream
// UpdateAttrs failure would leak the bump.

// AddRef bumps ref_count keyed by hash on the active transaction so a
// subsequent rollback undoes it. Returns metadata.ErrUnknownHash when no row
// matches.
func (tx *sqliteTransaction) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// postgres backend records ref count only — parameters intentionally
	// blanked.
	return tx.Core.AddRef(ctx, hash)
}

// ListFileChunks / EnumerateFileChunks run on the active
// transaction (tx.tx), NOT the pool. Delegating to the pool opens a separate
// connection that cannot see this transaction's uncommitted writes, so a Put
// followed by a List in the same WithTransaction would miss the pending row
// (read-after-write violation; the SQL is otherwise identical to the
// store-level methods).

// The file_blocks table schema lives in
// pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql.

// InjectCorruptHashRow stores a file_blocks row whose hash column holds a
// syntactically malformed value. Test-only: implements the storetest
// CorruptHashInjector capability so the conformance suite can exercise
// fail-closed enumeration. The TEXT column lets us bypass the
// Put contract that always serializes a valid ContentHash.String().
func (s *SQLiteMetadataStore) InjectCorruptHashRow(ctx context.Context, blockID string, badHash string) error {
	now := time.Now()
	_, err := s.exec(ctx, insertFileChunk+` ON CONFLICT (id) DO UPDATE SET hash = EXCLUDED.hash`,
		blockID, badHash, uint32(64), uint32(0), uint32(1), now, now, int(block.BlockStateRemote), nil,
	)
	if err != nil {
		return fmt.Errorf("inject corrupt hash row: %w", err)
	}
	return nil
}

// decrementAndReapManyTx applies the -1 UPDATE and the reap-at-zero DELETE to a
// whole id set, two statements per batch instead of two per id. The
// `ref_count = 0` predicate on the DELETE means a bump that landed between the
// two statements leaves that row alive, and an id with no row is a no-op — the
// same outcomes decrementAndReapTx produces one row at a time. SQLite caps how
// many parameters one statement may bind, so a large set runs as several
// batches.
func decrementAndReapManyTx(ctx context.Context, tx execer, ids []string) error {
	const maxIDsPerStatement = 500
	for len(ids) > 0 {
		batch := ids[:min(len(ids), maxIDsPerStatement)]
		ids = ids[len(batch):]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		in := " WHERE id IN (?" + strings.Repeat(",?", len(batch)-1) + ")"

		if _, err := tx.Exec(ctx,
			`UPDATE file_blocks SET ref_count = MAX(ref_count - 1, 0)`+in, args...); err != nil {
			return fmt.Errorf("decrement ref count: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM file_blocks`+in+` AND ref_count = 0`, args...); err != nil {
			return fmt.Errorf("reap zero-ref block: %w", err)
		}
	}
	return nil
}

// DecrementRefCountAndReapMany runs the batched decrement + reap on the active
// transaction so a subsequent rollback undoes the whole set.
func (tx *sqliteTransaction) DecrementRefCountAndReapMany(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return decrementAndReapManyTx(ctx, tx.tx, ids)
}
