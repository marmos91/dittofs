package sql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// objects carries the file_blocks surface that is more than a single-row read
// or write: the payload enumerations the GC mark and the size reconcile walk,
// the batched decrement-and-reap a truncate or unlink drives, and the test-only
// corrupt-row injector.
//
// The plain CRUD half lives in chunks.go. Schema lives in
// migrations/000010_file_blocks.up.sql.

// FileChunkColumns is the file_blocks column list every chunk read and write
// names, in the order ScanFileChunk reads them. Naming it once is what keeps a
// column added to one statement from being missed by the others, or by the
// scan. Both dialects spell the list identically, so it sits here rather than
// in a query struct.
const FileChunkColumns = `id, hash, data_size, start_offset, ref_count, last_access, created_at, state, last_sync_attempt_at`

// FileChunkUpsertTail is the conflict clause each dialect appends to its own
// insert to build ChunkQueries.Upsert. Only the insert's placeholders differ,
// so the clause itself is spelled once here.
//
// It omits ref_count from the update list so a concurrent IncrementRefCount /
// DecrementRefCount (which run as atomic SQL `+1` / `-1` UPDATEs) cannot be
// silently overwritten by a Put carrying a stale in-memory RefCount. The INSERT
// path still writes the caller's value verbatim, which is the contract for a new
// row; for an existing row RefCount mutates exclusively through
// Increment/Decrement. hash uses COALESCE so a zero-hash Put never NULLs a
// previously-persisted good hash.
const FileChunkUpsertTail = `
	ON CONFLICT (id) DO UPDATE SET
		hash = COALESCE(EXCLUDED.hash, file_blocks.hash),
		data_size = EXCLUDED.data_size,
		start_offset = EXCLUDED.start_offset,
		last_access = EXCLUDED.last_access,
		state = EXCLUDED.state,
		last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`

// EnumeratePayloads streams every distinct payloadID that has at least one
// FileChunk row through fn. FileChunk row IDs have the form
// {payloadID}/{chunkOffset}; the payloadID is everything BEFORE THE LAST '/'
// (payloadIDs are BuildPayloadID(shareName, filePath) and themselves contain
// slashes, so splitting on the FIRST slash would truncate every subdirectory
// file to its share name). We therefore parse the payloadID in Go on the last
// slash rather than in SQL.
//
// The rows cursor is fully drained and CLOSED before any fn callback runs.
// The sqlite pool is MaxOpenConns(1) and the warm/stats callbacks issue further
// reads (ListFileChunks) that need that single connection, so calling fn with
// the cursor still open would deadlock. This collect-then-call shape also
// matches the badger and memory backends.
func (c *Core) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
	const query = `SELECT DISTINCT id FROM file_blocks`
	ids, err := c.collectIDs(ctx, query)
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
// link count. nlink=0 (unlinked) inodes are excluded: their payload is dead, so
// the reconcile must treat it as stranded, not live.
func (c *Core) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
	const query = `SELECT DISTINCT content_id FROM inodes WHERE content_id IS NOT NULL AND content_id != '' AND nlink > 0`
	ids, err := c.collectIDs(ctx, query)
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

// collectIDs runs query (which must SELECT a single TEXT id column), scans
// every id into a slice, and closes the rows cursor before returning so the
// caller may safely issue further queries on the single-connection pool. It
// serves both the file_blocks and the inodes enumeration.
func (c *Core) collectIDs(ctx context.Context, query string) ([]string, error) {
	rows, err := c.X.Query(ctx, query)
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

// DecrementAndReapMany applies the -1 UPDATE and the reap-at-zero DELETE to a
// whole id set, two statements per batch instead of two per id. The
// `ref_count = 0` predicate on the DELETE means a bump that landed between the
// two statements leaves that row alive, and an id with no row is a no-op — the
// same outcomes Core.DecrementRefCountAndReap produces one row at a time. An
// empty set runs no statement at all.
//
// It takes its executor rather than riding on Core because the pair only has
// its meaning together: a method on Core would be promoted onto the pool-backed
// store as well, where the UPDATE and the DELETE autocommit separately and a
// rollback can no longer undo the decrement. Passing the executor keeps the
// caller's transaction visible at the call site.
//
// Callers must supply distinct ids, which is what metadata.FileChunkStore
// documents: a repeated id splits across two batches decrements its row twice.
func DecrementAndReapMany(ctx context.Context, x Executor, d Dialect, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += maxBoundParams {
		batch := ids[start:min(start+maxBoundParams, len(ids))]
		in, args := fileChunkIDInList(d, batch)
		if _, err := x.Exec(ctx, d.Chunks().DecrementRefMany+in, args...); err != nil {
			return fmt.Errorf("decrement ref count: %w", err)
		}
		// The DELETE needs no dialect entry: it carries no placeholder of its
		// own, so both spell it identically.
		if _, err := x.Exec(ctx, `DELETE FROM file_blocks`+in+` AND ref_count = 0`, args...); err != nil {
			return fmt.Errorf("reap zero-ref block: %w", err)
		}
	}
	return nil
}

// fileChunkIDInList renders ` WHERE id IN (?, ...)` for one batch of ids
// together with the arguments to bind. The decrement and the reap share both,
// so the two statements resolve the same set. One id binds one parameter and
// nothing else does, so a batch of maxBoundParams ids is the ceiling.
func fileChunkIDInList(d Dialect, batch []string) (string, []any) {
	var sb strings.Builder
	sb.WriteString(` WHERE id IN (`)
	args := make([]any, 0, len(batch))
	for i, id := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(d.Placeholder(i + 1))
		args = append(args, id)
	}
	sb.WriteByte(')')
	return sb.String(), args
}

// InjectCorruptHashRow stores a file_blocks row whose hash column holds a
// syntactically malformed value. Test-only: it implements the storetest
// CorruptHashInjector capability so the conformance suite can exercise
// fail-closed enumeration. The TEXT column lets us bypass the Put contract that
// always serializes a valid ContentHash.String().
func (c *Core) InjectCorruptHashRow(ctx context.Context, blockID string, badHash string) error {
	now := time.Now()
	_, err := c.X.Exec(ctx, c.D.Chunks().Insert+` ON CONFLICT (id) DO UPDATE SET hash = EXCLUDED.hash`,
		blockID, badHash, uint32(64), uint32(0), uint32(1), now, now, int(block.BlockStateRemote), nil,
	)
	if err != nil {
		return fmt.Errorf("inject corrupt hash row: %w", err)
	}
	return nil
}
